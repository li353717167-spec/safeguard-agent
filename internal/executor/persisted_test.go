package executor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mechrevo/safeguard-agent/internal/store"
	"github.com/mechrevo/safeguard-agent/internal/tools"
)

// memToolCallStore 是内存版 ToolCallStore：模拟 tool_calls 表的主键与三段式写入。
type memToolCallStore struct {
	rows        map[string]*store.ToolCall
	recordCalls int
	failRecord  bool
}

func newMemStore() *memToolCallStore {
	return &memToolCallStore{rows: map[string]*store.ToolCall{}}
}

func (m *memToolCallStore) BeginIntent(_ context.Context, tc *store.ToolCall) (*store.ToolCall, bool, error) {
	if existing, ok := m.rows[tc.IdemKey]; ok {
		return existing, true, nil
	}
	copied := *tc
	m.rows[tc.IdemKey] = &copied
	return nil, false, nil
}

func (m *memToolCallStore) RecordResult(_ context.Context, idemKey string, res store.ToolCallResult) error {
	if m.failRecord {
		return errors.New("模拟：数据库写失败")
	}
	row, ok := m.rows[idemKey]
	if !ok {
		return errors.New("模拟：意图记录缺失")
	}
	m.recordCalls++
	row.Status = res.Status
	row.Result = res.Result
	row.ErrClass = res.ErrClass
	row.ErrMessage = res.ErrMessage
	row.RetryCount = res.RetryCount
	row.FinishedAt = &res.FinishedAt
	return nil
}

// countingTool 统计下游被真实调用的次数。
type countingTool struct {
	spec tools.Spec
	err  error
	// failTimes 次失败（用 err），之后成功
	failTimes int
	calls     int
}

func (c *countingTool) Spec() tools.Spec { return c.spec }

func (c *countingTool) Validate(json.RawMessage) error { return nil }

func (c *countingTool) Invoke(context.Context, json.RawMessage) (tools.Result, error) {
	c.calls++
	if c.calls <= c.failTimes {
		return tools.Result{}, c.err
	}
	return tools.Result{Data: map[string]any{"found": true, "latest": 0.1}, Summary: "查到 0.1%"}, nil
}

func persistedTestExecutor(cfg Config, st ToolCallStore) (*PersistedExecutor, *Executor) {
	exec := New(cfg)
	return NewPersisted(exec, st), exec
}

func fastConfig() Config {
	return Config{
		Breakers: BreakerConfig{Window: time.Minute, Threshold: 5, OpenFor: 30 * time.Second},
		Retry: RetryPolicy{
			BaseDelay: time.Millisecond,
			MaxDelay:  5 * time.Millisecond,
			Jitter:    func(time.Duration) time.Duration { return 0 },
		},
	}
}

func testRef(stepNo int) CallRef {
	return CallRef{TaskID: testTask, StepNo: stepNo, CallIndex: 1}
}

// 三段式的第一、三段：意图先落库，结果后回写。
func TestPersistedWritesIntentThenResult(t *testing.T) {
	mem := newMemStore()
	pe, _ := persistedTestExecutor(fastConfig(), mem)
	tool := &countingTool{spec: testSpec(testTool)}

	out := pe.Run(context.Background(), tool, json.RawMessage(testArgs), testRef(1))
	if !out.OK() {
		t.Fatalf("执行应成功: %v", out.Error)
	}
	if out.IdemKey == "" || len(out.IdemKey) != idemKeyLen {
		t.Errorf("IdemKey = %q, 期望 %d 位", out.IdemKey, idemKeyLen)
	}
	if out.Replayed {
		t.Error("首次执行不应标记为回放")
	}
	if tool.calls != 1 {
		t.Errorf("下游被调用 %d 次, 期望 1", tool.calls)
	}

	row, ok := mem.rows[out.IdemKey]
	if !ok {
		t.Fatal("意图记录不存在")
	}
	if row.Status != store.ToolCallStatusSuccess {
		t.Errorf("状态 = %s, 期望 success", row.Status)
	}
	if row.Result == nil || !strings.Contains(*row.Result, `"found":true`) {
		t.Errorf("结果未回写: %v", row.Result)
	}
	if row.Args != `{"metric":"error_rate","service":"payment-service","window":"5m"}` {
		t.Errorf("落库的是规范化参数，实际: %s", row.Args)
	}
	if row.Tier != int8(tools.TierHarmless) {
		t.Errorf("tier = %d", row.Tier)
	}
	if row.FinishedAt == nil {
		t.Error("finished_at 未写")
	}
	if mem.recordCalls != 1 {
		t.Errorf("RecordResult 调用 %d 次, 期望 1", mem.recordCalls)
	}
}

