package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// 审计动作（action 列的取值）。
//
// 只记录**变更与拒绝**，不记录普通读取：
// 只读调用在 tool_calls 里已经有完整记录，再写一份审计只会让 90% 的噪音淹没 10% 的关键事件。
// "谁在什么时候改了什么东西、谁拦下了什么" 才是审计要回答的问题。
const (
	// AuditToolDenied 是一次被拦下的工具调用（权限不足 / 参数非法 / 未注册 / 预算耗尽）。
	// 它没有产生变更，但"有人尝试过"本身就是审计最关心的事实。
	AuditToolDenied = "tool.denied"
	// AuditToolExecuting 是变更类调用的"开始"记录：先落库，再执行。
	AuditToolExecuting = "tool.executing"
	// AuditToolExecuted 是变更类调用的"结束"记录（成功或失败，含结果摘要）。
	AuditToolExecuted = "tool.executed"
	// AuditApprovalRequested 是挂起并生成审批工单。
	AuditApprovalRequested = "approval.requested"
	// AuditApprovalDecided 是人工审批结论。
	AuditApprovalDecided = "approval.decided"
	// AuditTaskResumed 是从断点恢复任务。
	AuditTaskResumed = "task.resumed"
)

// 审计主体（actor 列的取值前缀）。
const (
	ActorCLI      = "user:cli"     // 命令行发起
	ActorAgent    = "system:agent" // Agent 自身发起（模型请求的工具调用）
	ActorApprover = "approver:"    // 人工审批人，后面拼审批者标识
	ActorSystem   = "system"       // 系统行为（过期清理等）
)

// AuditRepo 是审计日志的写入与查询。
//
// 一张硬规矩：**只增不删、不建外键**。任务被删除时执行状态可以级联清空，
// 但"谁在什么时候做了什么"必须留下——这是审计存在的全部意义。
type AuditRepo struct {
	db *gorm.DB
}

// NewAuditRepo 构造审计仓储。
func NewAuditRepo(db *gorm.DB) *AuditRepo { return &AuditRepo{db: db} }

// AuditEvent 是一次待写入的审计事件。
type AuditEvent struct {
	// TaskID 可空：鉴权失败、审批等行为可能不属于任何任务。
	TaskID string
	Actor  string
	Action string
	// Target 是被操作对象，例如 payment-service。
	Target  string
	Detail  map[string]any
	TraceID string
}

// Record 写入一条审计。
func (r *AuditRepo) Record(ctx context.Context, e AuditEvent) error {
	detail, err := marshalDetail(e.Detail)
	if err != nil {
		return err
	}

	row := &AuditLog{
		Actor:     e.Actor,
		Action:    e.Action,
		Detail:    detail,
		CreatedAt: time.Now().UTC(),
	}
	if e.TaskID != "" {
		taskID := e.TaskID
		row.TaskID = &taskID
	}
	if e.Target != "" {
		target := e.Target
		row.Target = &target
	}
	if e.TraceID != "" {
		traceID := e.TraceID
		row.TraceID = &traceID
	}

	if err := r.db.WithContext(ctx).Create(row).Error; err != nil {
		return fmt.Errorf("store: 写入审计失败: %w", err)
	}
	return nil
}

func marshalDetail(detail map[string]any) (*string, error) {
	if len(detail) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return nil, fmt.Errorf("store: 序列化审计详情失败: %w", err)
	}
	s := string(raw)
	return &s, nil
}

// ListByTask 按任务读取审计（时间升序）。
func (r *AuditRepo) ListByTask(ctx context.Context, taskID string) ([]AuditLog, error) {
	var rows []AuditLog
	if err := r.db.WithContext(ctx).
		Where("task_id = ?", taskID).
		Order("created_at asc, id asc").
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("store: 读取审计失败: %w", err)
	}
	return rows, nil
}

// ListRecent 读取最近的审计（跨任务，CLI 用）。
func (r *AuditRepo) ListRecent(ctx context.Context, limit int) ([]AuditLog, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	var rows []AuditLog
	if err := r.db.WithContext(ctx).
		Order("created_at desc, id desc").
		Limit(limit).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("store: 读取审计失败: %w", err)
	}
	return rows, nil
}
