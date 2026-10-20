package store

import "time"

// 本文件是 5 张表的 GORM 模型。
//
// ⚠️ 边界（这是本项目的一条硬规矩）：
// **模型只用于数据访问，schema 的唯一定义是 migrations/*.sql。**
// 不要对这些模型调用 AutoMigrate。原因：
//   - AutoMigrate 表达不了 CHECK 约束与索引注释，也建不出我们刻意命名的外键；
//   - 它没有版本与 checksum 概念，谁改了结构、什么时候改的，事后无从追溯；
//   - 它是"静默 ALTER"——加列加索引不报错，而我们宁可启动失败也不要 schema 漂移。
//
// 另一条约定：**状态列一律用本文档的常量字符串，不用魔法字符串，
// 也不要在 Raw SQL 里改状态列**——updated_at 由 GORM 的 autoUpdateTime 维护，
// 绕过 GORM 直接 UPDATE 状态列会让 updated_at 停在旧值上。
//
// 关于 JSON 列的类型选择：模型里一律用 string / *string，而不是 []byte 或 json.RawMessage。
// []byte 会以二进制字符集发送，MySQL 可能拒绝写入 JSON 列
// （Cannot create a JSON value from a string with CHARACTER SET 'binary'）；
// string 走普通字符集，由 MySQL 正常解析并校验 JSON 合法性。
// 可空列用指针：写 nil 得到 SQL NULL；往 JSON 列写空字符串会被拒绝（"" 不是合法 JSON）。

// tasks.status 的取值，必须与 0001_init.sql 的 chk_tasks_status 保持一致。
// 改动这里 = 改动约束 = 必须新增一个迁移文件（已应用的迁移不允许修改）。
const (
	TaskStatusPending         = "pending"
	TaskStatusRunning         = "running"
	TaskStatusWaitingApproval = "waiting_approval"
	TaskStatusSuccess         = "success"
	TaskStatusFailed          = "failed"
	TaskStatusCanceled        = "canceled"
)

// task_steps.status 的取值，对应 chk_task_steps_status。
const (
	StepStatusPending = "pending"
	StepStatusRunning = "running"
	StepStatusDone    = "done"
	StepStatusFailed  = "failed"
	StepStatusSkipped = "skipped"
)

// tool_calls.status 的取值，对应 chk_tool_calls_status。
//
// 这五个值就是幂等执行器（D5）的状态机：started 是"意图已落库"，
// waiting_approval 是"卡在人工审批"，success 是"下游已执行且结果已回写"。
// 注意 "started 但没有 finished_at" 是一个**必须能被恢复流程消费**的状态，
// 而不是一条异常日志。
const (
	ToolCallStatusStarted         = "started"
	ToolCallStatusExecuting       = "executing"
	ToolCallStatusWaitingApproval = "waiting_approval"
	ToolCallStatusSuccess         = "success"
	ToolCallStatusFailed          = "failed"
	ToolCallStatusSkipped         = "skipped"
)

// approvals.status 的取值，对应 chk_approvals_status。
const (
	ApprovalStatusPending  = "pending"
	ApprovalStatusApproved = "approved"
	ApprovalStatusRejected = "rejected"
	ApprovalStatusExpired  = "expired"
)