// 这条就是 D5 的验收：**同一个逻辑操作执行两次，下游只被真实执行一次**。
func TestPersistedReplaySkipsDownstreamExecution(t *testing.T) {
	mem := newMemStore()
	pe, _ := persistedTestExecutor(fastConfig(), mem)
	tool := &countingTool{spec: testSpec(testTool)}

	first := pe.Run(context.Background(), tool, json.RawMessage(testArgs), testRef(1))
	second := pe.Run(context.Background(), tool, json.RawMessage(testArgs), testRef(1))

	if tool.calls != 1 {
		t.Errorf("下游被调用 %d 次, 期望 1（第二次应当回放）", tool.calls)
	}
	if !second.OK() || !second.Replayed {
		t.Errorf("第二次应回放成功，实际 ok=%v replayed=%v err=%v", second.OK(), second.Replayed, second.Error)
	}
	if second.IdemKey != first.IdemKey {
		t.Errorf("两次 key 不同：%s != %s", second.IdemKey, first.IdemKey)
	}
	if !strings.Contains(second.Result.Summary, "回放") {
		t.Errorf("回放的摘要应说明是回放: %q", second.Result.Summary)
	}
	data, ok := second.Result.Data.(map[string]any)
	if !ok || data["found"] != true {
		t.Errorf("回放的结果应从库里解回来: %#v", second.Result.Data)
	}
	if len(mem.rows) != 1 {
		t.Errorf("库里应只有 1 行，实际 %d", len(mem.rows))
	}
}

// 参数写法不同（键顺序）也要命中同一个 key —— 否则"重试"会变成"重复执行"。
func TestPersistedReplayMatchesReorderedArgs(t *testing.T) {
	mem := newMemStore()
	pe, _ := persistedTestExecutor(fastConfig(), mem)
	tool := &countingTool{spec: testSpec(testTool)}

	pe.Run(context.Background(), tool, json.RawMessage(testArgs), testRef(1))
	second := pe.Run(context.Background(), tool, json.RawMessage(testArgs2), testRef(1))

	if tool.calls != 1 {
		t.Errorf("参数键顺序不同不应导致重复执行，实际调用 %d 次", tool.calls)
	}
	if !second.Replayed {
		t.Error("应当命中回放")
	}
}

// 这是洞 1 最难的那一格：**执行成功了，但结果没回写**（进程死在第 ① 与 ③ 段之间）。
// 恢复时看到 status=started 且没有 finished_at —— 用同一个 key 再执行一次是安全的。
func TestPersistedResumesHangingIntent(t *testing.T) {
	mem := newMemStore()
	pe, _ := persistedTestExecutor(fastConfig(), mem)
	tool := &countingTool{spec: testSpec(testTool)}

	// 先造一条"悬挂意图"：上一次进程只完成了第 ① 段。
	key, err := BuildIdempotencyKey(testRef(1).TaskID, 1, 1, testTool, json.RawMessage(testArgs))
	if err != nil {
		t.Fatalf("生成 key 失败: %v", err)
	}
	mem.rows[key] = &store.ToolCall{
		IdemKey: key, TaskID: testTask, StepNo: 1, CallIndex: 1,
		ToolName: testTool, Args: testArgs, Tier: int8(tools.TierHarmless),
		Status: store.ToolCallStatusStarted, StartedAt: time.Now().UTC().Add(-time.Minute),
	}

	out := pe.Run(context.Background(), tool, json.RawMessage(testArgs), testRef(1))
	if !out.OK() {
		t.Fatalf("悬挂意图应当被安全地重新执行: %v", out.Error)
	}
	if out.Replayed {
		t.Error("悬挂意图没有结果可回放，不应标记为回放")
	}
	if tool.calls != 1 {
		t.Errorf("下游应被调用 1 次（恢复执行），实际 %d", tool.calls)
	}
	if row := mem.rows[key]; row.Status != store.ToolCallStatusSuccess || row.Result == nil {
		t.Errorf("恢复后应补写结果，实际状态 %s / result=%v", row.Status, row.Result)
	}
}

