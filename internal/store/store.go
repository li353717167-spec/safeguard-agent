// Package store 负责与 MySQL 的交互：连接池与 ORM 装配、schema 迁移，以及后续
// （D5/D6/D7）的任务状态、工具调用、审批工单仓储。
//
// 四条设计原则：
//  1. 业务代码不直接持有 *gorm.DB 或 *sql.DB，统一通过 Store 访问——以后换实现或注入
//     测试替身时，改的是一处而不是到处。
//  2. 连接参数的安全默认值在 Open 里强制生效（parseTime=true、UTC、TLS、禁用多语句），
//     而不是靠每个环境把 DSN 写对。配置漂移是生产事故的常见来源。
//  3. 迁移走独立连接（multiStatements=true），主连接池永远不开多语句——
//     否则一处 SQL 注入就能被放大成多语句执行。
//  4. **ORM 只做数据访问，schema 归版本化 SQL 迁移**（见 models.go 顶部说明）：
//     AutoMigrate 表达不了 CHECK 约束与索引注释，也没有版本与 checksum 概念。
package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// Config 是数据库连接的装配参数。
type Config struct {
	DSN string
	// 以下的 0 值表示"用默认值"，见 applyDefaults。
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
	PingTimeout     time.Duration
	// Debug 打开后把每条 SQL 注入 slog（Debug 级别）。默认只记录慢查询与错误。
	Debug bool
}

func (c *Config) applyDefaults() {
	if c.MaxOpenConns <= 0 {
		c.MaxOpenConns = 10
	}
	if c.MaxIdleConns <= 0 {
		c.MaxIdleConns = 5
	}
	if c.ConnMaxLifetime <= 0 {
		c.ConnMaxLifetime = 30 * time.Minute
	}
	if c.ConnMaxIdleTime <= 0 {
		c.ConnMaxIdleTime = 5 * time.Minute
	}
	if c.PingTimeout <= 0 {
		c.PingTimeout = 5 * time.Second
	}
}

// Store 是数据库访问的入口。
type Store struct {
	// gorm 是数据访问句柄：所有 CRUD 都走它。
	gorm *gorm.DB
	// db 是 GORM 底下的连接池，供连接池调优与迁移器使用。
	db *sql.DB
	// rawDSN 保留原始 DSN：掩码只应作用于"输出"，不应作用于"连接"。
	rawDSN string
	// safeDSN 是掩码后的 DSN，只用于日志：任何地方都不要打印带密码的原始 DSN。
	safeDSN string
}

// Open 解析 DSN、强制安全参数、装配 ORM 与连接池，并做一次连通性探测。
func Open(ctx context.Context, cfg Config) (*Store, error) {
	cfg.applyDefaults()

	// DSN 仍由本项目自己归一化：TLS、UTC、parseTime 这些与"用不用 ORM"无关，
	// 而 GORM 的 mysql 驱动同样依赖 parseTime=true 才能把 DATETIME 扫成 time.Time。
	dsn, err := normalizeDSN(cfg.DSN, false)
	if err != nil {
		return nil, err
	}

	gdb, err := gorm.Open(gormmysql.Open(dsn), gormOptions(cfg.Debug))
	if err != nil {
		return nil, fmt.Errorf("store: 打开数据库失败（%s）: %w", MaskDSN(cfg.DSN), err)
	}

	db, err := gdb.DB()
	if err != nil {
		return nil, fmt.Errorf("store: 获取底层连接池失败: %w", err)
	}
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	db.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)

	pingCtx, cancel := context.WithTimeout(ctx, cfg.PingTimeout)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: 数据库不可达（%s）: %w", MaskDSN(cfg.DSN), err)
	}

	return &Store{gorm: gdb, db: db, rawDSN: cfg.DSN, safeDSN: MaskDSN(cfg.DSN)}, nil
}

// gormOptions 把 GORM 的行为钉死在本项目的不变量上。
//
// 这里每一行都是在防一类"默认行为悄悄违背设计"的事故：
//   - NowFunc：GORM 默认 time.Now()（本地时区），会破坏"时间统一 UTC"这条不变量；
//     缺了它，审计时间线会在时区漂移时自相矛盾。
//   - IgnoreRecordNotFoundError：查不到记录是正常业务结果，不该被记成 error。
//   - ParameterizedQueries：SQL 日志里不打印参数值——与 MaskDSN 同一个原则，
//     日志不该成为泄露通道（工具参数里可能有配置内容、审批理由）。
func gormOptions(debug bool) *gorm.Config {
	level := gormlogger.Warn
	if debug {
		level = gormlogger.Info
	}
	return &gorm.Config{
		NowFunc: utcNow,
		Logger: gormlogger.New(slogWriter{}, gormlogger.Config{
			SlowThreshold:             200 * time.Millisecond,
			LogLevel:                  level,
			IgnoreRecordNotFoundError: true,
			ParameterizedQueries:      true,
			Colorful:                  false,
		}),
	}
}

