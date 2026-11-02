package store

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ContextArchive 是"被压缩掉的原始上下文"。
//
// 与审计的区别：审计只增不删、要能回答"谁改了什么"；
// 归档是执行状态，任务删了就该跟着走（外键级联）。
type ContextArchive struct {
	ArchiveID string    `gorm:"column:archive_id;primaryKey;size:64"`
	TaskID    string    `gorm:"column:task_id;size:64;not null;index"`
	RoundNo   int       `gorm:"column:round_no;not null"`
	Role      string    `gorm:"column:role;size:16;not null"`
	ToolName  *string   `gorm:"column:tool_name;size:64"`
	Content   string    `gorm:"column:content;type:mediumtext;not null"`
	Tokens    int       `gorm:"column:tokens;not null"`
	CreatedAt time.Time `gorm:"column:created_at;not null"`
}

// TableName 返回表名。
func (ContextArchive) TableName() string { return "context_archive" }

// ContextArchiveEntry 是一条归档记录（仓储与上层交互用的结构）。
type ContextArchiveEntry struct {
	Ref      string
	TaskID   string
	Round    int
	Role     string
	ToolName string
	Content  string
	Tokens   int
}

// ContextArchiveRepo 是上下文归档仓储。
type ContextArchiveRepo struct {
	db *gorm.DB
}

// NewContextArchiveRepo 构造仓储。
func NewContextArchiveRepo(db *gorm.DB) *ContextArchiveRepo {
	return &ContextArchiveRepo{db: db}
}

// Save 批量写入归档。
//
// 用 ON CONFLICT DO NOTHING：ref 是内容哈希，同一条内容重复归档是正常的
// （压缩可能在多轮里反复评估同一段历史），不该因主键冲突让整个压缩失败。
func (r *ContextArchiveRepo) Save(ctx context.Context, entries []ContextArchiveEntry) error {
	if len(entries) == 0 {
		return nil
	}
	rows := make([]ContextArchive, 0, len(entries))
	now := time.Now().UTC()
	for _, e := range entries {
		row := ContextArchive{
			ArchiveID: e.Ref, TaskID: e.TaskID, RoundNo: e.Round,
			Role: e.Role, Content: e.Content, Tokens: e.Tokens, CreatedAt: now,
		}
		if e.ToolName != "" {
			name := e.ToolName
			row.ToolName = &name
		}
		rows = append(rows, row)
	}
	if err := r.db.WithContext(ctx).
		Clauses(clause.OnConflict{DoNothing: true}).
		Create(&rows).Error; err != nil {
		return fmt.Errorf("store: 归档上下文失败: %w", err)
	}
	return nil
}

// Get 按 ref 取一条（必须属于该任务，避免跨任务越权读取）。
func (r *ContextArchiveRepo) Get(ctx context.Context, taskID, ref string) (ContextArchiveEntry, bool, error) {
	var row ContextArchive
	err := r.db.WithContext(ctx).
		Where("archive_id = ? AND task_id = ?", ref, taskID).
		First(&row).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return ContextArchiveEntry{}, false, nil
		}
		return ContextArchiveEntry{}, false, fmt.Errorf("store: 读取归档 %s 失败: %w", ref, err)
	}
	return toEntry(row), true, nil
}

// Search 在任务范围内按关键词搜归档（内容 LIKE %q%），可按轮次过滤。
func (r *ContextArchiveRepo) Search(ctx context.Context, taskID, query string, round, limit int) ([]ContextArchiveEntry, error) {
	if limit <= 0 || limit > 20 {
		limit = 5
	}
	q := r.db.WithContext(ctx).Where("task_id = ?", taskID)
	if round > 0 {
		q = q.Where("round_no = ?", round)
	}
	if query != "" {
		q = q.Where("content LIKE ?", "%"+query+"%")
	}
	var rows []ContextArchive
	if err := q.Order("round_no asc, archive_id asc").Limit(limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("store: 搜索归档失败: %w", err)
	}
	out := make([]ContextArchiveEntry, 0, len(rows))
	for _, row := range rows {
		out = append(out, toEntry(row))
	}
	return out, nil
}

func toEntry(row ContextArchive) ContextArchiveEntry {
	e := ContextArchiveEntry{
		Ref: row.ArchiveID, TaskID: row.TaskID, Round: row.RoundNo,
		Role: row.Role, Content: row.Content, Tokens: row.Tokens,
	}
	if row.ToolName != nil {
		e.ToolName = *row.ToolName
	}
	return e
}
