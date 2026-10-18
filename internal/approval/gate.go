// Package approval 是审批子系统：把"需要人工审批的调用"挂起来。
//
// 关键顺序：**先落库，再挂起**。
// 工单与执行意图都必须能在进程退出后重新读到，所以"创建工单"绝不能只发生在内存里。
// 反过来做（先挂起再落库）时，只要进程在两步之间退出，就会留下一张"人间蒸发"的待办。
package approval

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/mechrevo/safeguard-agent/internal/store"
)

// TicketStore 是审批工单的持久化能力（接口定义在消费方）。
type TicketStore interface {
	Create(ctx context.Context, t *store.Approval) (*store.Approval, bool, error)
	Get(ctx context.Context, ticketID string) (store.Approval, error)
	Decide(ctx context.Context, ticketID, status, approver, reason string) error
	ListPending(ctx context.Context) ([]store.Approval, error)
	ExpireOverdue(ctx context.Context, now time.Time) (int64, error)
}

// IntentStore 负责落"等待审批"的执行意图（复用 tool_calls 仓储）。
type IntentStore interface {
	BeginIntent(ctx context.Context, tc *store.ToolCall) (*store.ToolCall, bool, error)
}

// Gate 是审批门禁。
type Gate struct {
	intents IntentStore
	tickets TicketStore
	now     func() time.Time
}

// NewGate 构造门禁。
func NewGate(intents IntentStore, tickets TicketStore) *Gate {
	return &Gate{intents: intents, tickets: tickets, now: func() time.Time { return time.Now().UTC() }}
}

// ParkRequest 是"挂起一次调用"所需的全部信息。
type ParkRequest struct {
	TaskID    string
	StepNo    int
	CallIndex int
	IdemKey   string
	ToolName  string
	// Args 是规范化后的参数 JSON。
	Args string
	Tier int8
	Role string
	TTL  time.Duration
	// Preview 是给审批人看的人话描述（来自工具的 Spec.Preview）。
	Preview string
}

// ParkResult 是挂起的结果。
type ParkResult struct {
	Ticket store.Approval
	// Created=false 表示命中了已有工单（恢复流程重入时不会重复建单）。
	Created bool
	// Status 是工单**当前**状态：pending / approved / rejected / expired。
	//
	// 为什么要返回它：续跑时同一张工单可能已经被批过（approved），
	// 这时不该再次挂起，而应当**放行执行**；被拒/过期则要把结论回喂给模型。
	Status string
	// AlreadyDone 表示这次调用此前已经成功执行过，无需再挂起。
	AlreadyDone bool
	// ExistingResult 是 AlreadyDone 时的结果 JSON。
	ExistingResult *string
}

// Park 挂起一次需要审批的调用。
//
// 两步顺序不能反：
//  1. 先落**执行意图**（status=waiting_approval）——它回答"这次调用停在哪了"；
//  2. 再建**审批工单**（idem_key 唯一）——它回答"等谁批、批什么"。
func (g *Gate) Park(ctx context.Context, req ParkRequest) (ParkResult, error) {
	now := g.now()

	// ① 执行意图
	intent := &store.ToolCall{
		IdemKey:   req.IdemKey,
		TaskID:    req.TaskID,
		StepNo:    req.StepNo,
		CallIndex: req.CallIndex,
		ToolName:  req.ToolName,
		Args:      req.Args,
		Tier:      req.Tier,
		Status:    store.ToolCallStatusWaitingApproval,
		StartedAt: now,
	}
	existing, existed, err := g.intents.BeginIntent(ctx, intent)
	if err != nil {
		return ParkResult{}, err
	}

	if existed {
		switch existing.Status {
		case store.ToolCallStatusSuccess:
			// 已经成功执行过：不需要再挂起，调用方可以直接回放结果。
			return ParkResult{AlreadyDone: true, ExistingResult: existing.Result}, nil
		case store.ToolCallStatusFailed, store.ToolCallStatusSkipped:
			return ParkResult{}, fmt.Errorf(
				"审批门禁: 这次调用此前已结束（status=%s），不能重复挂起；如确需再次执行，请换一个新的步骤", existing.Status)
		}
		// waiting_approval：继续往下走，Create 会因 idem_key 唯一而返回已有工单。
	}

	// ② 审批工单
	preview := req.Preview
	ticket := &store.Approval{
		TicketID:  newTicketID(),
		IdemKey:   req.IdemKey,
		TaskID:    req.TaskID,
		StepNo:    req.StepNo,
		ToolName:  req.ToolName,
		Args:      req.Args,
		Preview:   &preview,
		Status:    store.ApprovalStatusPending,
		ExpiresAt: now.Add(req.TTL),
		CreatedAt: now,
	}
	saved, created, err := g.tickets.Create(ctx, ticket)
	if err != nil {
		return ParkResult{}, err
	}
	return ParkResult{Ticket: *saved, Created: created, Status: saved.Status}, nil
}

// Decide 记录审批结果，并返回最新工单。
func (g *Gate) Decide(ctx context.Context, ticketID, status, approver, reason string) (store.Approval, error) {
	if err := g.tickets.Decide(ctx, ticketID, status, approver, reason); err != nil {
		return store.Approval{}, err
	}
	return g.tickets.Get(ctx, ticketID)
}

// ListPending 列出待审批工单。
func (g *Gate) ListPending(ctx context.Context) ([]store.Approval, error) {
	return g.tickets.ListPending(ctx)
}

// ExpireOverdue 把超时的待审批工单标记为过期。
func (g *Gate) ExpireOverdue(ctx context.Context) (int64, error) {
	return g.tickets.ExpireOverdue(ctx, g.now())
}

// newTicketID 生成工单号。
// 用随机字节而不是自增：工单号会出现在日志与命令行里，自增号会顺带泄露"系统一共开了多少单"。
func newTicketID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 在支持的操作系统上不会失败；真失败了也只能退化成时间戳。
		return fmt.Sprintf("tk_%d", time.Now().UnixNano())
	}
	return "tk_" + hex.EncodeToString(buf)
}
