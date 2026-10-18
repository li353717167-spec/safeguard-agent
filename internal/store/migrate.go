package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"
)

// 迁移文件编译进二进制：部署时不需要额外同步 .sql 文件，
// 也不会出现"代码版本和 schema 版本对不上"这种最常见的事故。
//
//go:embed migrations/*.sql
var migrationFS embed.FS

const (
	migrationsDir     = "migrations"
	migrationLockName = "safeguard_agent.schema_migration"
	// 等待迁移锁的秒数：多实例同时启动时，只有一个实例真正执行 DDL。
	migrationLockWaitSeconds = 10
)

// Migration 是一个已加载的迁移文件。
type Migration struct {
	Version  string // 文件名的数字前缀，例如 "0001"
	Name     string // 文件名，例如 "0001_init.sql"
	Body     string
	Checksum string // sha256(body)，用来发现"已应用的迁移被改过"
}

// MigrationResult 汇报本轮迁移做了什么，供启动日志与运维判断。
type MigrationResult struct {
	Applied []string // 本次真正执行的版本
	Skipped []string // 已应用且 checksum 一致，跳过
	Missing []string // 数据库里有记录但仓库里已没有对应文件（schema 漂移信号）
}

// LoadMigrations 读取并校验编译进来的迁移文件，按版本号升序返回。
func LoadMigrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFS, migrationsDir)
	if err != nil {
		return nil, fmt.Errorf("store: 读取迁移目录失败: %w", err)
	}

	out := make([]Migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		body, err := migrationFS.ReadFile(migrationsDir + "/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("store: 读取迁移文件 %s 失败: %w", e.Name(), err)
		}
		version, _, ok := strings.Cut(e.Name(), "_")
		if !ok || version == "" {
			return nil, fmt.Errorf("store: 迁移文件 %s 命名不合法，要求 <版本号>_<描述>.sql", e.Name())
		}
		sum := sha256.Sum256(body)
		out = append(out, Migration{
			Version:  version,
			Name:     e.Name(),
			Body:     string(body),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}
	if len(out) == 0 {
		return nil, errors.New("store: 没有找到任何迁移文件")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// Migrate 把数据库 schema 对齐到当前二进制内置的版本。
//
// 为什么不用 goose / golang-migrate：本项目的迁移需求只有三条——
// 按版本顺序、幂等可重入、改过的迁移必须报错。自己实现 100 行就能覆盖，
// 而且能讲清楚每一步为什么这么写；引入框架反而要额外解释"框架帮我做了什么"。
//
// MySQL 的 DDL 会隐式提交，事务包不住 DDL，所以这里不假装有原子性：
// 失败可能留下半套对象，因此所有 DDL 都必须写成 IF NOT EXISTS 这种幂等形式，
// 修完问题直接重跑即可。
func (s *Store) Migrate(ctx context.Context) (MigrationResult, error) {
	migrations, err := LoadMigrations()
	if err != nil {
		return MigrationResult{}, err
	}

	dsn, err := normalizeDSN(s.rawDSN, true)
	if err != nil {
		return MigrationResult{}, err
	}

	// 迁移用独立连接：只有它开启 multiStatements，用完即关。
	migDB, err := sql.Open("mysql", dsn)
	if err != nil {
		return MigrationResult{}, fmt.Errorf("store: 打开迁移连接失败: %w", err)
	}
	defer migDB.Close()
	migDB.SetMaxOpenConns(1)

	// GET_LOCK 是会话级锁，必须固定在同一条连接上取放，
	// 否则从连接池里换一条连接去 RELEASE_LOCK 会释放不掉（或者更糟：释放了别人的锁）。
	conn, err := migDB.Conn(ctx)
	if err != nil {
		return MigrationResult{}, fmt.Errorf("store: 获取迁移连接失败: %w", err)
	}
	defer conn.Close()

	var locked sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)", migrationLockName, migrationLockWaitSeconds).Scan(&locked); err != nil {
		return MigrationResult{}, fmt.Errorf("store: 申请迁移锁失败: %w", err)
	}
	if !locked.Valid || locked.Int64 != 1 {
		return MigrationResult{}, fmt.Errorf("store: 等待迁移锁超时（%ds），可能有其他实例正在迁移", migrationLockWaitSeconds)
	}
	defer func() {
		// 用独立的 context：即使调用方的 ctx 已取消，也必须把锁还回去。
		releaseCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(releaseCtx, "SELECT RELEASE_LOCK(?)", migrationLockName)
	}()

	if _, err := conn.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    VARCHAR(64) NOT NULL,
    name       VARCHAR(255) NOT NULL,
    checksum   CHAR(64)    NOT NULL,
    applied_at DATETIME(3) NOT NULL,
    PRIMARY KEY (version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='迁移记录：checksum 用来发现已应用的迁移被改动'`); err != nil {
		return MigrationResult{}, fmt.Errorf("store: 创建 schema_migrations 失败: %w", err)
	}

	applied, err := loadApplied(ctx, conn)
	if err != nil {
		return MigrationResult{}, err
	}

	var res MigrationResult
	known := make(map[string]struct{}, len(migrations))
	for _, m := range migrations {
		known[m.Version] = struct{}{}
		if prev, ok := applied[m.Version]; ok {
			if prev != m.Checksum {
				return res, fmt.Errorf(
					"store: 迁移 %s 已应用但内容被修改（库内 checksum %s，当前文件 %s）：已应用的迁移不允许修改，请新增一个迁移文件",
					m.Name, prev[:12], m.Checksum[:12])
			}
			res.Skipped = append(res.Skipped, m.Version)
			continue
		}

		started := time.Now()
		if _, err := conn.ExecContext(ctx, m.Body); err != nil {
			return res, fmt.Errorf("store: 迁移 %s 执行失败（MySQL DDL 无法回滚，修好后可重跑，DDL 均为幂等写法）: %w", m.Name, err)
		}
		if _, err := conn.ExecContext(ctx,
			"INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)",
			m.Version, m.Name, m.Checksum, time.Now().UTC(),
		); err != nil {
			return res, fmt.Errorf("store: 写入迁移记录 %s 失败: %w", m.Version, err)
		}
		res.Applied = append(res.Applied, fmt.Sprintf("%s(%s)", m.Version, time.Since(started).Round(time.Millisecond)))
	}

	for version := range applied {
		if _, ok := known[version]; !ok {
			res.Missing = append(res.Missing, version)
		}
	}
	sort.Strings(res.Missing)
	return res, nil
}

func loadApplied(ctx context.Context, conn *sql.Conn) (map[string]string, error) {
	rows, err := conn.QueryContext(ctx, "SELECT version, checksum FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("store: 读取迁移记录失败: %w", err)
	}
	defer rows.Close()

	applied := map[string]string{}
	for rows.Next() {
		var version, checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			return nil, fmt.Errorf("store: 解析迁移记录失败: %w", err)
		}
		applied[version] = checksum
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历迁移记录失败: %w", err)
	}
	return applied, nil
}
