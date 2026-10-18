package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/mechrevo/safeguard-agent/internal/store"
	"github.com/mechrevo/safeguard-agent/internal/tools"
)

// 为什么要有这个文件：前面的分类测试用的是手搓的 *net.OpError。
// 手搓的能过，不等于**真实驱动的错误包装**也能被识别——驱动、GORM、database/sql
// 每一层都可能把错误再包一层。这里用一个必然拒绝连接的地址，验证真实链路。
//
// 用法：连到一个必然拒绝的端口（127.0.0.1:1），不依赖任何外部环境，也不需要真实数据库。

func deadDSN() string {
	// tls=false + 极短超时：这里要的不是"能连上"，而是"尽快失败并产生真实的驱动错误"
	return "appuser:x@tcp(127.0.0.1:1)/appdb?timeout=500ms&tls=false&parseTime=true"
}

func deadDataSource(t *testing.T) *tools.MySQLDataSource {
	t.Helper()
	// SkipInitializeWithVersion 很关键：gorm 的 mysql 驱动默认会在 Open 时查
	// `SELECT VERSION()` 探测版本，那样连接失败就发生在 Open，测不到"查询失败"这条真实路径。
	db, err := gorm.Open(gormmysql.New(gormmysql.Config{
		DSN:                       deadDSN(),
		SkipInitializeWithVersion: true,
		DefaultStringSize:         255,
	}), &gorm.Config{
		DisableAutomaticPing: true, // 不在这里连；让查询时真正去连、真正失败
		Logger:               gormlogger.Discard,
	})
	if err != nil {
		t.Fatalf("构造 GORM 句柄失败: %v", err)
	}
	return tools.NewMySQLDataSource(db)
}

func TestRealDriverErrorIsClassifiedRetryable(t *testing.T) {
	ds := deadDataSource(t)

	_, err := ds.QueryMetrics(context.Background(), "payment-service", "error_rate", 5*time.Minute)
	if err == nil {
		t.Fatal("连到死端口应当失败")
	}

	// 这条断言的价值：它证明 Classify 依赖的 *net.OpError 判断在**真实错误包装链**上成立。
	if got := Classify(err); got != ClassRetryable {
		t.Errorf("真实驱动错误被分类为 %s, 期望 retryable；原始错误: %v", got, err)
	}
}

func TestExecutorWithRealToolRetriesThenClassifies(t *testing.T) {
	reg, err := tools.NewDefaultRegistry(deadDataSource(t))
	if err != nil {
		t.Fatalf("构造注册中心失败: %v", err)
	}
	queryMetrics, ok := reg.Get("query_metrics")
	if !ok {
		t.Fatal("找不到 query_metrics")
	}

	// 用毫秒级退避，避免测试真的等几百毫秒。
	exec := New(Config{
		Breakers: BreakerConfig{Window: time.Minute, Threshold: 5, OpenFor: 30 * time.Second},
		Retry: RetryPolicy{
			BaseDelay: time.Millisecond,
			MaxDelay:  5 * time.Millisecond,
			Jitter:    func(time.Duration) time.Duration { return 0 },
		},
	})

	out := exec.Execute(context.Background(), queryMetrics, json.RawMessage(`{"service":"payment-service","metric":"error_rate"}`))
	if out.OK() {
		t.Fatal("死端口不可能查成功")
	}
	if out.Class != ClassRetryable {
		t.Errorf("Class = %s, 期望 retryable（原始错误: %v）", out.Class, out.Error)
	}
	// 工具配置 MaxRetry=2 → 最多 3 次尝试
	if len(out.Attempts) != 3 {
		t.Errorf("尝试次数 = %d, 期望 3（1 + MaxRetry=2）", len(out.Attempts))
	}
	for i, a := range out.Attempts {
		if a.Class != ClassRetryable {
			t.Errorf("第 %d 次尝试分类 = %s, 期望 retryable", i+1, a.Class)
		}
	}
}