func TestPersistedRecordsFailureWithClass(t *testing.T) {
	mem := newMemStore()
	pe, _ := persistedTestExecutor(fastConfig(), mem)

	spec := testSpec(testTool)
	spec.MaxRetry = 2
	tool := &countingTool{spec: spec, failTimes: 99, err: retryableErr()}

	out := pe.Run(context.Background(), tool, json.RawMessage(testArgs), testRef(1))
	if out.OK() {
		t.Fatal("应当失败")
	}
	row := mem.rows[out.IdemKey]
	if row.Status != store.ToolCallStatusFailed {
		t.Errorf("状态 = %s, 期望 failed", row.Status)
	}
	if row.ErrClass == nil || *row.ErrClass != 0 {
		t.Errorf("err_class = %v, 期望 0（可重试）", row.ErrClass)
	}
	if row.RetryCount != 2 {
		t.Errorf("retry_count = %d, 期望 2", row.RetryCount)
	}
	if row.ErrMessage == nil || !strings.Contains(*row.ErrMessage, "connection refused") {
		t.Errorf("错误信息未回写: %v", row.ErrMessage)
	}
}

// 回写失败必须显形：不能让调用方以为"成功且已记录"。
func TestPersistedSurfacesWriteBackFailure(t *testing.T) {
	mem := newMemStore()
	mem.failRecord = true
	pe, _ := persistedTestExecutor(fastConfig(), mem)
	tool := &countingTool{spec: testSpec(testTool)}

	out := pe.Run(context.Background(), tool, json.RawMessage(testArgs), testRef(1))
	if out.OK() {
		t.Fatal("回写失败时不应报告成功")
	}
	if !strings.Contains(out.Error.Error(), "回写失败") {
		t.Errorf("错误信息应说明回写失败: %v", out.Error)
	}
	if out.Class != ClassFatal {
		t.Errorf("Class = %s, 期望 fatal", out.Class)
	}
	if tool.calls != 1 {
		t.Errorf("下游确实执行了 1 次，实际 %d", tool.calls)
	}
}

// 等待审批的意图不能被直接执行（D6 之前 fail closed）。
func TestPersistedRefusesWaitingApprovalIntent(t *testing.T) {
	mem := newMemStore()
	pe, _ := persistedTestExecutor(fastConfig(), mem)
	tool := &countingTool{spec: testSpec(testTool)}

	key, err := BuildIdempotencyKey(testRef(1).TaskID, 1, 1, testTool, json.RawMessage(testArgs))
	if err != nil {
		t.Fatalf("生成 key 失败: %v", err)
	}
	mem.rows[key] = &store.ToolCall{
		IdemKey: key, TaskID: testTask, StepNo: 1, CallIndex: 1,
		ToolName: testTool, Status: store.ToolCallStatusWaitingApproval, StartedAt: time.Now().UTC(),
	}

	out := pe.Run(context.Background(), tool, json.RawMessage(testArgs), testRef(1))
	if out.OK() {
		t.Fatal("等待审批的调用不应被执行")
	}
	if tool.calls != 0 {
		t.Errorf("下游不应被调用，实际 %d 次", tool.calls)
	}
	if !strings.Contains(out.Error.Error(), "审批") {
		t.Errorf("错误信息应提到审批: %v", out.Error)
	}
}

// 不同步骤 = 不同逻辑操作 → 必须各自执行一次。
func TestPersistedDifferentStepExecutesAgain(t *testing.T) {
	mem := newMemStore()
	pe, _ := persistedTestExecutor(fastConfig(), mem)
	tool := &countingTool{spec: testSpec(testTool)}

	first := pe.Run(context.Background(), tool, json.RawMessage(testArgs), testRef(1))
	second := pe.Run(context.Background(), tool, json.RawMessage(testArgs), testRef(2))

	if tool.calls != 2 {
		t.Errorf("不同步骤应各自执行，实际下游调用 %d 次", tool.calls)
	}
	if first.IdemKey == second.IdemKey {
		t.Error("不同步骤不应算出同一个 key")
	}
	if second.Replayed {
		t.Error("不同步骤不应命中回放")
	}
}

func TestStoreErrClassMapping(t *testing.T) {
	// 对齐 0001_init.sql 的注释：0=可重试 1=致命 2=参数错 3=熔断打开
	cases := map[ErrClass]int8{
		ClassRetryable:   0,
		ClassFatal:       1,
		ClassBadInput:    2,
		ClassCircuitOpen: 3,
		ClassOK:          1, // 成功不写 err_class；这里只验证兜底不炸
	}
	for class, want := range cases {
		if got := toStoreErrClass(class); got != want {
			t.Errorf("toStoreErrClass(%s) = %d, 期望 %d", class, got, want)
		}
	}
}
