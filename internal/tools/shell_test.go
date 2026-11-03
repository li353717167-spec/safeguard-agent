package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func shellArgsJSON(cmd string) json.RawMessage {
	raw, _ := json.Marshal(map[string]string{"command": cmd})
	return raw
}

func testShell(runner shellRunner) *shellTool {
	cfg := DefaultShellConfig()
	cfg.Rules = []ShellRule{
		{Exec: "df", Args: []string{"-h"}},
		{Exec: "journalctl", Args: []string{"-u"}},
	}
	cfg.Timeout = 2 * time.Second
	return &shellTool{cfg: cfg, runner: runner}
}

// 词法：任何壳元字符都拒绝——不支持管道、重定向、变量替换、命令串联。
func TestLexRejectsShellMetacharacters(t *testing.T) {
	for _, cmd := range []string{
		"df -h; rm -rf /",
		"df -h | grep x",
		"df -h > /tmp/out",
		"df -h && whoami",
		"journalctl -u $(whoami)",
		"df `id`",
		"df -h &",
		"echo hi\nid",
	} {
		if _, err := lexCommand(cmd); err == nil {
			t.Errorf("应拒绝含元字符的命令: %q", cmd)
		}
	}
}

func TestLexSplitsAndRespectsQuotes(t *testing.T) {
	argv, err := lexCommand(`journalctl -u "checkout service" -n 50`)
	if err != nil {
		t.Fatalf("正常命令不该被拒绝: %v", err)
	}
	want := []string{"journalctl", "-u", "checkout service", "-n", "50"}
	if len(argv) != len(want) {
		t.Fatalf("切词结果 %v，期望 %v", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Errorf("argv[%d]=%q，期望 %q", i, argv[i], want[i])
		}
	}
	if _, err := lexCommand(`df -h "unclosed`); err == nil {
		t.Error("引号未闭合应拒绝")
	}
}

func TestShellAllowlist(t *testing.T) {
	s := testShell(nil)

	if _, err := s.validate(shellArgsJSON("df -h")); err != nil {
		t.Errorf("允许清单内的命令应通过: %v", err)
	}
	// 清单里有 df，但参数不是 -h
	if _, err := s.validate(shellArgsJSON("df -i")); err == nil {
		t.Error("参数前缀不匹配应拒绝（df -i）")
	}
	// journalctl 必须带 -u
	if _, err := s.validate(shellArgsJSON("journalctl -n 10")); err == nil {
		t.Error("缺少 -u 前缀应拒绝")
	}
	if _, err := s.validate(shellArgsJSON("rm -rf /tmp/x")); err == nil {
		t.Error("不在清单内的命令必须拒绝")
	}
	// 带路径的可执行文件会绕过按名字匹配
	if _, err := s.validate(shellArgsJSON("/bin/df -h")); err == nil {
		t.Error("可执行文件带路径应拒绝")
	}
}

func TestShellArgGuards(t *testing.T) {
	s := testShell(nil)

	// 绝对路径必须在白名单根目录下
	if _, err := s.validate(shellArgsJSON("journalctl -u x --file /etc/shadow")); err == nil {
		t.Error("/etc/shadow 既不在允许根目录，也命中敏感片段，必须拒绝")
	}
	if _, err := s.validate(shellArgsJSON("journalctl -u x --file /var/log/app.log")); err != nil {
		t.Errorf("/var/log 下应允许: %v", err)
	}
	// 敏感片段
	if _, err := s.validate(shellArgsJSON("journalctl -u x --file /var/log/.ssh/id_rsa")); err == nil {
		t.Error(".ssh 必须拒绝")
	}
	// 参数过多
	if _, err := s.validate(shellArgsJSON("df -h " + strings.Repeat("x ", 20))); err == nil {
		t.Error("参数过多应拒绝")
	}
	// 空命令
	if _, err := s.validate(shellArgsJSON("   ")); err == nil {
		t.Error("空命令应拒绝")
	}
}

// 命令真的执行成功：把 stdout/stderr/退出码结构化返回。
func TestShellInvokeSuccess(t *testing.T) {
	s := testShell(func(_ context.Context, argv []string, _ string, _ []string, _ int) (string, string, int, bool, error) {
		return "Filesystem Size Used\n/dev/sda1 40G 12G\n", "", 0, false, nil
	})
	res, err := s.Invoke(context.Background(), shellArgsJSON("df -h"))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	data := res.Data.(map[string]any)
	if data["exit_code"] != 0 {
		t.Errorf("exit_code = %v", data["exit_code"])
	}
	if !strings.Contains(data["stdout"].(string), "/dev/sda1") {
		t.Errorf("stdout 丢失: %v", data["stdout"])
	}
	if !strings.Contains(res.Summary, "df") {
		t.Errorf("摘要应说明执行了什么: %s", res.Summary)
	}
}