// TestRealMySQLThroughExecutor 只在提供 SAFEGUARD_TEST_DB_DSN 时运行。
//
// 它验证的是相反的一面：**健康的下游不应被熔断或重试误伤**——成功路径必须只有 1 次尝试。
// 运行方式：
//
//	$env:SAFEGUARD_TEST_DB_DSN="appuser:...@tcp(host:3306)/db"; go test ./internal/executor -run RealMySQL -v
func TestRealMySQLThroughExecutor(t *testing.T) {
	dsn := os.Getenv("SAFEGUARD_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("未设置 SAFEGUARD_TEST_DB_DSN，跳过真实数据库集成测试")
	}

	ctx := context.Background()
	// 走生产同一条装配路径（含 DSN 归一化、TLS、UTC 时钟）
	st, err := store.Open(ctx, store.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	defer st.Close()

	reg, err := tools.NewDefaultRegistry(tools.NewMySQLDataSource(st.GORM()))
	if err != nil {
		t.Fatalf("构造注册中心失败: %v", err)
	}
	queryMetrics, _ := reg.Get("query_metrics")

	exec := New(DefaultConfig())
	out := exec.Execute(ctx, queryMetrics, json.RawMessage(`{"service":"payment-service","metric":"error_rate","window":"2h"}`))
	if !out.OK() {
		t.Fatalf("健康下游应当查询成功: %v", out.Error)
	}
	if len(out.Attempts) != 1 {
		t.Errorf("成功路径不该重试，尝试次数 = %d", len(out.Attempts))
	}
	if got := exec.Breakers().States()["query_metrics"]; got != "closed" {
		t.Errorf("熔断状态 = %s, 期望 closed", got)
	}
	if out.Result.Summary == "" {
		t.Error("结果摘要不应为空")
	}
	t.Logf("真实查询结果: %s（耗时 %s）", out.Result.Summary, out.Duration.Round(time.Millisecond))
}

// TestRealMySQLThreePhaseAndReplay 在真实 MySQL 上验证 D5 的三段式落库与回放。
//
// 需要 SAFEGUARD_TEST_DB_DSN。会写入一条任务与一条工具调用，结束时删除任务
// （顺便验证 tool_calls 的级联删除，也就是"执行状态可清理、审计保留"的那条设计）。
func TestRealMySQLThreePhaseAndReplay(t *testing.T) {
	dsn := os.Getenv("SAFEGUARD_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("未设置 SAFEGUARD_TEST_DB_DSN，跳过真实数据库集成测试")
	}

	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	// 用 t.Cleanup 而不是 defer：LIFO 顺序保证"删数据"发生在"关连接"之前。
	t.Cleanup(func() { _ = st.Close() })

	taskRepo := store.NewTaskRepo(st.GORM())
	taskID := fmt.Sprintf("task_it_%d", time.Now().UnixNano())
	now := time.Now().UTC()
	if err := taskRepo.Create(ctx, &store.Task{
		TaskID: taskID, UserQuery: "集成测试：三段式落库", Status: store.TaskStatusRunning,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	t.Cleanup(func() {
		if err := st.GORM().Where("task_id = ?", taskID).Delete(&store.Task{}).Error; err != nil {
			t.Logf("清理任务失败: %v", err)
		}
	})

	reg, err := tools.NewDefaultRegistry(tools.NewMySQLDataSource(st.GORM()))
	if err != nil {
		t.Fatalf("构造注册中心失败: %v", err)
	}
	queryMetrics, _ := reg.Get("query_metrics")

	toolCalls := store.NewToolCallRepo(st.GORM())
	pe := NewPersisted(New(DefaultConfig()), toolCalls)

	ref := CallRef{TaskID: taskID, StepNo: 1, CallIndex: 1}
	args := json.RawMessage(`{"service":"payment-service","metric":"error_rate","window":"2h"}`)

	first := pe.Run(ctx, queryMetrics, args, ref)
	if !first.OK() {
		t.Fatalf("首次执行应成功: %v", first.Error)
	}
	if first.Replayed {
		t.Error("首次执行不应是回放")
	}

	second := pe.Run(ctx, queryMetrics, args, ref)
	if !second.OK() {
		t.Fatalf("第二次执行应成功（回放）: %v", second.Error)
	}
	if !second.Replayed {
		t.Error("第二次应命中回放")
	}
	if second.IdemKey != first.IdemKey {
		t.Errorf("两次 key 不同: %s != %s", second.IdemKey, first.IdemKey)
	}

	rows, err := toolCalls.ListByTask(ctx, taskID)
	if err != nil {
		t.Fatalf("读取工具调用失败: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("tool_calls 应只有 1 行，实际 %d 行", len(rows))
	}
	row := rows[0]
	if row.Status != store.ToolCallStatusSuccess {
		t.Errorf("状态 = %s, 期望 success", row.Status)
	}
	if row.Result == nil {
		t.Error("result 未回写")
	}
	if row.FinishedAt == nil {
		t.Error("finished_at 未写")
	}
	t.Logf("三段式落库: key=%s status=%s tier=%d retry=%d", row.IdemKey, row.Status, row.Tier, row.RetryCount)

	// 删除任务 → tool_calls 级联清理（执行状态可重建），这条是 D1 就定下的行为。
	if err := st.GORM().Where("task_id = ?", taskID).Delete(&store.Task{}).Error; err != nil {
		t.Fatalf("删除任务失败: %v", err)
	}
	remaining, err := toolCalls.ListByTask(ctx, taskID)
	if err != nil {
		t.Fatalf("删除后读取工具调用失败: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("任务删除后 tool_calls 应级联清空，实际还剩 %d 行", len(remaining))
	}
}
