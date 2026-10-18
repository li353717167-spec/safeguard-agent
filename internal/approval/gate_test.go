package approval

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mechrevo/safeguard-agent/internal/store"
)

// ---------------------------------------------------------------------------
// 内存替身
// ---------------------------------------------------------------------------

type memIntents struct{ rows map[string]*store.ToolCall }

func newMemIntents() *memIntents { return &memIntents{rows: map[string]*store.ToolCall{}} }

func (m *memIntents) BeginIntent(_ context.Context, tc *store.ToolCall) (*store.ToolCall, bool, error) {
	if existing, ok := m.rows[tc.IdemKey]; ok {
		return existing, true, nil
	}
	copied := *tc
	m.rows[tc.IdemKey] = &copied
	return nil, false, nil
}

type memTickets struct {
	byID   map[string]*store.Approval
	byIdem map[string]string
}

func newMemTickets() *memTickets {
	return &memTickets{byID: map[string]*store.Approval{}, byIdem: map[string]string{}}
}

func (m *memTickets) Create(_ context.Context, t *store.Approval) (*store.Approval, bool, error) {
	if id, ok := m.byIdem[t.IdemKey]; ok {
		return m.byID[id], false, nil
	}
	copied := *t
	m.byID[t.TicketID] = &copied
	m.byIdem[t.IdemKey] = t.TicketID
	return &copied, true, nil
}

func (m *memTickets) Get(_ context.Context, ticketID string) (store.Approval, error) {
	if t, ok := m.byID[ticketID]; ok {
		return *t, nil
	}
	return store.Approval{}, store.ErrTicketNotFound
}

func (m *memTickets) Decide(_ context.Context, ticketID, status, approver, reason string) error {
	t, ok := m.byID[ticketID]
	if !ok || t.Status != store.ApprovalStatusPending {
		return store.ErrTicketNotFound
	}
	t.Status = status
	t.Approver = &approver
	t.Reason = &reason
	now := time.Now().UTC()
	t.DecidedAt = &now
	return nil
}

func (m *memTickets) ListPending(context.Context) ([]store.Approval, error) {
	var out []store.Approval
	for _, t := range m.byID {
		if t.Status == store.ApprovalStatusPending {
			out = append(out, *t)
		}
	}
	return out, nil
}

func (m *memTickets) ExpireOverdue(_ context.Context, now time.Time) (int64, error) {
	var n int64
	for _, t := range m.byID {
		if t.Status == store.ApprovalStatusPending && !t.ExpiresAt.After(now) {
			t.Status = store.ApprovalStatusExpired
			n++
		}
	}
	return n, nil
}

func parkReq() ParkRequest {
	return ParkRequest{
		TaskID:    "task_1",
		StepNo:    1,
		CallIndex: 1,
		IdemKey:   "0123456789abcdef0123456789abcdef",
		ToolName:  "restart_service",
		Args:      `{"service":"payment-service"}`,
		Tier:      int8(2),
		Role:      "sre",
		TTL:       15 * time.Minute,
		Preview:   "重启 payment-service（影响线上流量）",
	}
}

func newTestGate() (*Gate, *memIntents, *memTickets) {
	intents := newMemIntents()
	tickets := newMemTickets()
	return NewGate(intents, tickets), intents, tickets
}

// ---------------------------------------------------------------------------
// 用例
// ---------------------------------------------------------------------------

// 挂起必须**两步都落库**：执行意图 + 审批工单。
func TestParkWritesIntentAndTicket(t *testing.T) {
	gate, intents, tickets := newTestGate()

	res, err := gate.Park(context.Background(), parkReq())
	if err != nil {
		t.Fatalf("挂起失败: %v", err)
	}
	if !res.Created {
		t.Error("首次挂起应创建工单")
	}
	if res.Ticket.Status != store.ApprovalStatusPending {
		t.Errorf("工单状态 = %s, 期望 pending", res.Ticket.Status)
	}
	if !strings.HasPrefix(res.Ticket.TicketID, "tk_") {
		t.Errorf("工单号格式不对: %s", res.Ticket.TicketID)
	}
	if res.Ticket.Preview == nil || !strings.Contains(*res.Ticket.Preview, "重启") {
		t.Errorf("工单必须带可读的预览: %v", res.Ticket.Preview)
	}
	if !res.Ticket.ExpiresAt.After(time.Now().UTC()) {
		t.Error("过期时间应在未来")
	}

	intent, ok := intents.rows[parkReq().IdemKey]
	if !ok {
		t.Fatal("执行意图未落库")
	}
	if intent.Status != store.ToolCallStatusWaitingApproval {
		t.Errorf("意图状态 = %s, 期望 waiting_approval", intent.Status)
	}

	if len(tickets.byID) != 1 {
		t.Errorf("应当只有一张工单，实际 %d", len(tickets.byID))
	}
}

