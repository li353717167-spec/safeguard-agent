package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func TestMaskDSNNeverLeaksPassword(t *testing.T) {
	const raw = "safeguard:sup3r-secret@tcp(127.0.0.1:3306)/safeguard_agent"
	masked := MaskDSN(raw)

	if strings.Contains(masked, "sup3r-secret") {
		t.Errorf("掩码后仍含明文密码: %s", masked)
	}
	if !strings.Contains(masked, "****") {
		t.Errorf("掩码结果应含 ****: %s", masked)
	}
	if !strings.Contains(masked, "127.0.0.1:3306") {
		t.Errorf("主机信息不应被掩掉（排障要靠它）: %s", masked)
	}

	if got := MaskDSN("这不是一个 DSN"); got != "<invalid dsn>" {
		t.Errorf("非法 DSN 的掩码结果 = %q, 期望 <invalid dsn>", got)
	}
}

// TestNormalizeDSNPreservesPasswordWithAtSign 钉住一个真实踩过的坑。
//
// 最初的实现是 ParseDSN → FormatDSN 往返。FormatDSN 会对 user/password 做 URL 转义，
// 而 ParseDSN 不会反转义，于是密码 p@ssw0rd 会被写成 p%40ssw0rd，
// 连接时用的就是错的密码，而服务器只会回一句 "Access denied"，
// 从报错完全看不出是编码问题。这个用例就是防止有人"顺手优化"回那种写法。
func TestNormalizeDSNPreservesPasswordWithAtSign(t *testing.T) {
	const raw = "appuser:p@ssw0rd@tcp(127.0.0.1:3306)/appdb"

	for _, multi := range []bool{false, true} {
		out, err := normalizeDSN(raw, multi)
		if err != nil {
			t.Fatalf("normalizeDSN(multi=%v) 失败: %v", multi, err)
		}
		cfg, err := mysql.ParseDSN(out)
		if err != nil {
			t.Fatalf("归一化后的 DSN 无法被驱动解析: %v", err)
		}
		if cfg.User != "appuser" {
			t.Errorf("User = %q, 期望 appuser", cfg.User)
		}
		if cfg.Passwd != "p@ssw0rd" {
			t.Errorf("密码在归一化过程中被改动了: %q（FormatDSN 的 URL 转义陷阱）", cfg.Passwd)
		}
		if cfg.Addr != "127.0.0.1:3306" {
			t.Errorf("Addr = %q, 期望 127.0.0.1:3306", cfg.Addr)
		}
		if cfg.DBName != "appdb" {
			t.Errorf("DBName = %q, 期望 appdb", cfg.DBName)
		}
		if cfg.MultiStatements != multi {
			t.Errorf("MultiStatements = %v, 期望 %v", cfg.MultiStatements, multi)
		}
	}

	// TLS 默认打开：MySQL 8 的 caching_sha2_password 在缓存未命中时需要 full auth，
	// 明文连接下要依赖服务器下发 RSA 公钥，实测有服务器（MySQL 8.0.32 on NAS）不下发。
	out, _ := normalizeDSN(raw, false)
	if !strings.Contains(out, "tls=preferred") {
		t.Errorf("DSN 未默认开启 TLS: %s", out)
	}
	cfg, _ := mysql.ParseDSN(out)
	if cfg.TLS == nil {
		t.Error("tls=preferred 应让驱动持有 TLS 配置，否则缓存未命中时会认证失败")
	}
}

func TestNormalizeDSNRespectsExplicitTLS(t *testing.T) {
	out, err := normalizeDSN("u:p@tcp(127.0.0.1:3306)/db?tls=skip-verify", false)
	if err != nil {
		t.Fatalf("normalizeDSN 失败: %v", err)
	}
	if !strings.Contains(out, "tls=skip-verify") {
		t.Errorf("显式配置的 tls 参数被覆盖了: %s", out)
	}
}

