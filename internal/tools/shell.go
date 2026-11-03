package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// ShellRule 是允许清单里的一条：可执行文件名 + 参数前缀。
//
// 用"参数前缀"而不是正则：前缀可预测、可审计，也不会因为正则写错而放行意外的命令。
type ShellRule struct {
	Exec string
	Args []string
}

// ShellMode 决定这个 shell 工具的风险档位。
type ShellMode int

const (
	// ShellRead 是只读档：TierHarmless，不审批，但**不能**碰私钥/配置/凭据。
	ShellRead ShellMode = iota
	// ShellWrite 是变更档：TierDangerous，每次都要人工审批。
	ShellWrite
)

// ShellConfig 是 shell 工具的配置（读/写两档共用）。
type ShellConfig struct {
	Enabled bool
	// ReadOnly 是只读档的允许清单（默认无需审批）。
	ReadOnly []ShellRule
	// Mutating 是变更档的允许清单（每次都要审批）。
	Mutating []ShellRule
	// WorkDir 是固定工作目录（空则用进程当前目录）。
	WorkDir string
	// Timeout 是单次执行超时。
	Timeout time.Duration
	// MaxOutputBytes 是 stdout/stderr 各自的保留上限。
	MaxOutputBytes int
	// MaxArgs 是参数个数上限。
	MaxArgs int
	// MaxCommandLen 是整条命令的字符上限。
	MaxCommandLen int
	// AllowedPathRoots 是"允许出现在参数里的绝对路径"白名单。
	AllowedPathRoots []string
}

// DefaultShellConfig 返回默认配置。
//
// 只读清单刻意短：每一项都是"看一眼系统状态"，不读任何用户文件。
// 变更清单更短：只有明确会改系统、且必须审批的几类。
func DefaultShellConfig() ShellConfig {
	return ShellConfig{
		Enabled: true,
		ReadOnly: []ShellRule{
			{Exec: "df", Args: []string{"-h"}},
			{Exec: "free", Args: []string{"-h"}},
			{Exec: "uptime"},
			{Exec: "hostname"},
			{Exec: "whoami"},
			{Exec: "id"},
			{Exec: "uname", Args: []string{"-a"}},
			{Exec: "journalctl", Args: []string{"-u"}},
			{Exec: "systemctl", Args: []string{"status"}},
			{Exec: "docker", Args: []string{"ps"}},
		},
		Mutating: []ShellRule{
			{Exec: "systemctl", Args: []string{"restart"}},
			{Exec: "systemctl", Args: []string{"start"}},
			{Exec: "systemctl", Args: []string{"stop"}},
			{Exec: "docker", Args: []string{"restart"}},
		},
		Timeout:          10 * time.Second,
		MaxOutputBytes:   8192,
		MaxArgs:          12,
		MaxCommandLen:    512,
		AllowedPathRoots: []string{"/var/log", "/tmp"},
	}
}

// Normalize 补齐零值。
func (c ShellConfig) Normalize() ShellConfig {
	d := DefaultShellConfig()
	if c.Timeout <= 0 {
		c.Timeout = d.Timeout
	}
	if c.MaxOutputBytes <= 0 {
		c.MaxOutputBytes = d.MaxOutputBytes
	}
	if c.MaxArgs <= 0 {
		c.MaxArgs = d.MaxArgs
	}
	if c.MaxCommandLen <= 0 {
		c.MaxCommandLen = d.MaxCommandLen
	}
	if len(c.AllowedPathRoots) == 0 {
		c.AllowedPathRoots = d.AllowedPathRoots
	}
	return c
}

func (c ShellConfig) rulesFor(mode ShellMode) []ShellRule {
	if mode == ShellWrite {
		return c.Mutating
	}
	return c.ReadOnly
}

// shellForbidden 是**壳元字符**黑名单。
//
// 我们根本不走 `sh -c`：命令被切成 argv 后直接 exec。
// 但即使这样也要显式拒绝这些字符——否则用户会误以为 `a; b` 或 `a | b` 能工作，
// 更糟的是将来某次重构真去调了 shell，这些输入就变成了注入。
const shellForbidden = ";|&<>`$(){}\\"