// utcNow 是注入给 GORM 的时钟：一切自动时间戳（created_at / updated_at）都用 UTC。
func utcNow() time.Time { return time.Now().UTC() }

// slogWriter 把 GORM 的日志接进 slog，避免日志里出现两套时间格式与两个输出流。
type slogWriter struct{}

func (slogWriter) Printf(format string, args ...any) {
	slog.Debug(strings.TrimSpace(fmt.Sprintf(format, args...)), "component", "gorm")
}

// GORM 返回 ORM 句柄。后续所有仓储（任务、工具调用、审批、审计）都从这里取。
func (s *Store) GORM() *gorm.DB { return s.gorm }

// DB 暴露底层连接池，供连接池调优、迁移器与少数必须手写 SQL 的场景（如 D7 的
// SELECT ... FOR UPDATE 恢复扫描）使用。业务层的日常读写请优先用 GORM。
func (s *Store) DB() *sql.DB { return s.db }

// SafeDSN 返回掩码后的 DSN，可安全写入日志。
func (s *Store) SafeDSN() string { return s.safeDSN }

// Ping 探测连通性。
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Stats 返回连接池运行指标，用于可观测性（池子打满前要能看见）。
func (s *Store) Stats() sql.DBStats { return s.db.Stats() }

// Close 关闭连接池。
func (s *Store) Close() error { return s.db.Close() }

// normalizeDSN 校验 DSN 并补齐安全参数。
//
// 这里刻意**不做** "ParseDSN → FormatDSN" 的往返：FormatDSN 会对 user/password
// 做 URL 转义，而 ParseDSN 不会反转义，于是一个含 @ 的密码（本项目部署时正踩到这一点）
// 会在往返中变成 p%40ssw0rd —— 连接时用的就是错的密码，报错还只是
// "Access denied"，排查成本极高。所以只重写 query 部分，用户信息与地址原样保留。
func normalizeDSN(raw string, multiStatements bool) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("store: DSN 为空，形如 user:password@tcp(127.0.0.1:3306)/safeguard_agent")
	}
	// ParseDSN 只用来校验格式与提前暴露错误，不用它的输出。
	if _, err := mysql.ParseDSN(raw); err != nil {
		return "", fmt.Errorf("store: DSN 解析失败: %w", err)
	}

	base, query, _ := strings.Cut(raw, "?")
	params, err := url.ParseQuery(query)
	if err != nil {
		return "", fmt.Errorf("store: DSN 查询参数解析失败: %w", err)
	}
	setDefault := func(key, value string) {
		if _, ok := params[key]; !ok {
			params.Set(key, value)
		}
	}

	// 时间语义：读回来就是 time.Time，且一律按 UTC 解释。
	setDefault("parseTime", "true")
	setDefault("loc", "UTC")
	// 会话时区固定 UTC：DATETIME 不带时区，写入方与读取方必须约定同一基准。
	setDefault("time_zone", "'+00:00'")
	// 建连超时，避免网络黑洞把启动挂死。
	setDefault("timeout", "5s")
	// MySQL 8 默认 caching_sha2_password：首次认证（服务端缓存未命中）需要 full auth，
	// 明文连接下客户端必须向服务器索取 RSA 公钥——有些服务器（实测 NAS 上的 MySQL 8.0.32）
	// 根本不下发，表现就是"密码没错却一直 Access denied"。走 TLS 后可直接发送密码，
	// 绕开整条 RSA 依赖，所以这里默认 tls=preferred（服务器支持就用，不支持则回退）。
	setDefault("tls", "preferred")
	// 多语句由调用方按连接用途钉死：迁移连接开、主连接池关。
	params.Set("multiStatements", strconv.FormatBool(multiStatements))

	return base + "?" + params.Encode(), nil
}

// MaskDSN 把 DSN 中的密码替换为 ****，用于日志与错误信息。
func MaskDSN(raw string) string {
	cfg, err := mysql.ParseDSN(raw)
	if err != nil {
		return "<invalid dsn>"
	}
	if cfg.Passwd != "" {
		cfg.Passwd = "****"
	}
	return cfg.FormatDSN()
}