func TestNormalizeDSNForcesSafeDefaults(t *testing.T) {
	const raw = "u:p@tcp(127.0.0.1:3306)/db"

	// 迁移连接：需要多语句
	out, err := normalizeDSN(raw, true)
	if err != nil {
		t.Fatalf("normalizeDSN 失败: %v", err)
	}
	cfg, err := mysql.ParseDSN(out)
	if err != nil {
		t.Fatalf("归一化后的 DSN 无法被驱动解析: %v", err)
	}
	if !cfg.ParseTime {
		t.Error("parseTime 必须被强制打开：DATETIME 要扫描成 time.Time")
	}
	if cfg.Loc != time.UTC {
		t.Errorf("Loc = %v, 期望 UTC", cfg.Loc)
	}
	if !cfg.MultiStatements {
		t.Error("迁移连接需要 multiStatements")
	}
	if got := cfg.Params["time_zone"]; got != "'+00:00'" {
		t.Errorf("time_zone = %q, 期望 '+00:00'", got)
	}
	if cfg.Timeout != 5*time.Second {
		t.Errorf("Timeout = %s, 期望默认 5s（避免网络黑洞把启动挂死）", cfg.Timeout)
	}

	// 主连接池：绝不能开多语句，否则一处 SQL 注入会被放大成多语句执行
	outMain, err := normalizeDSN(raw, false)
	if err != nil {
		t.Fatalf("normalizeDSN 失败: %v", err)
	}
	cfgMain, err := mysql.ParseDSN(outMain)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cfgMain.MultiStatements {
		t.Error("主连接池不应开启 multiStatements")
	}

	// 用户显式配置的会话时区应被尊重，而不是被默认值覆盖
	const withTZ = "u:p@tcp(127.0.0.1:3306)/db?time_zone=%27%2B08%3A00%27"
	outTZ, err := normalizeDSN(withTZ, false)
	if err != nil {
		t.Fatalf("normalizeDSN 失败: %v", err)
	}
	cfgTZ, err := mysql.ParseDSN(outTZ)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got := cfgTZ.Params["time_zone"]; got != "'+08:00'" {
		t.Errorf("显式 time_zone 被覆盖了: %q", got)
	}
}

func TestOpenRejectsBadDSNWithoutTouchingNetwork(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
	}{
		{name: "空 DSN", dsn: ""},
		{name: "非法 DSN", dsn: "这不是一个合法的 DSN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 必须在解析阶段就失败：不能等到建连超时（5s 或更久）才暴露配置错误。
			if _, err := Open(context.Background(), Config{DSN: tc.dsn}); err == nil {
				t.Fatal("期望报错")
			}
		})
	}
}

func TestLoadMigrations(t *testing.T) {
	migrations, err := LoadMigrations()
	if err != nil {
		t.Fatalf("LoadMigrations 失败: %v", err)
	}
	if len(migrations) == 0 {
		t.Fatal("没有加载到任何迁移文件（embed 是否生效？）")
	}

	first := migrations[0]
	if first.Version != "0001" {
		t.Errorf("首个迁移版本 = %q, 期望 0001", first.Version)
	}
	if first.Name != "0001_init.sql" {
		t.Errorf("首个迁移文件名 = %q", first.Name)
	}
	if len(first.Checksum) != 64 {
		t.Errorf("checksum 长度 = %d, 期望 sha256 的 64 位十六进制", len(first.Checksum))
	}

	// 版本号必须严格升序：乱序执行会让"新迁移依赖旧迁移"的假设失效。
	for i := 1; i < len(migrations); i++ {
		if migrations[i-1].Version >= migrations[i].Version {
			t.Errorf("迁移未按版本升序排列: %s >= %s", migrations[i-1].Version, migrations[i].Version)
		}
	}
}

