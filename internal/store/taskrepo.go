package store

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// TaskRepo 是任务状态机的仓储。
//
// 它只暴露"编排层真正需要"的几个动作，而不是把 *gorm.DB 交出去：
// 状态怎么写、updated_at 谁维护、哪些列允许被改，都由这里说了算。
// 这样 D7 的断点恢复要改写入语义时，改的是一处。
type TaskRepo struct {
	db *gorm.DB
}

// NewTaskRepo 构造仓储。
func NewTaskRepo(db *gorm.DB) *TaskRepo { return &TaskRepo{db: db} }

// Create 落一条新任务。
func (r *TaskRepo) Create(ctx context.Context, task *Task) error {
	if err := r.db.WithContext(ctx).Create(task).Error; err != nil {
		return fmt.Errorf("store: 创建任务失败: %w", err)
	}
	return nil
}

// UpdateProgress 更新任务状态与进度。
//
// ⚠️ 这里用 Select 显式列出要写的列，而不是直接 Updates：
// GORM 的 Updates 默认**跳过零值字段**，而 current_step=0 / total_rounds=0 是合法取值。
// 一旦被跳过，任务表里就会留下一个永远停在旧值的进度——而进度恰恰是 D7 恢复时唯一能依赖的东西。
func (r *TaskRepo) UpdateProgress(ctx context.Context, taskID, status string, currentStep, totalRounds int) error {
	updates := Task{
		Status:      status,
		CurrentStep: currentStep,
		TotalRounds: totalRounds,
		UpdatedAt:   time.Now().UTC(),
	}
	res := r.db.WithContext(ctx).
		Model(&Task{}).
		Where("task_id = ?", taskID).
		Select("status", "current_step", "total_rounds", "updated_at").
		Updates(updates)
	if res.Error != nil {
		return fmt.Errorf("store: 更新任务进度失败: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("store: 任务 %s 不存在，无法更新进度", taskID)
	}
	return nil
}

// AppendStep 写入一个步骤。
//
// 用 upsert（主键 task_id + step_no）：续跑时**会重新处理同一个 step_no**
// （被挂起的那个调用在原步骤号上再次执行），因此这里必须是"更新"而不是"插入"。
// 真正的"只允许一次"发生在 tool_calls 那边——执行记录不允许重复。
func (r *TaskRepo) AppendStep(ctx context.Context, step *TaskStep) error {
	err := r.db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "task_id"}, {Name: "step_no"}},
			DoUpdates: clause.AssignmentColumns([]string{"round_no", "thought", "plan", "status", "updated_at"}),
		}).
		Create(step).Error
	if err != nil {
		return fmt.Errorf("store: 写入步骤 %s#%d 失败: %w", step.TaskID, step.StepNo, err)
	}
	return nil
}

// Get 读一条任务。
func (r *TaskRepo) Get(ctx context.Context, taskID string) (Task, error) {
	var task Task
	if err := r.db.WithContext(ctx).Where("task_id = ?", taskID).First(&task).Error; err != nil {
		return Task{}, fmt.Errorf("store: 读取任务 %s 失败: %w", taskID, err)
	}
	return task, nil
}

// Steps 按步骤号升序读出任务的全部步骤（D7 恢复、CLI 回放都要用）。
func (r *TaskRepo) Steps(ctx context.Context, taskID string) ([]TaskStep, error) {
	var steps []TaskStep
	if err := r.db.WithContext(ctx).
		Where("task_id = ?", taskID).
		Order("step_no asc").
		Find(&steps).Error; err != nil {
		return nil, fmt.Errorf("store: 读取任务 %s 的步骤失败: %w", taskID, err)
	}
	return steps, nil
}