// 恢复流程重入时必须复用同一张工单，而不是重复建单。
func TestParkIsIdempotentOnReentry(t *testing.T) {
	gate, intents, tickets := newTestGate()

	first, err := gate.Park(context.Background(), parkReq())
	if err != nil {
		t.Fatalf("首次挂起失败: %v", err)
	}
	second, err := gate.Park(context.Background(), parkReq())
	if err != nil {
		t.Fatalf("再次挂起失败（应当幂等）: %v", err)
	}

	if second.Created {
		t.Error("第二次挂起不应新建工单")
	}
	if second.Ticket.TicketID != first.Ticket.TicketID {
		t.Errorf("工单号变了: %s != %s", second.Ticket.TicketID, first.Ticket.TicketID)
	}
	if len(tickets.byID) != 1 || len(intents.rows) != 1 {
		t.Errorf("不应产生重复记录：tickets=%d intents=%d", len(tickets.byID), len(intents.rows))
	}
}

// 意图已经是 success：不需要再挂起，调用方可以直接回放结果。
func TestParkReportsAlreadyDone(t *testing.T) {
	gate, intents, _ := newTestGate()
	result := `{"status":"healthy"}`
	intents.rows[parkReq().IdemKey] = &store.ToolCall{
		IdemKey: parkReq().IdemKey,
		Status:  store.ToolCallStatusSuccess,
		Result:  &result,
	}

	res, err := gate.Park(context.Background(), parkReq())
	if err != nil {
		t.Fatalf("挂起失败: %v", err)
	}
	if !res.AlreadyDone {
		t.Error("应当报告 AlreadyDone")
	}
	if res.ExistingResult == nil || *res.ExistingResult != result {
		t.Errorf("应带回已有结果: %v", res.ExistingResult)
	}
}

// 已经失败/跳过的调用不能重复挂起，否则用户会看到一张永远批不完的单。
func TestParkRefusesAfterTerminalState(t *testing.T) {
	gate, intents, _ := newTestGate()
	intents.rows[parkReq().IdemKey] = &store.ToolCall{
		IdemKey: parkReq().IdemKey,
		Status:  store.ToolCallStatusFailed,
	}

	if _, err := gate.Park(context.Background(), parkReq()); err == nil {
		t.Fatal("已结束的调用不应能再次挂起")
	} else if !strings.Contains(err.Error(), "不能重复挂起") {
		t.Errorf("错误信息应说明原因: %v", err)
	}
}

// 审批只能在 pending → approved/rejected 之间迁移。
func TestDecideOnlyFromPending(t *testing.T) {
	gate, _, _ := newTestGate()
	res, err := gate.Park(context.Background(), parkReq())
	if err != nil {
		t.Fatalf("挂起失败: %v", err)
	}

	approved, err := gate.Decide(context.Background(), res.Ticket.TicketID, store.ApprovalStatusApproved, "alice", "确认可以重启")
	if err != nil {
		t.Fatalf("批准失败: %v", err)
	}
	if approved.Status != store.ApprovalStatusApproved {
		t.Errorf("状态 = %s, 期望 approved", approved.Status)
	}
	if approved.Approver == nil || *approved.Approver != "alice" {
		t.Errorf("审批人未记录: %v", approved.Approver)
	}
	if approved.DecidedAt == nil {
		t.Error("decided_at 未记录")
	}

	// 已经批过的单不能再拒
	if _, err := gate.Decide(context.Background(), res.Ticket.TicketID, store.ApprovalStatusRejected, "bob", ""); err == nil {
		t.Error("已完成的工单不应能再次变更")
	}
}

// 超时未处理的工单要能被清理，否则会永远占着 pending。
func TestExpireOverdue(t *testing.T) {
	gate, _, tickets := newTestGate()
	res, err := gate.Park(context.Background(), parkReq())
	if err != nil {
		t.Fatalf("挂起失败: %v", err)
	}
	// 把过期时间改到过去
	tickets.byID[res.Ticket.TicketID].ExpiresAt = time.Now().UTC().Add(-time.Minute)

	n, err := gate.ExpireOverdue(context.Background())
	if err != nil {
		t.Fatalf("过期处理失败: %v", err)
	}
	if n != 1 {
		t.Errorf("过期数量 = %d, 期望 1", n)
	}
	if tickets.byID[res.Ticket.TicketID].Status != store.ApprovalStatusExpired {
		t.Errorf("状态 = %s, 期望 expired", tickets.byID[res.Ticket.TicketID].Status)
	}
}