// Task 对应 tasks 表：一次完整的用户请求。
type Task struct {
	TaskID      string    `gorm:"column:task_id;primaryKey;size:64"`
	UserQuery   string    `gorm:"column:user_query;type:text;not null"`
	Status      string    `gorm:"column:status;size:32;not null"`
	CurrentStep int       `gorm:"column:current_step;not null"`
	TotalRounds int       `gorm:"column:total_rounds;not null"`
	CreatedAt   time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt   time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

func (Task) TableName() string { return "tasks" }

// TaskStep 对应 task_steps 表：一次工具调用 = 一个步骤。
// step_no 按"调用"递增，round_no 记录它属于第几轮 ReAct——恢复重放的原子单位就是 step。
type TaskStep struct {
	TaskID    string    `gorm:"column:task_id;primaryKey;size:64"`
	StepNo    int       `gorm:"column:step_no;primaryKey"`
	RoundNo   int       `gorm:"column:round_no;not null"`
	Thought   *string   `gorm:"column:thought;type:text"`
	Plan      *string   `gorm:"column:plan;type:json"`
	Status    string    `gorm:"column:status;size:32;not null"`
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

func (TaskStep) TableName() string { return "task_steps" }

// ToolCall 对应 tool_calls 表，是幂等与恢复的核心。
//
// IdemKey 是主键，不是自增 id：这样"这个操作到底发出去过没有"是一次主键查询能回答的问题，
// 而不是靠翻日志反推。Tier 与 ErrClass 是整数枚举（0/1/2 与 0..3），
// 它们的 Go 侧类型分别由 D2 的工具层与 D4 的执行器定义，这里只按 schema 存值。
type ToolCall struct {
	IdemKey    string     `gorm:"column:idem_key;primaryKey;size:32"`
	TaskID     string     `gorm:"column:task_id;size:64;not null"`
	StepNo     int        `gorm:"column:step_no;not null"`
	CallIndex  int        `gorm:"column:call_index;not null"`
	ToolName   string     `gorm:"column:tool_name;size:64;not null"`
	Args       string     `gorm:"column:args;type:json;not null"`
	Tier       int8       `gorm:"column:tier;not null"`
	Status     string     `gorm:"column:status;size:32;not null"`
	Result     *string    `gorm:"column:result;type:json"`
	ErrClass   *int8      `gorm:"column:err_class"`
	ErrMessage *string    `gorm:"column:err_message;type:text"`
	RetryCount int        `gorm:"column:retry_count;not null"`
	StartedAt  time.Time  `gorm:"column:started_at;not null"`
	FinishedAt *time.Time `gorm:"column:finished_at"`
	// 执行租约：抢占执行权用（0004_claim.sql）。attempt 记录被抢到几次，便于发现"超时被重抢"。
	ClaimOwner     *string    `gorm:"column:claim_owner;size:64"`
	ClaimExpiresAt *time.Time `gorm:"column:claim_expires_at"`
	Attempt        int        `gorm:"column:attempt;not null"`
}

func (ToolCall) TableName() string { return "tool_calls" }

// Approval 对应 approvals 表：审批工单。
// IdemKey 上有唯一约束——恢复流程重入时不会重复建单，这是恢复路径的幂等保证。
type Approval struct {
	TicketID  string     `gorm:"column:ticket_id;primaryKey;size:64"`
	IdemKey   string     `gorm:"column:idem_key;size:32;not null"`
	TaskID    string     `gorm:"column:task_id;size:64;not null"`
	StepNo    int        `gorm:"column:step_no;not null"`
	ToolName  string     `gorm:"column:tool_name;size:64;not null"`
	Args      string     `gorm:"column:args;type:json;not null"`
	Preview   *string    `gorm:"column:preview;type:text"`
	Status    string     `gorm:"column:status;size:32;not null"`
	Approver  *string    `gorm:"column:approver;size:64"`
	Reason    *string    `gorm:"column:reason;type:text"`
	ExpiresAt time.Time  `gorm:"column:expires_at;not null"`
	DecidedAt *time.Time `gorm:"column:decided_at"`
	CreatedAt time.Time  `gorm:"column:created_at;autoCreateTime"`
}

func (Approval) TableName() string { return "approvals" }

// AuditLog 对应 audit_logs 表：只增不改，回答"谁在什么时候做了什么"。
//
// 它刻意没有外键、也没有 UpdatedAt：审计记录不该被更新，任务被删除也不该抹掉它。
// TaskID 可空，因为鉴权失败、工单审批这类行为可能不属于任何任务。
type AuditLog struct {
	ID        uint64    `gorm:"column:id;primaryKey;autoIncrement"`
	TaskID    *string   `gorm:"column:task_id;size:64"`
	Actor     string    `gorm:"column:actor;size:64;not null"`
	Action    string    `gorm:"column:action;size:64;not null"`
	Target    *string   `gorm:"column:target;size:255"`
	Detail    *string   `gorm:"column:detail;type:json"`
	TraceID   *string   `gorm:"column:trace_id;size:64"`
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
}

func (AuditLog) TableName() string { return "audit_logs" }

// services.status 的取值，对应 chk_services_status。
const (
	ServiceStatusHealthy  = "healthy"
	ServiceStatusDegraded = "degraded"
	ServiceStatusDown     = "down"
)

// 服务重要级别，对应 chk_services_criticality。
// ⚠️ 与工具的 Tier（风险等级）是两件事：核心服务上的只读查询依然是无害操作。
const (
	ServiceCriticalityCore      int8 = 0
	ServiceCriticalityImportant int8 = 1
	ServiceCriticalityNormal    int8 = 2
)

// Service 对应 services 表：服务目录，只读工具 list_services 的数据源。
type Service struct {
	Name        string    `gorm:"column:name;primaryKey;size:64"`
	Criticality int8      `gorm:"column:criticality;not null"`
	Owner       string    `gorm:"column:owner;size:64;not null"`
	Status      string    `gorm:"column:status;size:32;not null"`
	Description *string   `gorm:"column:description;size:255"`
	CreatedAt   time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt   time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

func (Service) TableName() string { return "services" }

// ServiceConfig 对应 service_configs 表：只读工具 query_config 的数据源。
type ServiceConfig struct {
	Service     string    `gorm:"column:service;primaryKey;size:64"`
	ConfigKey   string    `gorm:"column:config_key;primaryKey;size:64"`
	ConfigValue string    `gorm:"column:config_value;size:255;not null"`
	UpdatedAt   time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

func (ServiceConfig) TableName() string { return "service_configs" }

// ServiceMetric 对应 service_metrics 表：只读工具 query_metrics 的数据源。
//
// 这是时间序列，没有 created_at/updated_at：时间语义由 window_start 承担，
// 而它属于数据本身（采样窗口起点），不是"行的创建时间"。
type ServiceMetric struct {
	Service     string    `gorm:"column:service;primaryKey;size:64"`
	Metric      string    `gorm:"column:metric;primaryKey;size:32"`
	WindowStart time.Time `gorm:"column:window_start;primaryKey"`
	Value       float64   `gorm:"column:value;not null"`
}

func (ServiceMetric) TableName() string { return "service_metrics" }
