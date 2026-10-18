package executor

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/mechrevo/safeguard-agent/internal/llm"
	"github.com/mechrevo/safeguard-agent/internal/tools"
)

// ---------------------------------------------------------------------------
// 测试替身
// ---------------------------------------------------------------------------

// failingTool 前 failTimes 次调用返回 err，之后成功。
type failingTool struct {
	spec      tools.Spec
	failTimes int
	err       error
	block     bool // 阻塞到 ctx 结束（用来测超时）

	calls int
}

func (t *failingTool) Spec() tools.Spec { return t.spec }

func (t *failingTool) Validate(json.RawMessage) error { return nil }

func (t *failingTool) Invoke(ctx context.Context, _ json.RawMessage) (tools.Result, error) {
	t.calls++
	if t.block {
		<-ctx.Done()
		return tools.Result{}, ctx.Err()
	}
	if t.calls <= t.failTimes {
		return tools.Result{}, t.err
	}
	return tools.Result{Data: map[string]any{"ok": true}, Summary: "成功"}, nil
}

func testSpec(name string) tools.Spec {
	return tools.Spec{
		Name:        name,
		Description: "测试用工具，描述长度需要满足注册门禁的最低要求。",
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
		Tier:        tools.TierHarmless,
		Timeout:     time.Second,
		MaxRetry:    2,
		Idempotent:  true,
	}
}

// retryableErr 是"下游不可用"的典型错误：网络层失败。
func retryableErr() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// newTestExecutor 构造带假时钟与假 sleep 的执行器：退避等待只推进时钟，不真的睡。
func newTestExecutor(clock *fakeClock, cfg Config) (*Executor, *[]time.Duration) {
	sleeps := []time.Duration{}
	cfg.now = clock.now
	cfg.sleep = func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		sleeps = append(sleeps, d)
		clock.advance(d)
		return nil
	}
	return New(cfg), &sleeps
}

func zeroJitterConfig() Config {
	cfg := DefaultConfig()
	cfg.Retry.Jitter = func(time.Duration) time.Duration { return 0 }
	return cfg
}

// ---------------------------------------------------------------------------
// 异常分类
// ---------------------------------------------------------------------------

