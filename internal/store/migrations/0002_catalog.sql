-- 0002_catalog.sql · 服务目录（只读工具的数据源）
--
-- 这三张表不是"演示用的假数据表"，而是工具的真实数据源：
-- query_metrics / query_config / list_services 都从这里读。
--
-- 为什么 schema 在迁移里、数据却用 `-seed` 另灌（见 store/seed.go）：
--   1) 迁移只应该改结构。把数据塞进迁移，等于让每套环境的业务数据都由版本号决定；
--   2) 指标是时间序列，演示与评测时"最近 5 分钟"必须永远有数据——需要能随时重灌，
--      而迁移只会执行一次。
-- 因此：迁移负责"表存在"，seed 负责"有数据"，两者都可重复执行。

-- ---------------------------------------------------------------------------
-- 服务目录
-- 注意 criticality 是**服务的重要级别**，与工具的 Tier（风险等级）是两个概念：
-- 一个"核心服务"上的只读查询仍然是 TierHarmless。
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS services (
    name        VARCHAR(64)  NOT NULL COMMENT '服务名',
    criticality TINYINT      NOT NULL COMMENT '0=核心 1=重要 2=普通',
    owner       VARCHAR(64)  NOT NULL COMMENT '归属团队',
    status      VARCHAR(32)  NOT NULL COMMENT 'healthy/degraded/down',
    description VARCHAR(255) NULL,
    created_at  DATETIME(3)  NOT NULL,
    updated_at  DATETIME(3)  NOT NULL,
    PRIMARY KEY (name),
    KEY idx_services_status (status),
    CONSTRAINT chk_services_criticality CHECK (criticality IN (0, 1, 2)),
    CONSTRAINT chk_services_status CHECK (status IN ('healthy', 'degraded', 'down'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='服务目录：list_services 的数据源';

-- ---------------------------------------------------------------------------
-- 服务配置项
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS service_configs (
    service      VARCHAR(64)  NOT NULL,
    config_key   VARCHAR(64)  NOT NULL,
    config_value VARCHAR(255) NOT NULL,
    updated_at   DATETIME(3)  NOT NULL,
    PRIMARY KEY (service, config_key),
    CONSTRAINT fk_service_configs_service FOREIGN KEY (service) REFERENCES services (name) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='服务配置：query_config 的数据源';

-- ---------------------------------------------------------------------------
-- 服务指标（时间序列）
-- window_start 是该采样点代表的时间窗口起点；查询时按 [now-window, now] 过滤。
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS service_metrics (
    service      VARCHAR(64) NOT NULL,
    metric       VARCHAR(32) NOT NULL COMMENT 'error_rate / qps / p99_latency',
    window_start DATETIME(3) NOT NULL,
    value        DOUBLE      NOT NULL,
    PRIMARY KEY (service, metric, window_start),
    -- 时间序列查询的主路径：按服务 + 指标 + 时间倒序取最近几个点
    KEY idx_service_metrics_lookup (service, metric, window_start DESC),
    CONSTRAINT fk_service_metrics_service FOREIGN KEY (service) REFERENCES services (name) ON DELETE CASCADE,
    CONSTRAINT chk_service_metrics_metric CHECK (metric IN ('error_rate', 'qps', 'p99_latency')),
    CONSTRAINT chk_service_metrics_value CHECK (value >= 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='服务指标时间序列：query_metrics 的数据源';