// 非零退出码：不是"工具坏了"，要把输出带回去让模型判断。
func TestShellInvokeNonZeroExit(t *testing.T) {
	s := testShell(func(_ context.Context, _ []string, _ string, _ []string, _ int) (string, string, int, bool, error) {
		return "", "No such unit: nope\n", 1, false, nil
	})
	_, err := s.Invoke(context.Background(), shellArgsJSON("journalctl -u nope"))
	if err == nil {
		t.Fatal("退出码非 0 应返回错误")
	}
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("应是 ExitError，实际 %T", err)
	}
	if exitErr.Code != 1 || !strings.Contains(exitErr.Stderr, "No such unit") {
		t.Errorf("错误里必须带上退出码与 stderr: %+v", exitErr)
	}
	if !strings.Contains(err.Error(), "No such unit") {
		t.Errorf("错误文本应包含 stderr 摘要: %s", err.Error())
	}
}

// 超时必须被终止并给出明确原因（不能把 ctx 取消当成普通失败）。
func TestShellInvokeTimeout(t *testing.T) {
	s := testShell(func(ctx context.Context, _ []string, _ string, _ []string, _ int) (string, string, int, bool, error) {
		<-ctx.Done()
		return "", "", -1, false, ctx.Err()
	})
	_, err := s.Invoke(context.Background(), shellArgsJSON("df -h"))
	if err == nil || !strings.Contains(err.Error(), "超过") {
		t.Fatalf("超时应返回明确错误: %v", err)
	}
}

// 输出里的密钥必须被遮掉：即使环境变量已白名单化，命令输出仍可能带 DSN/token。
func TestShellRedactsSecrets(t *testing.T) {
	s := testShell(func(_ context.Context, _ []string, _ string, _ []string, _ int) (string, string, int, bool, error) {
		return "DSN=appuser:Lisheng@123@tcp(10.0.0.1:3306)/appdb\npassword=hunter2\nAuthorization: Bearer sk-abc123\n", "", 0, false, nil
	})
	res, err := s.Invoke(context.Background(), shellArgsJSON("df -h"))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	stdout := res.Data.(map[string]any)["stdout"].(string)
	for _, leaked := range []string{"Lisheng@123", "hunter2", "sk-abc123"} {
		if strings.Contains(stdout, leaked) {
			t.Errorf("密钥泄漏: %q\n输出: %s", leaked, stdout)
		}
	}
	if !strings.Contains(stdout, "***") {
		t.Errorf("应留下遮罩标记: %s", stdout)
	}
}

// 环境变量白名单：绝不把 Agent 自己的密钥传给命令。
func TestMinimalEnvExcludesSecrets(t *testing.T) {
	t.Setenv("SAFEGUARD_DB_DSN", "appuser:secret@tcp(127.0.0.1)/appdb")
	t.Setenv("SAFEGUARD_LLM_API_KEY", "sk-secret")
	for _, kv := range minimalEnv() {
		for _, leaked := range []string{"SAFEGUARD_DB_DSN", "SAFEGUARD_LLM_API_KEY", "sk-secret", "appuser:secret"} {
			if strings.Contains(kv, leaked) {
				t.Errorf("环境变量泄漏: %s", kv)
			}
		}
	}
}

func TestLimitedBufferTruncates(t *testing.T) {
	b := &limitedBuffer{limit: 8}
	_, _ = b.Write([]byte("0123456789abcdef"))
	if !b.truncated {
		t.Error("超过上限应标记截断")
	}
	if len(b.String()) != 8 {
		t.Errorf("应只保留 8 字节，实际 %d", len(b.String()))
	}
}

// 注册门禁：Spec 必须能通过校验（描述长度、超时等）。
func TestShellPassesRegistryGate(t *testing.T) {
	reg, err := NewFullRegistry(nil, nil)
	if err != nil {
		t.Fatalf("注册内置工具失败: %v", err)
	}
	if err := reg.Register(NewShell(DefaultShellConfig())); err != nil {
		t.Fatalf("run_shell 未通过注册门禁: %v", err)
	}
}

// 真跑一次无害命令：证明 exec 路径（不是只测 mock）。
func TestShellRunsRealCommand(t *testing.T) {
	bin := "hostname"
	if _, err := os.Stat("/usr/bin/" + bin); err != nil && runtime.GOOS != "windows" {
		if _, err := os.Stat("/bin/" + bin); err != nil {
			t.Skipf("本机没有 %s，跳过真实执行测试", bin)
		}
	}
	cfg := DefaultShellConfig()
	cfg.Rules = []ShellRule{{Exec: bin}}
	cfg.Timeout = 5 * time.Second
	tool := NewShell(cfg)

	res, err := tool.Invoke(context.Background(), shellArgsJSON(bin))
	if err != nil {
		t.Fatalf("真实执行失败: %v", err)
	}
	if strings.TrimSpace(res.Data.(map[string]any)["stdout"].(string)) == "" {
		t.Error("hostname 应该非空输出")
	}
}
