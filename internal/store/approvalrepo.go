package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// ApprovalRepo 是审批工单的仓储。
//
// 一条硬规矩：**先落库再挂起**。工单必须能在进程退出后重新读到，
// 所以"创建工单"这一步不能只发生在内存里。
type ApprovalRepo struct {
	db *gorm.DB
}

// NewApprovalRepo 构造仓储。
func NewApprovalRepo(db *gorm.DB) *ApprovalRepo { return &ApprovalRepo{db: db} }

// Create 创建工单。
//
// `idem_key` 上有唯一约束：同一个工具调用只允许一张工单。
// 已存在时不报错、直接返回已有工单——恢复流程重入时不会重复建单。
func (r *ApprovalRepo) Create(ctx context.Context, t *Approval) (*Approval, bool, error) {
	if err := r.db.WithContext(ctx).Create(t).Error; err != nil {
		if !isDuplicateEntry(err) {
			return nil, false, fmt.Errorf("store: 创建审批工单失败: %w", err)
		}
		var existing Approval
		if readErr := r.db.WithContext(ctx).Where("idem_key = ?", t.IdemKey).First(&existing).Error; readErr != nil {
			return nil, false, fmt.Errorf("store: 读取已存在的审批工单失败: %w", readErr)
		}
		return &existing, true, nil
	}
	return t, false, nil
}

// Get 按工单号读取。
func (r *ApprovalRepo) Get(ctx context.Context, ticketID string) (Approval, error) {
	var t Approval
	if err := r.db.WithContext(ctx).Where("ticket_id = ?", ticketID).First(&t).Error; err != nil {
		return Approval{}, fmt.Errorf("store: 读取审批工单 %s 失败: %w", ticketID, err)
	}
	return t, nil
}

// Decide 记录审批结果。
//
// 只允许 pending → approved / rejected 这一种迁移：
// 用 Where(status='pending') 做条件更新，避免"已经批过的工单被再批一次"或"批完再拒"。
// RowsAffected==0 说明工单不存在，或者已经不在 pending 状态。
func (r *ApprovalRepo) Decide(ctx context.Context, ticketID, status, approver, reason string) error {
	if status != ApprovalStatusApproved && status != ApprovalStatusRejected {
		return fmt.Errorf("store: 审批结果只能是 approved/rejected，收到 %q", status)
	}
	updates := map[string]any{
		"status":     status,
		"approver":   approver,
		"reason":     reason,
		"decided_at": time.Now().UTC(),
	}
	tx := r.db.WithContext(ctx).
		Model(&Approval{}).
		Where("ticket_id = ? AND status = ?", ticketID, ApprovalStatusPending).
		Updates(updates)
	if tx.Error != nil {
		return fmt.Errorf("store: 更新审批工单失败: %w", tx.Error)
	}
	if tx.RowsAffected == 0 {
		return fmt.Errorf("store: 工单 %s 不存在或已不是待审批状态", ticketID)
	}
	return nil
}

// ListPending 列出待审批工单（按创建时间升序）。
func (r *ApprovalRepo) ListPending(ctx context.Context) ([]Approval, error) {
	var tickets []Approval
	if err := r.db.WithContext(ctx).
		Where("status = ?", ApprovalStatusPending).
		Order("created_at asc").
		Find(&tickets).Error; err != nil {
		return nil, fmt.Errorf("store: 读取待审批工单失败: %w", err)
	}
	return tickets, nil
}

// ExpireOverdue 把超时未处理的工单标记为 expired。
//
// 返回过期数量。调用方（CLI 启动或定期任务）据此清理"永远不会有人批"的挂起。
func (r *ApprovalRepo) ExpireOverdue(ctx context.Context, now time.Time) (int64, error) {
	tx := r.db.WithContext(ctx).
		Model(&Approval{}).
		Where("status = ? AND expires_at <= ?", ApprovalStatusPending, now).
		Updates(map[string]any{
			"status":     ApprovalStatusExpired,
			"decided_at": now,
		})
	if tx.Error != nil {
		return 0, fmt.Errorf("store: 过期审批工单失败: %w", tx.Error)
	}
	return tx.RowsAffected, nil
}

// ErrTicketNotFound 便于调用方判断。
var ErrTicketNotFound = errors.New("审批工单不存在")
