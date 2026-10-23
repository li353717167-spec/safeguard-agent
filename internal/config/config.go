// Package config 负责把运行期配置从环境变量装载进来。
//
// 两条原则：
//  1. 密钥只走环境变量（或本地 .env），绝不写进代码、配置文件或数据库。
//  2. 配置装载只做"形状解析"，必需项由使用方校验——因为 `-migrate` 不需要模型 key，
//     `-prompt` 不需要数据库。强制要求用不到的配置项，是本地开发最常见的摩擦来源。
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// 环境变量前缀，统一避免和其他工具撞名。
const envPrefix = "SAFEGUARD_"

// Config 是全部运行期配置。
type Config struct {
	LLM      LLM
	DB       DB
	Context  Context
	LogLevel string
}

// LLM 是模型侧配置。
type LLM struct {
	BaseURL string
	APIKey  string
	Model   string
	Timeout time.Duration
}

// Context 是上下文预算配置（发给模型的内容体积）。
type Context struct {
	// MaxTokens 是估算上限：超了先裁剪工具结果、再折叠早期轮次。
	MaxTokens int
	// KeepRecentRounds 是永不折叠的最近轮数。
	KeepRecentRounds int
	// MaxToolResultBytes 是单条工具结果保留的最大字节数。
	MaxToolResultBytes int
}

// DB 是数据库侧配置。
type DB struct {
	DSN             string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

// Load 从环境变量装载配置，缺省项使用默认值。
func Load() (Config, error) {
	timeout, err := durationEnv("LLM_TIMEOUT", 60*time.Second)
	if err != nil {
		return Config{}, err
	}
	maxOpen, err := intEnv("DB_MAX_OPEN_CONNS", 10)
	if err != nil {
		return Config{}, err
	}
	maxIdle, err := intEnv("DB_MAX_IDLE_CONNS", 5)
	if err != nil {
		return Config{}, err
	}
	connLife, err := durationEnv("DB_CONN_MAX_LIFETIME", 30*time.Minute)
	if err != nil {
		return Config{}, err
	}
	ctxTokens, err := intEnv("CONTEXT_MAX_TOKENS", 6000)
	if err != nil {
		return Config{}, err
	}
	ctxKeep, err := intEnv("CONTEXT_KEEP_ROUNDS", 2)
	if err != nil {
		return Config{}, err
	}
	ctxToolBytes, err := intEnv("CONTEXT_MAX_TOOL_BYTES", 2000)
	if err != nil {
		return Config{}, err
	}

	return Config{
		LLM: LLM{
			BaseURL: stringEnv("LLM_BASE_URL", "https://api.deepseek.com/v1"),
			APIKey:  strings.TrimSpace(os.Getenv(envPrefix + "LLM_API_KEY")),
			Model:   stringEnv("LLM_MODEL", "deepseek-chat"),
			Timeout: timeout,
		},
		DB: DB{
			DSN:             strings.TrimSpace(os.Getenv(envPrefix + "DB_DSN")),
			MaxOpenConns:    maxOpen,
			MaxIdleConns:    maxIdle,
			ConnMaxLifetime: connLife,
		},
		Context: Context{
			MaxTokens:          ctxTokens,
			KeepRecentRounds:   ctxKeep,
			MaxToolResultBytes: ctxToolBytes,
		},
		LogLevel: stringEnv("LOG_LEVEL", "info"),
	}, nil
}

// Validate 校验模型侧配置。只在真正要调用模型时调用。
//
// APIKey 刻意不校验：本地 llama.cpp / vLLM / LM Studio 与内网网关通常不校验密钥，
// 把 key 当必填会让"先跑通本地模型"这件事无谓地卡住。
func (l LLM) Validate() error {
	if l.BaseURL == "" {
		return fmt.Errorf("%sLLM_BASE_URL 不能为空", envPrefix)
	}
	if l.Model == "" {
		return fmt.Errorf("%sLLM_MODEL 不能为空", envPrefix)
	}
	return nil
}

// Validate 校验数据库配置。只在真正要访问数据库时调用。
func (d DB) Validate() error {
	if d.DSN == "" {
		return fmt.Errorf("缺少 %sDB_DSN，形如 safeguard:safeguard@tcp(127.0.0.1:3306)/safeguard_agent", envPrefix)
	}
	return nil
}

// SlogLevel 把配置里的字符串转成 slog 级别。
func (c Config) SlogLevel() (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(c.LogLevel)) {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("%sLOG_LEVEL=%q 不合法（debug/info/warn/error）", envPrefix, c.LogLevel)
	}
}

func stringEnv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(envPrefix + key)); v != "" {
		return v
	}
	return def
}

func intEnv(key string, def int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(envPrefix + key))
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s%s=%q 不是合法整数: %w", envPrefix, key, raw, err)
	}
	if v < 0 {
		return 0, fmt.Errorf("%s%s=%d 不能为负数", envPrefix, key, v)
	}
	return v, nil
}

func durationEnv(key string, def time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(envPrefix + key))
	if raw == "" {
		return def, nil
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s%s=%q 不是合法时长（如 60s / 2m）: %w", envPrefix, key, raw, err)
	}
	if v <= 0 {
		return 0, fmt.Errorf("%s%s=%s 必须为正数", envPrefix, key, v)
	}
	return v, nil
}
