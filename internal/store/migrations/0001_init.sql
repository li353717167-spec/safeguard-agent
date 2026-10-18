-- 0001_init.sql · safeguard-agent 初始 schema
-- 方言：MySQL 8.0.16+（用到 CHECK 约束与原生 JSON 类型）
--
-- 三条硬约定（不是风格偏好，是踩过坑之后的规则）：
--   1) 时间列一律 DATETIME(3)，一律由应用写入 UTC，不给 CURRENT_TIMESTAMP 默认值。
--      理由：应用时钟与数据库时钟是两个时间源，一旦服务器时区或容器时区漂移，
--      审计时间线与状态排序就会出现"结果早于开始"这类无法解释的记录。
--   2) 状态列一律 VARCHAR + CHECK 约束。非法状态要在写入时被数据库拒绝，
--      而不是等恢复流程读到一条谁也没定义过的状态再炸。
--   3) 本文件一旦执行就不再允许修改：migrate 会记录并校验 checksum，
--      改了内容启动即失败——宁可启动失败，也不要各环境 schema 静默漂移。
--      因此所有 DDL 都写成幂等形式（IF NOT EXISTS），因为 MySQL 的 DDL 无法回滚。

-- ---------------------------------------------------------------------------
-- 任务：一次完整的用户请求
-- 回答的问题：这批任务做到哪了？有没有卡在等审批？能不能安全续跑？
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS tasks (
    task_id      VARCHAR(64) NOT NULL COMMENT '任务 ID',
    user_query   TEXT        NOT NULL COMMENT '原始用户输入（审计与评测都要回看它）',
    status       VARCHAR(32) NOT NULL COMMENT 'pending/running/waiting_approval/success/failed/canceled',
    current_step INT         NOT NULL DEFAULT 0 COMMENT '已推进到的步骤号',
    total_rounds INT         NOT NULL DEFAULT 0 COMMENT '已消耗的 ReAct 轮次',
    created_at   DATETIME(3) NOT NULL,
    updated_at   DATETIME(3) NOT NULL,
    PRIMARY KEY (task_id),
    -- 启动时"扫描所有挂起任务并重新挂载"走这条索引（D7 断点恢复的第一个查询）
    KEY idx_tasks_status_updated (status, updated_at),
    CONSTRAINT chk_tasks_status CHECK (status IN ('pending','running','waiting_approval','success','failed','canceled'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='任务状态机：编排层的进度事实';

-- ---------------------------------------------------------------------------
-- 步骤：一次工具调用 = 一个步骤（恢复重放的原子单位）
-- round_no 记录它属于第几轮 ReAct；模型一轮可能发起多个工具调用，
-- 因此 step_no 按"调用"递增，而不是按"轮次"递增。
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS task_steps (
    task_id    VARCHAR(64) NOT NULL,
    step_no    INT         NOT NULL COMMENT '步骤号，从 1 开始，单调递增',
    round_no   INT         NOT NULL COMMENT '所属 ReAct 轮次，从 1 开始',
    thought    TEXT        NULL COMMENT '模型本轮的思考文本（若有）',
    plan       JSON        NULL COMMENT '本轮意图的结构化快照，重放与轨迹评测都依赖它',
    status     VARCHAR(32) NOT NULL COMMENT 'pending/running/done/failed/skipped',
    created_at DATETIME(3) NOT NULL,
    updated_at DATETIME(3) NOT NULL,
    PRIMARY KEY (task_id, step_no),
    KEY idx_task_steps_round (task_id, round_no),
    CONSTRAINT fk_task_steps_task FOREIGN KEY (task_id) REFERENCES tasks (task_id) ON DELETE CASCADE,
    CONSTRAINT chk_task_steps_status CHECK (status IN ('pending','running','done','failed','skipped'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='决策状态：模型想做什么（可重放）';

-- ---------------------------------------------------------------------------
-- 工具调用：幂等与恢复的核心表
-- 主键就是幂等 key —— "这个操作到底发出去过没有"由主键回答，不靠查日志反推。
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS tool_calls (
    idem_key    CHAR(32)     NOT NULL COMMENT '幂等 key：sha256(task_id:step_no:call_index:tool:规范化参数) 前 32 位',
    task_id     VARCHAR(64)  NOT NULL,
    step_no     INT          NOT NULL,
    call_index  INT          NOT NULL COMMENT '同一 step 内的第几个工具调用（同一轮可并发多个）',
    tool_name   VARCHAR(64)  NOT NULL,
    args        JSON         NOT NULL COMMENT '规范化后的调用参数（规范化保证同逻辑操作产生同 key）',
    tier        TINYINT      NOT NULL COMMENT '0=无害 1=写 2=高危',
    status      VARCHAR(32)  NOT NULL COMMENT 'started/waiting_approval/success/failed/skipped',
    result      JSON         NULL COMMENT '下游返回结果（规范化后）',
    err_class   TINYINT      NULL COMMENT '0=可重试 1=致命 2=参数错 3=熔断打开',
    err_message TEXT         NULL,
    retry_count INT          NOT NULL DEFAULT 0,
    started_at  DATETIME(3)  NOT NULL,
    finished_at DATETIME(3)  NULL,
    PRIMARY KEY (idem_key),
    -- 同一逻辑位置只允许一条执行记录：这是"重放时跳过已成功步骤"的判定依据
    UNIQUE KEY uk_tool_calls_target (task_id, step_no, call_index),
    KEY idx_tool_calls_task_status (task_id, status),
    -- 恢复扫描：悬挂的 started（进程死在调用下游的中间态）、待审批的记录
    KEY idx_tool_calls_recovery (status, started_at),
    CONSTRAINT fk_tool_calls_task FOREIGN KEY (task_id) REFERENCES tasks (task_id) ON DELETE CASCADE,
    CONSTRAINT chk_tool_calls_status CHECK (status IN ('started','waiting_approval','success','failed','skipped'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='执行状态：下游做到哪了（幂等 key 为主键，与审计日志分离）';

-- ---------------------------------------------------------------------------
-- 审批工单
-- 审批不是模型的判断，是工具配置触发后生成的一张"待办"。
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS approvals (
    ticket_id  VARCHAR(64) NOT NULL,
    idem_key   CHAR(32)    NOT NULL COMMENT '关联的工具调用幂等 key',
    task_id    VARCHAR(64) NOT NULL,
    step_no    INT         NOT NULL,
    tool_name  VARCHAR(64) NOT NULL,
    args       JSON        NOT NULL,
    preview    TEXT        NULL COMMENT 'dry-run 预览：审批人看到的必须是"将要发生什么"，不是原始参数',
    status     VARCHAR(32) NOT NULL COMMENT 'pending/approved/rejected/expired',
    approver   VARCHAR(64) NULL,
    reason     TEXT        NULL COMMENT '拒绝理由会回喂给模型，让它给出替代方案',
    expires_at DATETIME(3) NOT NULL COMMENT '过期即视为拒绝：挂起不能无限期占用（默认 TTL 由工具配置决定）',
    decided_at DATETIME(3) NULL,
    created_at DATETIME(3) NOT NULL,
    PRIMARY KEY (ticket_id),
    -- 同一幂等 key 只允许一张工单：恢复流程重入时不会重复建单，这是恢复路径的幂等保证
    UNIQUE KEY uk_approvals_idem (idem_key),
    KEY idx_approvals_pending (status, expires_at),
    CONSTRAINT chk_approvals_status CHECK (status IN ('pending','approved','rejected','expired'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='审批工单：挂起与恢复的交接点';

-- ---------------------------------------------------------------------------
-- 审计日志：只增不改，回答"谁在什么时候做了什么"
-- 刻意与 tool_calls 分离：审计是合规用途，执行状态是恢复用途，混表会导致
-- 恢复逻辑依赖日志反推。也刻意不对 tasks 建外键——任务被删除不能抹掉审计痕迹。
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS audit_logs (
    id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    task_id    VARCHAR(64)  NULL COMMENT '可空：鉴权失败、工单审批等行为可能不属于任何任务',
    actor      VARCHAR(64)  NOT NULL COMMENT 'user:<id> / approver:<id> / system',
    action     VARCHAR(64)  NOT NULL COMMENT 'task.created / tool_call.requested / approval.approved / ...',
    target     VARCHAR(255) NULL COMMENT '被操作对象，如 payment-service',
    detail     JSON         NULL,
    trace_id   VARCHAR(64)  NULL COMMENT '与链路追踪对齐，便于从一条审计翻到完整执行轨迹',
    created_at DATETIME(3)  NOT NULL,
    PRIMARY KEY (id),
    KEY idx_audit_task_time (task_id, created_at),
    KEY idx_audit_actor_time (actor, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='审计日志（append-only，永不级联删除）';
