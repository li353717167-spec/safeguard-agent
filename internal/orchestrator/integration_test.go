package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/mechrevo/safeguard-agent/internal/approval"
	"github.com/mechrevo/safeguard-agent/internal/executor"
	"github.com/mechrevo/safeguard-agent/internal/llm"
	"github.com/mechrevo/safeguard-agent/internal/store"
	"github.com/mechrevo/safeguard-agent/internal/tools"
)

// TestRealDBParkingAndApproval 在真实 MySQL 上验证 D6 的完整挂起链路：
// 高危工具被拦住 → 先落库（执行意图 + 审批工单）→ 任务状态 waiting_approval → 审批通过。
//
// 需要 SAFEGUARD_TEST_DB_DSN。它不需要模型（用脚本化的假模型），所以模型端点下线时也能跑。
func TestRealDBParkingAndApproval(t *testing.T) {
	dsn := os.Getenv("SAFEGUARD_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("未设置 SAFEGUARD_TEST_DB_DSN，跳过真实数据库集成测试")
	}

	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	// 用 t.Cleanup 而不是 defer：Cleanup 按 LIFO 执行，
	// 先注册关闭数据库、后注册删除数据，删除就会发生在关闭之前。
	// （写成 defer 时 defer 先跑，清理会拿到一个已关闭的连接池，然后静默失败。）
	t.Cleanup(func() { _ = st.Close() })

	ds := tools.NewMySQLDataSource(st.GORM())
	reg, err := tools.NewFullRegistry(ds, ds)
	if err != nil {
		t.Fatalf("注册工具失败: %v", err)
	}

	toolCalls := store.NewToolCallRepo(st.GORM())
	approvals := store.NewApprovalRepo(st.GORM())
	gate := approval.NewGate(toolCalls, approvals)

	taskID := fmt.Sprintf("task_it6_%d", time.Now().UnixNano())
	client := &fakeLLM{responses: []llm.ChatResponse{
		toolCallResponse(call("call_1", "restart_service", `{"service":"checkout-service","reason":"错误率持续上升"}`)),
	}}

	o := New(client, reg, store.NewTaskRepo(st.GORM()), Options{
		TaskID: taskID,
		// 落库执行器：即便挂起路径用不到它，也保持与生产一致的装配。
		Runner: executor.NewPersisted(executor.New(executor.DefaultConfig()), toolCalls),
		Gate:   gate,
	})

	res, err := o.Run(ctx, "checkout-service 错误率上升了，帮我重启一下")
	if err != nil {
		t.Fatalf("挂起不应返回错误: %v", err)
	}
	if !res.WaitingApproval() || res.Ticket == nil {
		t.Fatalf("应当挂起并带回工单，实际 stop=%s ticket=%v", res.Stop, res.Ticket)
	}
	t.Logf("已挂起：ticket=%s 预览=%q", res.Ticket.TicketID, derefString(res.Ticket.Preview))

	// 清理：工单没有外键，需要单独删
	t.Cleanup(func() {
		_ = st.GORM().Where("task_id = ?", taskID).Delete(&store.Task{}).Error
		_ = st.GORM().Where("ticket_id = ?", res.Ticket.TicketID).Delete(&store.Approval{}).Error
	})

	// ① 任务状态落成 waiting_approval
	task, err := store.NewTaskRepo(st.GORM()).Get(ctx, taskID)
	if err != nil {
		t.Fatalf("读取任务失败: %v", err)
	}
	if task.Status != store.TaskStatusWaitingApproval {
		t.Errorf("任务状态 = %s, 期望 waiting_approval", task.Status)
	}

	// ② 执行意图 + 工单都已落库
	rows, err := toolCalls.ListByTask(ctx, taskID)
	if err != nil {
		t.Fatalf("读取工具调用失败: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("tool_calls 应有 1 行，实际 %d", len(rows))
	}
	if rows[0].Status != store.ToolCallStatusWaitingApproval {
		t.Errorf("意图状态 = %s, 期望 waiting_approval", rows[0].Status)
	}
	if rows[0].ToolName != "restart_service" {
		t.Errorf("工具名 = %s", rows[0].ToolName)
	}

	pending, err := approvals.ListPending(ctx)
	if err != nil {
		t.Fatalf("读取待审批工单失败: %v", err)
	}
	var found *store.Approval
	for i := range pending {
		if pending[i].TicketID == res.Ticket.TicketID {
			found = &pending[i]
		}
	}
	if found == nil {
		t.Fatal("工单没有出现在待审批列表里")
	}
	if found.Preview == nil || *found.Preview == "" {
		t.Error("工单缺少预览")
	}
	if found.ExpiresAt.Before(time.Now().UTC()) {
		t.Error("工单过期时间应在未来")
	}

	// ③ 审批通过
	approved, err := gate.Decide(ctx, res.Ticket.TicketID, store.ApprovalStatusApproved, "sre-oncall", "确认可以重启")
	if err != nil {
		t.Fatalf("审批失败: %v", err)
	}
	if approved.Status != store.ApprovalStatusApproved {
		t.Errorf("工单状态 = %s, 期望 approved", approved.Status)
	}
	if approved.Approver == nil || *approved.Approver != "sre-oncall" {
		t.Errorf("审批人未记录: %v", approved.Approver)
	}

	// ④ 审批后仍是 waiting_approval 的执行意图——真正"续跑"是 D7 的事
	rows, err = toolCalls.ListByTask(ctx, taskID)
	if err != nil {
		t.Fatalf("读取工具调用失败: %v", err)
	}
	if rows[0].Status != store.ToolCallStatusWaitingApproval {
		t.Errorf("审批只改工单；执行意图应保持 waiting_approval，实际 %s", rows[0].Status)
	}
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// TestRealDBResumeAfterApproval 在真实 MySQL 上验证 D7 的完整闭环：
// 挂起 → 审批 → 恢复 → 被挂起的调用在**原步骤**上执行一次 → 任务完成。
//
// 它要证明的是"没有重复副作用"：tool_calls 里仍然只有原来那一行、
// config_changes 里只有一条台账，且两者的幂等 key 相同。
func TestRealDBResumeAfterApproval(t *testing.T) {
	dsn := os.Getenv("SAFEGUARD_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("未设置 SAFEGUARD_TEST_DB_DSN，跳过真实数据库集成测试")
	}

	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const (
		service   = "payment-service"
		configKey = "timeout_ms"
		newValue  = "4321"
	)
	catalog := store.NewCatalogRepo(st.GORM())
	oldValue, hadValue, err := catalog.ConfigValue(ctx, service, configKey)
	if err != nil {
		t.Fatalf("读取原配置失败: %v", err)
	}
	t.Cleanup(func() {
		if !hadValue {
			return
		}
		// 还原原值，并把这次还原也记的台账擦掉——测试不该在演示数据里留痕。
		restoreKey := "restore" + fmt.Sprint(time.Now().UnixNano())
		_, _, _ = catalog.ApplyConfig(ctx, restoreKey, service, configKey, oldValue)
		_ = st.GORM().Where("idem_key = ?", restoreKey).Delete(&store.ConfigChange{}).Error
	})

	ds := tools.NewMySQLDataSource(st.GORM())
	reg, err := tools.NewFullRegistry(ds, ds)
	if err != nil {
		t.Fatalf("注册工具失败: %v", err)
	}
	updateTool, ok := reg.Get("update_config")
	if !ok {
		t.Fatal("找不到 update_config")
	}

	// 先走一次 dry-run 拿到 confirm 令牌（这正是工具强制的"二次确认"流程）。
	preview, err := updateTool.Invoke(ctx, json.RawMessage(
		fmt.Sprintf(`{"service":%q,"key":%q,"value":%q}`, service, configKey, newValue)))
	if err != nil {
		t.Fatalf("预览失败: %v", err)
	}
	previewData, _ := preview.Data.(map[string]any)
	token, _ := previewData["confirm"].(string)
	if token == "" {
		t.Fatalf("预览没有返回 confirm 令牌: %#v", preview.Data)
	}

	toolCalls := store.NewToolCallRepo(st.GORM())
	tickets := store.NewApprovalRepo(st.GORM())
	gate := approval.NewGate(toolCalls, tickets)

	taskID := fmt.Sprintf("task_it7_%d", time.Now().UnixNano())
	realArgs := fmt.Sprintf(`{"service":%q,"key":%q,"value":%q,"dry_run":false,"confirm":%q}`,
		service, configKey, newValue, token)
	client := &fakeLLM{responses: []llm.ChatResponse{
		toolCallResponse(call("call_1", "update_config", realArgs)),
		answerResponse("已完成配置修改。"),
	}}

	o := New(client, reg, store.NewTaskRepo(st.GORM()), Options{
		TaskID: taskID,
		Runner: executor.NewPersisted(executor.New(executor.DefaultConfig()), toolCalls),
		Gate:   gate,
	})
	t.Cleanup(func() {
		_ = st.GORM().Where("task_id = ?", taskID).Delete(&store.Task{}).Error
		_ = st.GORM().Where("task_id = ?", taskID).Delete(&store.Approval{}).Error
	})

	// ① 挂起
	res, err := o.Run(ctx, "把 payment-service 的 timeout_ms 改成 4321")
	if err != nil {
		t.Fatalf("挂起失败: %v", err)
	}
	if !res.WaitingApproval() || res.Ticket == nil {
		t.Fatalf("应当挂起，实际 stop=%s", res.Stop)
	}
	rows, err := toolCalls.ListByTask(ctx, taskID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("应有 1 条执行意图: rows=%d err=%v", len(rows), err)
	}
	parkedKey := rows[0].IdemKey
	if rows[0].Status != store.ToolCallStatusWaitingApproval {
		t.Fatalf("意图状态 = %s, 期望 waiting_approval", rows[0].Status)
	}
	// 测试写进下游台账的那条也要清掉，别留在演示数据里。
	t.Cleanup(func() {
		_ = st.GORM().Where("idem_key = ?", parkedKey).Delete(&store.ConfigChange{}).Error
	})

	// ② 审批
	if _, err := gate.Decide(ctx, res.Ticket.TicketID, store.ApprovalStatusApproved, "tester", "同意修改"); err != nil {
		t.Fatalf("审批失败: %v", err)
	}

	// ③ 恢复
	res2, err := o.Resume(ctx, taskID)
	if err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	if res2.Stop != StopAnswered {
		t.Errorf("恢复后应正常结束，实际 stop=%s", res2.Stop)
	}

	// ④ 没有重复副作用
	rows, err = toolCalls.ListByTask(ctx, taskID)
	if err != nil {
		t.Fatalf("读取工具调用失败: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("tool_calls 应仍只有 1 行，实际 %d 行（说明恢复变成了重复执行）", len(rows))
	}
	if rows[0].IdemKey != parkedKey {
		t.Errorf("执行记录的幂等 key 变了：%s != %s", rows[0].IdemKey, parkedKey)
	}
	if rows[0].Status != store.ToolCallStatusSuccess {
		t.Errorf("执行记录状态 = %s, 期望 success", rows[0].Status)
	}

	gotValue, found, err := catalog.ConfigValue(ctx, service, configKey)
	if err != nil || !found || gotValue != newValue {
		t.Errorf("配置未按预期生效: value=%q found=%v err=%v", gotValue, found, err)
	}

	var changeCount int64
	if err := st.GORM().Model(&store.ConfigChange{}).Where("idem_key = ?", parkedKey).Count(&changeCount).Error; err != nil {
		t.Fatalf("统计变更台账失败: %v", err)
	}
	if changeCount != 1 {
		t.Errorf("变更台账应有 1 条（幂等 key 命中同一条），实际 %d", changeCount)
	}

	task, err := store.NewTaskRepo(st.GORM()).Get(ctx, taskID)
	if err != nil {
		t.Fatalf("读取任务失败: %v", err)
	}
	if task.Status != store.TaskStatusSuccess {
		t.Errorf("任务状态 = %s, 期望 success", task.Status)
	}
	t.Logf("恢复完成：task=%s key=%s 配置=%s→%s 台账=%d 行", taskID, parkedKey, oldValue, gotValue, changeCount)
}

// TestRealDBAuditTrail 在真实 MySQL 上验证审计落地：
// 变更前写"开始"、变更后写结果、任务删除后审计仍然存活（只增不删）。
func TestRealDBAuditTrail(t *testing.T) {
	dsn := os.Getenv("SAFEGUARD_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("未设置 SAFEGUARD_TEST_DB_DSN，跳过真实数据库集成测试")
	}

	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ds := tools.NewMySQLDataSource(st.GORM())
	reg, err := tools.NewFullRegistry(ds, ds)
	if err != nil {
		t.Fatalf("注册工具失败: %v", err)
	}

	taskID := fmt.Sprintf("task_it_audit_%d", time.Now().UnixNano())
	gate := approval.NewGate(store.NewToolCallRepo(st.GORM()), store.NewApprovalRepo(st.GORM()))
	auditRepo := store.NewAuditRepo(st.GORM())

	t.Cleanup(func() {
		// 审计只增不删，但测试不该在演示数据里留痕：显式清掉本用例产生的记录。
		_ = st.GORM().Where("task_id = ?", taskID).Delete(&store.AuditLog{}).Error
		_ = st.GORM().Where("task_id = ?", taskID).Delete(&store.Task{}).Error
		_ = st.GORM().Where("task_id = ?", taskID).Delete(&store.Approval{}).Error
		// restart 会把 checkout-service 置成 healthy，恢复成 seed 的 degraded
		_ = st.GORM().Exec("UPDATE services SET status = 'degraded' WHERE name = 'checkout-service'").Error
	})

	client := &fakeLLM{responses: []llm.ChatResponse{
		toolCallResponse(call("call_1", "restart_service", `{"service":"checkout-service","reason":"错误率上升"}`)),
		answerResponse("已重启。"),
	}}
	o := New(client, reg, store.NewTaskRepo(st.GORM()), Options{
		TaskID:  taskID,
		Gate:    gate,
		Auditor: auditRepo,
	})

	res, err := o.Run(ctx, "重启 checkout-service")
	if err != nil || !res.WaitingApproval() {
		t.Fatalf("应当挂起: err=%v stop=%s", err, res.Stop)
	}

	// 挂起阶段：应有 approval.requested
	rows, err := auditRepo.ListByTask(ctx, taskID)
	if err != nil {
		t.Fatalf("读取审计失败: %v", err)
	}
	if !hasAction(rows, store.AuditApprovalRequested) {
		t.Fatalf("缺少 approval.requested，实际: %v", actionsOf(rows))
	}

	// 审批 + 恢复：应补上 tool.executing 与 tool.executed
	if _, err := gate.Decide(ctx, res.Ticket.TicketID, store.ApprovalStatusApproved, "tester", "同意"); err != nil {
		t.Fatalf("审批失败: %v", err)
	}
	if _, err := o.Resume(ctx, taskID); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}

	rows, err = auditRepo.ListByTask(ctx, taskID)
	if err != nil {
		t.Fatalf("读取审计失败: %v", err)
	}
	for _, want := range []string{store.AuditApprovalRequested, store.AuditToolExecuting, store.AuditToolExecuted} {
		if !hasAction(rows, want) {
			t.Errorf("缺少 %s，实际: %v", want, actionsOf(rows))
		}
	}
	before := len(rows)

	// 删除任务 → tool_calls 级联清空，但审计必须留下
	if err := st.GORM().Where("task_id = ?", taskID).Delete(&store.Task{}).Error; err != nil {
		t.Fatalf("删除任务失败: %v", err)
	}
	rows, err = auditRepo.ListByTask(ctx, taskID)
	if err != nil {
		t.Fatalf("删除后读取审计失败: %v", err)
	}
	if len(rows) != before {
		t.Errorf("任务删除后审计应存活：删除前 %d 条，删除后 %d 条", before, len(rows))
	}
	t.Logf("审计链路: %v", actionsOf(rows))
}

func hasAction(rows []store.AuditLog, action string) bool {
	for _, r := range rows {
		if r.Action == action {
			return true
		}
	}
	return false
}

func actionsOf(rows []store.AuditLog) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Action)
	}
	return out
}