// TestMigrationKeepsIdempotencyInvariants 是"设计契约测试"。
//
// 下面这几条不是代码风格，而是本项目的核心承诺：
//   - 幂等 key 做主键 → 没有它，"这个操作发出去过没有"就无法一次查询回答；
//   - (task_id, step_no, call_index) 唯一 → 没有它，重放就无法判断哪些步骤已完成；
//   - 每个 CREATE TABLE 都是 IF NOT EXISTS → MySQL 的 DDL 不能回滚，迁移必须可重入；
//   - audit_logs 不建外键 → 任务被删除不能抹掉审计痕迹。
//
// 有人手滑改掉任何一条，这里会立刻失败，而不是等到恢复流程在凌晨三点出问题。
func TestMigrationKeepsIdempotencyInvariants(t *testing.T) {
	migrations, err := LoadMigrations()
	if err != nil {
		t.Fatalf("LoadMigrations 失败: %v", err)
	}
	body := migrations[0].Body

	mustContain := []string{
		"PRIMARY KEY (idem_key)",
		"UNIQUE KEY uk_tool_calls_target (task_id, step_no, call_index)",
		"KEY idx_tool_calls_recovery (status, started_at)",
		"KEY idx_tasks_status_updated (status, updated_at)",
		"UNIQUE KEY uk_approvals_idem (idem_key)",
	}
	for _, want := range mustContain {
		if !strings.Contains(body, want) {
			t.Errorf("0001_init.sql 缺少关键约束: %q", want)
		}
	}

	if n, ifNotExists := strings.Count(body, "CREATE TABLE"), strings.Count(body, "CREATE TABLE IF NOT EXISTS"); n != ifNotExists {
		t.Errorf("有 %d 个 CREATE TABLE 未写成 IF NOT EXISTS（共 %d 个）：MySQL DDL 无法回滚，迁移必须幂等", n-ifNotExists, n)
	}

	idx := strings.Index(body, "CREATE TABLE IF NOT EXISTS audit_logs")
	if idx < 0 {
		t.Fatal("找不到 audit_logs 建表语句")
	}
	auditBlock := body[idx:]
	if strings.Contains(auditBlock, "FOREIGN KEY") {
		t.Error("audit_logs 不应建外键：审计必须比它记录的业务数据活得更久")
	}
}

// TestModelsMatchMigrationTables 是「模型与迁移不许走偏」的契约测试。
//
// 模型只负责数据访问，schema 的唯一事实源是 migrations/*.sql。两边一旦分叉
// （改了表名、漏了模型、加了表没建模型），这里立刻失败——不需要数据库，也不用真跑一次 CRUD。
// 没有它，分叉只会在某个凌晨以 "Error 1146: Table doesn't exist" 的形式暴露。
func TestModelsMatchMigrationTables(t *testing.T) {
	declared := []string{
		"tasks", "task_steps", "tool_calls", "approvals", "audit_logs",
		"services", "service_configs", "service_metrics", "config_changes",
	}

	migrations, err := LoadMigrations()
	if err != nil {
		t.Fatalf("LoadMigrations 失败: %v", err)
	}
	var allSQL strings.Builder
	for _, m := range migrations {
		allSQL.WriteString(m.Body)
	}
	body := allSQL.String()

	models := []any{
		Task{}, TaskStep{}, ToolCall{}, Approval{}, AuditLog{},
		Service{}, ServiceConfig{}, ServiceMetric{}, ConfigChange{},
	}
	seen := map[string]bool{}
	for _, m := range models {
		namer, ok := m.(interface{ TableName() string })
		if !ok {
			t.Fatalf("%T 未实现 TableName()：不允许依赖 GORM 的复数化推断", m)
		}
		name := namer.TableName()
		if seen[name] {
			t.Errorf("表 %q 被多个模型映射", name)
		}
		seen[name] = true
	}

	for _, want := range declared {
		if !strings.Contains(body, "CREATE TABLE IF NOT EXISTS "+want+" (") {
			t.Errorf("迁移里没有声明表 %q（或写法不是 IF NOT EXISTS）", want)
		}
		if !seen[want] {
			t.Errorf("表 %q 没有对应的 GORM 模型：数据访问会分裂成「一半走 ORM、一半手写 SQL」", want)
		}
	}
	for name := range seen {
		found := false
		for _, want := range declared {
			if name == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("模型声明了迁移里不存在的表 %q", name)
		}
	}
}

// TestGORMClockIsUTC 钉住「时间统一 UTC」这条不变量。
// GORM 默认用 time.Now()（本地时区），少了 NowFunc 会让自动时间戳在时区漂移时自相矛盾。
func TestGORMClockIsUTC(t *testing.T) {
	if got := utcNow(); got.Location() != time.UTC {
		t.Errorf("utcNow().Location() = %v, 期望 UTC", got.Location())
	}

	opts := gormOptions(false)
	if opts.NowFunc == nil {
		t.Fatal("gormOptions 未注入 NowFunc：自动时间戳会退回本地时区")
	}
	if opts.NowFunc().Location() != time.UTC {
		t.Error("gormOptions 注入的时钟不是 UTC")
	}
	if opts.Logger == nil {
		t.Error("gormOptions 必须配置 Logger：否则 SQL 日志会绕开 slog 与参数脱敏")
	}
}