func TestClassify(t *testing.T) {
	apiErr := func(status int, err error) *llm.APIError {
		return &llm.APIError{Provider: "test", Status: status, Message: "x", Err: err}
	}

	cases := []struct {
		name string
		err  error
		want ErrClass
	}{
		{"成功", nil, ClassOK},
		{"参数错误", &tools.ArgError{Msg: "metric 非法"}, ClassBadInput},
		{"查不到（不应到达这里，但归类为坏输入）", &tools.NotFoundError{Kind: "service", Name: "x"}, ClassBadInput},
		{"调用方取消不可重试", context.Canceled, ClassFatal},
		{"超时可重试", context.DeadlineExceeded, ClassRetryable},
		{"429 可重试", apiErr(429, nil), ClassRetryable},
		{"503 可重试", apiErr(503, nil), ClassRetryable},
		{"401 致命", apiErr(401, nil), ClassFatal},
		{"403 致命", apiErr(403, nil), ClassFatal},
		{"400 坏输入", apiErr(400, nil), ClassBadInput},
		{"422 坏输入", apiErr(422, nil), ClassBadInput},
		{"状态码 0 的网络错误可重试", apiErr(0, errors.New("dial failed")), ClassRetryable},
		{"状态码 0 但底层被取消则不可重试", apiErr(0, context.Canceled), ClassFatal},
		{"连接已坏可重试", driver.ErrBadConn, ClassRetryable},
		{"MySQL 连接失效可重试", mysql.ErrInvalidConn, ClassRetryable},
		{"网络层失败可重试", retryableErr(), ClassRetryable},
		{"无法归类的错误当致命", errors.New("something odd"), ClassFatal},
		{"熔断打开", &CircuitOpenError{Tool: "t"}, ClassCircuitOpen},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.err); got != tc.want {
				t.Errorf("Classify(%v) = %s, 期望 %s", tc.err, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 退避策略
// ---------------------------------------------------------------------------

func TestRetryDelayCurve(t *testing.T) {
	p := RetryPolicy{BaseDelay: 100 * time.Millisecond, MaxDelay: 2 * time.Second, Jitter: func(time.Duration) time.Duration { return 0 }}
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond}
	for i, w := range want {
		if got := p.delay(i + 1); got != w {
			t.Errorf("delay(%d) = %s, 期望 %s", i+1, got, w)
		}
	}

	// 指数增长要被 MaxDelay 截断
	big := RetryPolicy{BaseDelay: time.Second, MaxDelay: 2 * time.Second, Jitter: func(time.Duration) time.Duration { return 0 }}
	if got := big.delay(6); got != 2*time.Second {
		t.Errorf("delay(6) = %s, 期望被 MaxDelay 截到 2s", got)
	}

	// 抖动上限是基础延迟的一半，避免抖动主导退避
	withJitter := RetryPolicy{
		BaseDelay: 100 * time.Millisecond,
		MaxDelay:  2 * time.Second,
		Jitter:    func(max time.Duration) time.Duration { return max },
	}
	if got := withJitter.delay(1); got != 150*time.Millisecond {
		t.Errorf("delay(1) = %s, 期望 100ms + 50ms 抖动", got)
	}
}

// ---------------------------------------------------------------------------
// 熔断器
// ---------------------------------------------------------------------------

func TestBreakerOpensHalfOpensAndCloses(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	cfg := BreakerConfig{Window: time.Minute, Threshold: 3, OpenFor: 30 * time.Second}
	b := newBreaker("query_metrics", cfg, clock.now)

	// 关闭状态：放行
	for i := 0; i < 2; i++ {
		if err := b.Allow(); err != nil {
			t.Fatalf("第 %d 次放行失败: %v", i+1, err)
		}
		b.OnRetryableFailure()
	}
	if got := b.State(); got != "closed" {
		t.Errorf("失败 2 次（阈值 3）状态 = %s, 期望 closed", got)
	}

	// 第 3 次失败 → 打开
	if err := b.Allow(); err != nil {
		t.Fatalf("放行失败: %v", err)
	}
	b.OnRetryableFailure()
	if got := b.State(); got != "open" {
		t.Fatalf("状态 = %s, 期望 open", got)
	}

	// 打开期间快速失败，且错误里要能看出还要等多久
	err := b.Allow()
	if err == nil {
		t.Fatal("打开状态必须拒绝")
	}
	openErr, ok := IsCircuitOpen(err)
	if !ok {
		t.Fatalf("期望 *CircuitOpenError, 实际 %T", err)
	}
	if openErr.RetryAfter <= 0 {
		t.Errorf("RetryAfter 应为正数，实际 %s", openErr.RetryAfter)
	}
	if openErr.Failures < 3 {
		t.Errorf("Failures = %d, 期望 >= 3", openErr.Failures)
	}

	// 过了 OpenFor → 半开，只放一个试探
	clock.advance(31 * time.Second)
	if err := b.Allow(); err != nil {
		t.Fatalf("半开应放行试探请求: %v", err)
	}
	if err := b.Allow(); err == nil {
		t.Fatal("半开期间已有试探在飞，第二个请求应被拒绝")
	}

	// 试探成功 → 关闭
	b.OnSuccess()
	if got := b.State(); got != "closed" {
		t.Errorf("试探成功后状态 = %s, 期望 closed", got)
	}
	if err := b.Allow(); err != nil {
		t.Errorf("关闭后应放行: %v", err)
	}
}

func TestBreakerHalfOpenFailureReopens(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	b := newBreaker("t", BreakerConfig{Window: time.Minute, Threshold: 1, OpenFor: 30 * time.Second}, clock.now)

	_ = b.Allow()
	b.OnRetryableFailure()
	if got := b.State(); got != "open" {
		t.Fatalf("状态 = %s, 期望 open", got)
	}

	clock.advance(31 * time.Second)
	if err := b.Allow(); err != nil {
		t.Fatalf("半开应放行: %v", err)
	}
	b.OnRetryableFailure() // 试探失败
	if got := b.State(); got != "open" {
		t.Errorf("试探失败后状态 = %s, 期望重新打开", got)
	}
	// 重新打开后要重新计时
	if err := b.Allow(); err == nil {
		t.Error("重新打开后应立即拒绝")
	}
}

func TestBreakerFailureWindowExpires(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	b := newBreaker("t", BreakerConfig{Window: time.Minute, Threshold: 3, OpenFor: 30 * time.Second}, clock.now)

	_ = b.Allow()
	b.OnRetryableFailure()
	_ = b.Allow()
	b.OnRetryableFailure()

	// 超过窗口，旧的两次失败应当失效
	clock.advance(70 * time.Second)
	_ = b.Allow()
	b.OnRetryableFailure()

	if got := b.State(); got != "closed" {
		t.Errorf("窗口外的失败不应计入，状态 = %s, 期望 closed", got)
	}
}

// 这条钉住一个容易做错的地方：只有"可重试失败"才计入熔断。
// 否则模型反复发错参数，就会把工具熔断掉——把模型的错误算到下游头上。
func TestBreakerIgnoresNeutralResults(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	b := newBreaker("t", BreakerConfig{Window: time.Minute, Threshold: 2, OpenFor: 30 * time.Second}, clock.now)

	for i := 0; i < 10; i++ {
		if err := b.Allow(); err != nil {
			t.Fatalf("参数错误不应导致熔断，第 %d 次被拒: %v", i+1, err)
		}
		b.OnNeutral()
	}
	if got := b.State(); got != "closed" {
		t.Errorf("状态 = %s, 期望 closed", got)
	}
}

// 半开时如果结果是"中性"的（例如参数错误），试探名额必须还回去，
// 否则这个工具会被永久卡死。
func TestBreakerNeutralReleasesHalfOpenProbe(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	b := newBreaker("t", BreakerConfig{Window: time.Minute, Threshold: 1, OpenFor: 30 * time.Second}, clock.now)

	_ = b.Allow()
	b.OnRetryableFailure()
	clock.advance(31 * time.Second)

	if err := b.Allow(); err != nil {
		t.Fatalf("半开应放行: %v", err)
	}
	b.OnNeutral() // 结果与下游健康度无关
	if err := b.Allow(); err != nil {
		t.Errorf("中性结果后试探名额应被释放，实际被拒: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 执行器
// ---------------------------------------------------------------------------

func TestExecuteRetriesRetryableThenSucceeds(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	exec, sleeps := newTestExecutor(clock, zeroJitterConfig())

	tool := &failingTool{spec: testSpec("query_metrics"), failTimes: 2, err: retryableErr()}
	out := exec.Execute(context.Background(), tool, json.RawMessage(`{}`))

	if !out.OK() {
		t.Fatalf("重试后应成功，实际: %v", out.Error)
	}
	if tool.calls != 3 {
		t.Errorf("实际调用 %d 次, 期望 3（2 次失败 + 1 次成功）", tool.calls)
	}
	if len(out.Attempts) != 3 || !out.Retried() {
		t.Errorf("Attempts = %d, Retried = %v", len(out.Attempts), out.Retried())
	}
	// 退避曲线：100ms 然后 200ms（MaxRetry=2 允许三次尝试）
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond}
	if len(*sleeps) != len(want) {
		t.Fatalf("退避次数 = %d, 期望 %d: %v", len(*sleeps), len(want), *sleeps)
	}
	for i := range want {
		if (*sleeps)[i] != want[i] {
			t.Errorf("第 %d 次退避 = %s, 期望 %s", i+1, (*sleeps)[i], want[i])
		}
	}
	if got := exec.Breakers().States()["query_metrics"]; got != "closed" {
		t.Errorf("最终成功应让熔断保持 closed，实际 %s", got)
	}
}

func TestExecuteGivesUpAfterMaxRetry(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	exec, _ := newTestExecutor(clock, zeroJitterConfig())

	tool := &failingTool{spec: testSpec("query_metrics"), failTimes: 99, err: retryableErr()}
	out := exec.Execute(context.Background(), tool, json.RawMessage(`{}`))

	if out.OK() {
		t.Fatal("应当失败")
	}
	if out.Class != ClassRetryable {
		t.Errorf("Class = %s, 期望 retryable", out.Class)
	}
	// MaxRetry=2 → 最多 3 次尝试
	if tool.calls != 3 {
		t.Errorf("实际调用 %d 次, 期望 3", tool.calls)
	}
}

func TestExecuteDoesNotRetryBadInput(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	exec, sleeps := newTestExecutor(clock, zeroJitterConfig())

	tool := &failingTool{spec: testSpec("query_metrics"), failTimes: 99, err: &tools.ArgError{Msg: "metric 非法"}}
	out := exec.Execute(context.Background(), tool, json.RawMessage(`{}`))

	if out.Class != ClassBadInput {
		t.Errorf("Class = %s, 期望 bad_input", out.Class)
	}
	if tool.calls != 1 {
		t.Errorf("参数错误不该重试，实际调用 %d 次", tool.calls)
	}
	if len(*sleeps) != 0 {
		t.Errorf("参数错误不该退避，实际等待 %v", *sleeps)
	}
}

// 非幂等工具即便配了 MaxRetry 也只试一次——重试就是在重复产生副作用。
func TestExecuteNeverRetriesNonIdempotentTool(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	exec, _ := newTestExecutor(clock, zeroJitterConfig())

	spec := testSpec("restart_service")
	spec.Idempotent = false
	spec.MaxRetry = 3

	tool := &failingTool{spec: spec, failTimes: 99, err: retryableErr()}
	out := exec.Execute(context.Background(), tool, json.RawMessage(`{}`))

	if tool.calls != 1 {
		t.Errorf("非幂等工具只能调一次，实际 %d 次", tool.calls)
	}
	if out.Class != ClassRetryable {
		t.Errorf("Class = %s, 期望 retryable", out.Class)
	}
}

func TestExecuteCircuitOpenFastFailsWithoutCallingDownstream(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	cfg := zeroJitterConfig()
	cfg.Breakers = BreakerConfig{Window: time.Minute, Threshold: 5, OpenFor: 30 * time.Second}
	exec, _ := newTestExecutor(clock, cfg)

	spec := testSpec("query_metrics")
	spec.MaxRetry = 0 // 只看熔断，不让重试混淆计数
	tool := &failingTool{spec: spec, failTimes: 999, err: retryableErr()}

	// 5 次可重试失败 → 熔断打开
	for i := 0; i < 5; i++ {
		out := exec.Execute(context.Background(), tool, json.RawMessage(`{}`))
		if out.Class != ClassRetryable {
			t.Fatalf("第 %d 次 Class = %s, 期望 retryable", i+1, out.Class)
		}
	}
	if got := exec.Breakers().States()["query_metrics"]; got != "open" {
		t.Fatalf("状态 = %s, 期望 open", got)
	}

	callsBefore := tool.calls
	out := exec.Execute(context.Background(), tool, json.RawMessage(`{}`))
	if out.Class != ClassCircuitOpen {
		t.Errorf("Class = %s, 期望 circuit_open", out.Class)
	}
	if tool.calls != callsBefore {
		t.Errorf("熔断期间不该真的打下游，调用次数从 %d 变成了 %d", callsBefore, tool.calls)
	}
	if _, ok := IsCircuitOpen(out.Error); !ok {
		t.Errorf("错误类型应是 *CircuitOpenError，实际 %T", out.Error)
	}
}

func TestExecuteTimeoutIsClassifiedRetryable(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	exec, _ := newTestExecutor(clock, zeroJitterConfig())

	spec := testSpec("slow_tool")
	spec.Timeout = 5 * time.Millisecond
	spec.MaxRetry = 0
	tool := &failingTool{spec: spec, block: true}

	out := exec.Execute(context.Background(), tool, json.RawMessage(`{}`))
	if out.Class != ClassRetryable {
		t.Errorf("超时应归类为 retryable，实际 %s (%v)", out.Class, out.Error)
	}
}

func TestExecuteStopsRetryingWhenContextCanceled(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	cfg := zeroJitterConfig()
	cfg.now = clock.now
	// 模拟"退避等待期间任务被取消"
	cfg.sleep = func(context.Context, time.Duration) error { return context.Canceled }
	exec := New(cfg)

	tool := &failingTool{spec: testSpec("query_metrics"), failTimes: 99, err: retryableErr()}
	out := exec.Execute(context.Background(), tool, json.RawMessage(`{}`))

	if tool.calls != 1 {
		t.Errorf("退避被取消后不应再试，实际调用 %d 次", tool.calls)
	}
	if out.Class != ClassFatal {
		t.Errorf("取消应归类为 fatal，实际 %s", out.Class)
	}
}

// TestBreakerSetStatesPerTool 验证两件事：
//  1. 熔断按工具隔离（一个工具打开，另一个不受影响）；
//  2. 状态可以通过 States() 读出来——这是 CLI 打印与将来接指标上报的入口。
func TestBreakerSetStatesPerTool(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	set := newBreakerSet(BreakerConfig{Window: time.Minute, Threshold: 3, OpenFor: 30 * time.Second}, clock)

	// 让 query_metrics 打开
	breaker := set.For("query_metrics")
	for i := 0; i < 3; i++ {
		now = now.Add(time.Second)
		if err := breaker.Allow(); err != nil {
			t.Fatalf("第 %d 次 Allow 被拒: %v", i+1, err)
		}
		breaker.OnRetryableFailure()
	}

	// 另一个工具完全不受影响
	other := set.For("query_config")
	if err := other.Allow(); err != nil {
		t.Errorf("query_config 不该被连累: %v", err)
	}

	states := set.States()
	if states["query_metrics"] != "open" {
		t.Errorf("query_metrics 状态 = %q, 期望 open", states["query_metrics"])
	}
	if states["query_config"] != "closed" {
		t.Errorf("query_config 状态 = %q, 期望 closed", states["query_config"])
	}

	// 打开期间 Allow 快速失败，且错误里带剩余等待时间
	if err := breaker.Allow(); err == nil {
		t.Fatal("打开期间应当拒绝")
	} else if _, ok := IsCircuitOpen(err); !ok {
		t.Fatalf("应当是 CircuitOpenError，实际 %T", err)
	}
}