// deniedArgs 是"参数里出现就拒绝"的敏感片段（大小写不敏感）。
//
// 这一层是给**只读档**兜底的：只读命令默认不需要审批，如果它还能读 `~/.ssh/id_rsa`，
// "只读"就成了"免审批地读走所有密钥"。所以私钥、凭据、配置一律拦掉。
var deniedArgs = []string{
	// 私钥与 SSH
	".ssh", "id_rsa", "id_dsa", "id_ecdsa", "id_ed25519", "authorized_keys",
	".gnupg", ".p12", ".pfx", ".pem", ".key", ".jks", ".keystore",
	// 云与工具凭据
	".aws", ".kube", ".docker/config.json", ".netrc", ".npmrc", ".pypirc",
	".git-credentials", ".config/gcloud", ".azure", ".aliyun", ".tencentcloud",
	// 通用配置/密钥
	".env", "credentials", "secret", "token", "passwd", "shadow",
	"kubeconfig", "serviceaccount",
	// 进程环境（可能带密钥）
	"/proc/self/environ", "/proc/1/environ", "environ",
	// 路径穿越
	"..",
}

// secretPatterns 用于把输出里的密钥遮掉：即使参数已经审过，
// 命令的输出仍可能带上 DSN / token（例如 systemctl status 回显 Environment=）。
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(password|passwd|pwd|token|secret|api[_-]?key)\b\s*[=:]\s*\S+`),
	regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*)://[^:@\s]+:[^@\s]+@`),
	// 不带 scheme 的 DSN：user:password@host（MySQL 字符串就是这种形状）
	regexp.MustCompile(`\b[a-zA-Z0-9_.-]+:[^\s@:/]{3,}@([a-zA-Z0-9_.-]+(?::\d+)?)`),
	regexp.MustCompile(`(?i)(authorization:\s*bearer)\s+\S+`),
}

// ShellError 是"命令被拒绝"的错误：带上拒绝原因，让模型能改用别的命令。
type ShellError struct {
	Reason string
}

func (e *ShellError) Error() string { return "run_shell 拒绝执行：" + e.Reason }

// ExitError 表示命令真的跑了，但退出码非 0。
type ExitError struct {
	Command string
	Code    int
	Stdout  string
	Stderr  string
}

func (e *ExitError) Error() string {
	msg := fmt.Sprintf("命令 %q 退出码 %d", e.Command, e.Code)
	if tail := lastLines(e.Stderr, 3); tail != "" {
		msg += "；stderr：" + tail
	} else if tail := lastLines(e.Stdout, 2); tail != "" {
		msg += "；输出：" + tail
	}
	return msg
}

type shellArgs struct {
	Command string `json:"command"`
}

// shellTool 是 run_shell / run_shell_admin 的实现。
//
// 五道闸门，全部在代码里（不靠提示词）：
//  1. 词法：拒绝一切壳元字符，切词后**直接 exec**，不起 shell；
//  2. 允许清单：可执行文件 + 参数前缀必须命中（读档与写档是两份清单）；
//  3. 参数审查：路径必须在白名单根目录下，**私钥/凭据/配置片段一律拒绝**；
//  4. 运行沙箱：固定工作目录、最小环境变量、超时、输出上限；
//  5. 分级：只读档默认放行；变更档 TierDangerous 强制人工审批，执行前写审计。
type shellTool struct {
	cfg    ShellConfig
	mode   ShellMode
	runner shellRunner
}

type shellRunner func(ctx context.Context, argv []string, dir string, env []string, limit int) (stdout, stderr string, code int, truncated bool, err error)

// NewShell 构造**只读**档（默认无需审批）。
func NewShell(cfg ShellConfig) Tool {
	return &shellTool{cfg: cfg.Normalize(), mode: ShellRead, runner: execShell}
}

// NewShellAdmin 构造**变更**档（每次都要审批）。
func NewShellAdmin(cfg ShellConfig) Tool {
	return &shellTool{cfg: cfg.Normalize(), mode: ShellWrite, runner: execShell}
}

func (t *shellTool) name() string {
	if t.mode == ShellWrite {
		return "run_shell_admin"
	}
	return "run_shell"
}

