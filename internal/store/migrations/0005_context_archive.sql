-- 0005_context_archive.sql · 上下文归档
--
-- 压缩会"删掉/降级"历史消息，但模型之后可能需要细节（"刚才那个 95 分位的数值是多少"）。
-- 所以压缩不能只丢，还要**留一份原文 + 一个短引用**：
--   - 摘要行尾带 [ctx:xxxxxxxx]；
--   - 模型可以调用 recall_context 用 ref 取回、或用关键词搜索。
--
-- 这里存的是"执行状态"而不是审计：任务删掉时跟着清理（ON DELETE CASCADE），
-- 这与 audit_logs 的"只增不删"是两套不同的语义，别混。

CREATE TABLE IF NOT EXISTS context_archive (
    archive_id VARCHAR(64)  NOT NULL COMMENT '短引用（ctx_xxxxxxxx），摘要与裁剪说明里回指它',
    task_id    VARCHAR(64)  NOT NULL,
    round_no   INT          NOT NULL COMMENT '产生该内容时的轮次',
    role       VARCHAR(16)  NOT NULL COMMENT 'user/assistant/tool',
    tool_name  VARCHAR(64)  NULL,
    content    MEDIUMTEXT   NOT NULL COMMENT '原始内容（未压缩）',
    tokens     INT          NOT NULL COMMENT '估算 token，便于回答"这条多大"',
    created_at DATETIME(3)  NOT NULL,
    PRIMARY KEY (archive_id),
    -- 同一个任务里按轮次扫；ref 是主键，重复归档自动忽略
    KEY idx_context_archive_task (task_id, round_no),
    CONSTRAINT fk_context_archive_task FOREIGN KEY (task_id) REFERENCES tasks (task_id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_ai_ci
  COMMENT = '压缩归档：被裁剪/折叠的原始上下文，供 recall_context 回查';
