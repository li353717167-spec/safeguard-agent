package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

// ToolCallRepo 是 tool_calls 表的仓储。幂等执行器的三段式落库就在这里：
// 写意图 → 调下游 → 写结果。
type ToolCallRepo struct {
	db *gorm.DB
}

// NewToolCallRepo 构造仓储。
func NewToolCallRepo(db *gorm.DB) *ToolCallRepo { return &ToolCallRepo{db: db} }

// BeginIntent 落"执行意图"（第一段）。
//
// 返回 (已有记录, existed, error)：
//   - 首次写入：返回 (nil, false, nil)；
//   - key 已存在：**不覆盖**，返回已有记录与 existed=true，
//     由调用方决定是"回放上次结果"还是"用同一个 key 再执行一次"。
//
// 为什么用 MySQL 的 1062 显式判断重复，而不是 GORM 的 TranslateError：
// 后者需要全局配置，且不同驱动对"重复键"的映射并不一致；1062 是 MySQL 的确定语义。
func (r *ToolCallRepo) BeginIntent(ctx context.Context, tc *ToolCall) (*ToolCall, bool, error) {
	err := r.db.WithContext(ctx).Create(tc).Error
	if err == nil {
		return nil, false, nil
	}
	if !isDuplicateEntry(err) {
		return nil, false, fmt.Errorf("store: 写入工具调用意图失败: %w", err)
	}

	var existing ToolCall
	if err := r.db.WithContext(ctx).Where("idem_key = ?", tc.IdemKey).First(&existing).Error; err != nil {
		return nil, false, fmt.Errorf("store: 读取已存在的工具调用失败: %w", err)
	}
	return &existing, true, nil
}

// ToolCallResult 是第三段要回写的内容。
type ToolCallResult struct {
	Status string
	// Result 是下游返回的规范化 JSON；成功才有。
	Result *string
	// ErrClass 是异常分类编码，取值见 0001_init.sql 的注释（0=可重试 1=致命 2=参数错 3=熔断打开）。
	ErrClass   *int8
	ErrMessage *string
	RetryCount int
	FinishedAt time.Time
}

// Claim 抢占一次调用的执行权（乐观 CAS + 租约）。
//
// 允许抢的状态只有三种：
//   - started：意图已落库但还没人抢（首次执行，或进程崩在中间）；
//   - waiting_approval：**且调用方持有审批放行信号**（allowApproval=true）时才允许；
//   - executing：且租约已过期（原执行者崩了，允许别人接手）。
//
// 返回 claimed=false 表示"没抢到"：要么别人正在执行，要么状态已经变成终态（success 等）。
// 调用方在 false 时**必须不要执行**，并重新读一次状态决定是回放还是返回"处理中"。
//
// 为什么用一条 UPDATE 而不是先查后改：查和改之间就是竞态窗口，
// 并发保护的全部意义就在把这两步压成一次原子操作。
func (r *ToolCallRepo) Claim(ctx context.Context, idemKey, owner string, lease time.Duration, allowApproval bool) (bool, error) {
	now := time.Now().UTC()
	expires := now.Add(lease)

	res := r.db.WithContext(ctx).Exec(`
UPDATE tool_calls
   SET status = 'executing',
       claim_owner = ?,
       claim_expires_at = ?,
       attempt = attempt + 1
 WHERE idem_key = ?
   AND ( status = 'started'
      OR ( status = 'waiting_approval' AND ? = 1 )
      OR ( status = 'executing' AND (claim_expires_at IS NULL OR claim_expires_at < ?) ) )`,
		owner, expires, idemKey, boolToInt(allowApproval), now)
	if res.Error != nil {
		return false, fmt.Errorf("store: 抢占执行权失败: %w", res.Error)
	}
	return res.RowsAffected > 0, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// RecordResult 回写执行结果（第三段）。
//
// 用 map 而不是 struct 更新：GORM 对 **struct** 会跳过零值字段，
// 而 `retry_count=0`、`result=NULL` 都是合法取值；map 里的零值会照常写入。
func (r *ToolCallRepo) RecordResult(ctx context.Context, idemKey string, res ToolCallResult) error {
	updates := map[string]any{
		"status":      res.Status,
		"result":      res.Result,
		"err_class":   res.ErrClass,
		"err_message": res.ErrMessage,
		"retry_count": res.RetryCount,
		"finished_at": res.FinishedAt,
		// 执行结束就释放租约：否则这一行会一直显示"有人在执行"。
		"claim_owner":      nil,
		"claim_expires_at": nil,
	}
	tx := r.db.WithContext(ctx).
		Model(&ToolCall{}).
		Where("idem_key = ?", idemKey).
		Updates(updates)
	if tx.Error != nil {
		return fmt.Errorf("store: 回写工具调用结果失败: %w", tx.Error)
	}
	if tx.RowsAffected == 0 {
		return fmt.Errorf("store: 幂等 key %s 不存在，无法回写结果（意图记录缺失）", idemKey)
	}
	return nil
}

// Get 按幂等 key 读一条记录（排障、测试、以及 CLI 回放）。
func (r *ToolCallRepo) Get(ctx context.Context, idemKey string) (ToolCall, error) {
	var tc ToolCall
	if err := r.db.WithContext(ctx).Where("idem_key = ?", idemKey).First(&tc).Error; err != nil {
		return ToolCall{}, fmt.Errorf("store: 读取工具调用 %s 失败: %w", idemKey, err)
	}
	return tc, nil
}

// ListByTask 按任务读出全部工具调用（按步骤号排序）。
func (r *ToolCallRepo) ListByTask(ctx context.Context, taskID string) ([]ToolCall, error) {
	var calls []ToolCall
	if err := r.db.WithContext(ctx).
		Where("task_id = ?", taskID).
		Order("step_no asc, call_index asc").
		Find(&calls).Error; err != nil {
		return nil, fmt.Errorf("store: 读取任务 %s 的工具调用失败: %w", taskID, err)
	}
	return calls, nil
}

func isDuplicateEntry(err error) bool {
	var myErr *mysql.MySQLError
	return errors.As(err, &myErr) && myErr.Number == 1062
}
