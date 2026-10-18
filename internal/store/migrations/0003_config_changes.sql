-- 0003_config_changes.sql · 配置变更台账（写工具的下游幂等落点）
--
-- 为什么需要这张表：D5 的幂等是"本地状态表 + 下游幂等"双保险。
-- 本地那张表在 tool_calls；而**下游**（这里就是我们的服务目录）也要能识别重复。
-- update_config 执行时把幂等 key 透传到下游：同一个 key 再来一次，这里不会重复记一行，
-- service_configs 也不会被重复改一次。
--
-- 刻意不建外键：变更台账比服务目录活得久——服务被删了，"谁在什么时候把它改成过什么"
-- 仍然需要留下证据（和 audit_logs 同一套哲学）。

CREATE TABLE IF NOT EXISTS config_changes (
    idem_key   CHAR(32)     NOT NULL COMMENT '幂等 key：同一个 key 只允许生效一次',
    service    VARCHAR(64)  NOT NULL,
    config_key VARCHAR(64)  NOT NULL,
    old_value  VARCHAR(255) NULL COMMENT '改之前的值（新建配置时为空）',
    new_value  VARCHAR(255) NOT NULL,
    applied_at DATETIME(3)  NOT NULL,
    PRIMARY KEY (idem_key),
    KEY idx_config_changes_service (service, applied_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='配置变更台账：下游幂等的事实来源';