func (t *shellTool) Spec() Spec {
	read := t.mode == ShellRead

	desc := "执行一条**只读**系统命令（默认无需审批）。参数按空格分隔，**不支持**管道、重定向、" +
		"变量替换、命令串联（`; | & > $ ( )` 等会被直接拒绝）。" +
		"为安全起见，读取私钥/凭据/配置文件（`.ssh`、`*.pem`、`id_rsa`、`.env`、`shadow`、`credentials` 等）一律拒绝。" +
		"只能执行允许清单内的命令：\n" + formatRules(t.cfg.ReadOnly)
	if !read {
		desc = "执行一条**变更类**系统命令（每次都必须人工审批）。参数按空格分隔，**不支持**管道、重定向、" +
			"变量替换、命令串联。读取私钥/凭据/配置文件一律拒绝。只能执行允许清单内的命令：\n" +
			formatRules(t.cfg.Mutating)
	}

	spec := Spec{
		Name:        t.name(),
		Description: desc,
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "command": {"type": "string", "description": "完整命令，例如：df -h 或 journalctl -u checkout-service -n 50"}
  },
  "required": ["command"]
}`),
		Tier:       TierHarmless,
		MaxRetry:   1, // 只读命令重试是安全的
		Timeout:    t.cfg.Timeout + 5*time.Second,
		Idempotent: true,
		Preview: func(_ context.Context, rawArgs json.RawMessage) (string, error) {
			argv, err := t.validate(rawArgs)
			if err != nil {
				return "（参数被拒绝：" + err.Error() + "）", nil
			}
			return fmt.Sprintf("将执行：%s\n工作目录：%s\n超时：%s", strings.Join(argv, " "), t.workDir(), t.cfg.Timeout), nil
		},
		Audit: func(rawArgs json.RawMessage) (string, bool, error) {
			argv, err := t.validate(rawArgs)
			if err != nil {
				return t.name(), false, nil
			}
			// 只读档不产生变更；变更档按"变更"审计。
			return argv[0], !read, nil
		},
	}

	if !read {
		spec.Tier = TierDangerous
		spec.RequireApproval = true
		spec.ApprovalRole = "sre"
		spec.ApprovalTTL = 15 * time.Minute
		spec.MaxRetry = 0 // 变更命令绝不自动重试
		spec.Idempotent = false
	}
	return spec
}

func (t *shellTool) Validate(rawArgs json.RawMessage) error {
	_, err := t.validate(rawArgs)
	return err
}

func (t *shellTool) Invoke(ctx context.Context, rawArgs json.RawMessage) (Result, error) {
	argv, err := t.validate(rawArgs)
	if err != nil {
		return Result{}, err
	}

	runCtx, cancel := context.WithTimeout(ctx, t.cfg.Timeout)
	defer cancel()

	started := time.Now()
	stdout, stderr, code, truncated, runErr := t.runner(runCtx, argv, t.workDir(), minimalEnv(), t.cfg.MaxOutputBytes)
	elapsed := time.Since(started)

	if runErr != nil {
		if errors.Is(runErr, context.DeadlineExceeded) || errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return Result{}, fmt.Errorf("命令 %q 超过 %s 未结束，已被终止", argv[0], t.cfg.Timeout)
		}
		return Result{}, fmt.Errorf("启动命令 %q 失败：%w", argv[0], runErr)
	}

	stdout = redactSecrets(stdout)
	stderr = redactSecrets(stderr)

	if code != 0 {
		return Result{}, &ExitError{
			Command: strings.Join(argv, " "), Code: code, Stdout: stdout, Stderr: stderr,
		}
	}

	mode := "write"
	if t.mode == ShellRead {
		mode = "read"
	}
	data := map[string]any{
		"command":     strings.Join(argv, " "),
		"exit_code":   code,
		"stdout":      stdout,
		"stderr":      stderr,
		"truncated":   truncated,
		"duration_ms": elapsed.Milliseconds(),
		"mode":        mode,
	}
	summary := fmt.Sprintf("执行 %s 成功（退出码 0，耗时 %s）", argv[0], elapsed.Round(time.Millisecond))
	if truncated {
		summary += "，输出已截断"
	}
	return Result{Data: data, Summary: summary}, nil
}

// validate 是五道闸门里的前三道（词法 / 允许清单 / 参数审查）。
//
// Validate 与 Invoke 都会调用它：多一次纯函数调用，换"任何路径都不会绕过校验"。
func (t *shellTool) validate(rawArgs json.RawMessage) ([]string, error) {
	var args shellArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &ShellError{Reason: "参数不是合法 JSON：" + err.Error()}
	}
	cmdline := strings.TrimSpace(args.Command)
	if cmdline == "" {
		return nil, &ShellError{Reason: "command 不能为空"}
	}
	if len(cmdline) > t.cfg.MaxCommandLen {
		return nil, &ShellError{Reason: fmt.Sprintf("命令过长（%d > %d 字符）", len(cmdline), t.cfg.MaxCommandLen)}
	}

	argv, err := lexCommand(cmdline)
	if err != nil {
		return nil, err
	}
	if len(argv) == 0 {
		return nil, &ShellError{Reason: "命令解析后为空"}
	}
	if len(argv)-1 > t.cfg.MaxArgs {
		return nil, &ShellError{Reason: fmt.Sprintf("参数过多（%d > %d）", len(argv)-1, t.cfg.MaxArgs)}
	}

	// 不允许带路径的执行文件：`/bin/rm` 会绕过按名字匹配的允许清单。
	if strings.ContainsAny(argv[0], `/\`) {
		return nil, &ShellError{Reason: "可执行文件不能带路径（只允许按命令名匹配允许清单）"}
	}

	rules := t.cfg.rulesFor(t.mode)
	if !allowedByRules(rules, argv) {
		return nil, &ShellError{Reason: fmt.Sprintf("命令 %q 不在%s允许清单内。允许的命令：\n%s",
			strings.Join(argv, " "), t.modeLabel(), formatRules(rules))}
	}

	// 参数审查：既查"能不能读"，也查"能读到哪"。
	for _, arg := range argv[1:] {
		if hit, bad := sensitiveFragment(arg); bad {
			return nil, &ShellError{Reason: fmt.Sprintf("参数 %q 命中敏感片段 %q（不允许读取私钥/凭据/配置）", arg, hit)}
		}
		if strings.HasPrefix(arg, "/") && !pathAllowed(t.cfg.AllowedPathRoots, arg) {
			return nil, &ShellError{Reason: fmt.Sprintf("路径 %q 不在允许的根目录内（允许：%s）", arg, strings.Join(t.cfg.AllowedPathRoots, ", "))}
		}
	}
	return argv, nil
}

func (t *shellTool) modeLabel() string {
	if t.mode == ShellWrite {
		return "变更类"
	}
	return "只读"
}

func allowedByRules(rules []ShellRule, argv []string) bool {
	for _, rule := range rules {
		if rule.Exec != argv[0] {
			continue
		}
		if len(argv)-1 < len(rule.Args) {
			continue
		}
		ok := true
		for i, want := range rule.Args {
			if argv[1+i] != want {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// sensitiveFragment 判断参数是否指向私钥/凭据/配置。
//
// 大小写不敏感；命中即拒绝（不是遮罩——**读都不允许**）。
func sensitiveFragment(arg string) (string, bool) {
	lower := strings.ToLower(strings.ReplaceAll(arg, `\`, "/"))
	for _, denied := range deniedArgs {
		if strings.Contains(lower, denied) {
			return denied, true
		}
	}
	return "", false
}

func pathAllowed(roots []string, p string) bool {
	clean := strings.ReplaceAll(p, `\`, "/")
	for _, root := range roots {
		root = strings.TrimSuffix(strings.ReplaceAll(root, `\`, "/"), "/")
		if clean == root || strings.HasPrefix(clean, root+"/") {
			return true
		}
	}
	return false
}

func (t *shellTool) workDir() string {
	if strings.TrimSpace(t.cfg.WorkDir) != "" {
		return t.cfg.WorkDir
	}
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

// lexCommand 把命令切成 argv。
//
// 支持单/双引号包裹含空格的参数；**任何壳元字符都会被拒绝**（引号内也不例外）。
// 不做变量替换、不做通配符展开、不做反斜杠转义——能力越少，越可预测。
func lexCommand(s string) ([]string, error) {
	var (
		argv   []string
		cur    strings.Builder
		inWord bool
		quote  rune
	)
	flush := func() {
		if inWord {
			argv = append(argv, cur.String())
			cur.Reset()
			inWord = false
		}
	}

	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			if strings.ContainsRune(shellForbidden, r) {
				return nil, &ShellError{Reason: fmt.Sprintf("不允许的字符 %q（不支持 shell 语法）", r)}
			}
			cur.WriteRune(r)
			inWord = true

		case r == '\'' || r == '"':
			quote = r
			inWord = true

		case r == ' ' || r == '\t':
			flush()

		case r == '\n' || r == '\r':
			return nil, &ShellError{Reason: "不允许换行（一条命令只能是一行）"}

		default:
			if strings.ContainsRune(shellForbidden, r) {
				return nil, &ShellError{Reason: fmt.Sprintf("不允许的字符 %q（不支持管道/重定向/变量替换/命令串联）", r)}
			}
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, &ShellError{Reason: "引号没有闭合"}
	}
	flush()
	return argv, nil
}

// minimalEnv 是**最小环境变量**。
//
// Agent 进程的环境里有数据库 DSN、模型 API Key，直接透传等于把密钥交给每一条命令。
func minimalEnv() []string {
	env := []string{"LANG=C.UTF-8", "LC_ALL=C.UTF-8", "TZ=UTC"}
	for _, key := range []string{"PATH", "HOME", "SystemRoot", "TEMP", "TMP", "USERPROFILE", "COMSPEC"} {
		if v := os.Getenv(key); v != "" {
			env = append(env, key+"="+v)
		}
	}
	return env
}

func execShell(ctx context.Context, argv []string, dir string, env []string, limit int) (string, string, int, bool, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = nil // 不接 stdin：避免命令停下来等输入

	stdout := &limitedBuffer{limit: limit}
	stderr := &limitedBuffer{limit: limit}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			return stdout.String(), stderr.String(), -1, stdout.truncated || stderr.truncated, err
		}
	}
	return stdout.String(), stderr.String(), code, stdout.truncated || stderr.truncated, nil
}

// limitedBuffer 只保留前 limit 字节，超过就丢弃并标记（不无限吃内存）。
type limitedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	remain := b.limit - b.buf.Len()
	if remain <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > remain {
		b.buf.Write(p[:remain])
		b.truncated = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) String() string { return b.buf.String() }

func redactSecrets(s string) string {
	if s == "" {
		return s
	}
	for _, re := range secretPatterns {
		if strings.Contains(re.String(), "([a-zA-Z0-9_.-]+") {
			s = re.ReplaceAllString(s, "***@$1")
			continue
		}
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			switch {
			case strings.Contains(m, "://"):
				idx := strings.Index(m, "://")
				at := strings.LastIndex(m, "@")
				if at > idx {
					return m[:idx+3] + "***" + m[at:]
				}
				return "***"
			default:
				if i := strings.IndexAny(m, "=:"); i >= 0 {
					return m[:i+1] + "***"
				}
				return "***"
			}
		})
	}
	return s
}

func lastLines(s string, n int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := strings.Join(lines, " ")
	if len(out) > 200 {
		out = out[:200] + "…"
	}
	return out
}

func formatRules(rules []ShellRule) string {
	if len(rules) == 0 {
		return "（空：所有命令都会被拒绝）"
	}
	parts := make([]string, 0, len(rules))
	for _, r := range rules {
		parts = append(parts, strings.TrimSpace(r.Exec+" "+strings.Join(r.Args, " ")))
	}
	return "  - " + strings.Join(parts, "\n  - ")
}

// ParseShellRules 解析 `exe arg1 arg2;exe2 ...` 形式的允许清单。
func ParseShellRules(s string) ([]ShellRule, error) {
	var rules []ShellRule
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		fields := strings.Fields(part)
		if len(fields) == 0 {
			continue
		}
		if strings.ContainsAny(fields[0], `/\`) {
			return nil, fmt.Errorf("允许清单里的命令不能带路径：%q", fields[0])
		}
		rules = append(rules, ShellRule{Exec: fields[0], Args: fields[1:]})
	}
	return rules, nil
}
