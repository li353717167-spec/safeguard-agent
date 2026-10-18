package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseDotEnvLine(t *testing.T) {
	cases := []struct {
		name      string
		line      string
		wantKey   string
		wantValue string
		wantOK    bool
		wantErr   bool
	}{
		{name: "普通键值", line: "SAFEGUARD_DB_DSN=user:pw@tcp(127.0.0.1:3306)/db", wantKey: "SAFEGUARD_DB_DSN", wantValue: "user:pw@tcp(127.0.0.1:3306)/db", wantOK: true},
		{name: "空行", line: "   ", wantOK: false},
		{name: "注释行", line: "# 这是注释", wantOK: false},
		{name: "export 前缀", line: "export FOO=bar", wantKey: "FOO", wantValue: "bar", wantOK: true},
		{name: "键两侧空格", line: "  FOO = bar  ", wantKey: "FOO", wantValue: "bar", wantOK: true},
		{name: "双引号值", line: `FOO="two words"`, wantKey: "FOO", wantValue: "two words", wantOK: true},
		{name: "单引号值保留井号", line: `FOO='x#y'`, wantKey: "FOO", wantValue: "x#y", wantOK: true},
		{name: "引号后跟行尾注释", line: `FOO='x#y' # 注释`, wantKey: "FOO", wantValue: "x#y", wantOK: true},
		{name: "引号内的井号不被当注释", line: `FOO="a # b"`, wantKey: "FOO", wantValue: "a # b", wantOK: true},
		{name: "无引号值的行尾注释", line: "FOO=bar # 注释", wantKey: "FOO", wantValue: "bar", wantOK: true},
		{name: "密码里的井号不被误伤", line: "FOO=pa#ss", wantKey: "FOO", wantValue: "pa#ss", wantOK: true},
		{name: "空值", line: "FOO=", wantKey: "FOO", wantValue: "", wantOK: true},
		{name: "缺少等号", line: "FOO", wantErr: true},
		{name: "键名非法字符", line: "FO O=1", wantErr: true},
		{name: "引号未闭合", line: `FOO="abc`, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, value, ok, err := parseDotEnvLine(tc.line)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际得到 key=%q value=%q", key, value)
				}
				return
			}
			if err != nil {
				t.Fatalf("未预期的错误: %v", err)
			}
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, 期望 %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if key != tc.wantKey {
				t.Errorf("key = %q, 期望 %q", key, tc.wantKey)
			}
			if value != tc.wantValue {
				t.Errorf("value = %q, 期望 %q", value, tc.wantValue)
			}
		})
	}
}

func TestLoadDotEnvAppliesAndRespectsRealEnv(t *testing.T) {
	// t.Setenv 会在用例结束后自动还原（首次调用相当于"用完置空"），
	// 避免这几个键泄漏到同一进程的其他用例里。
	t.Setenv("SAFEGUARD_TEST_FROM_FILE", "")
	t.Setenv("SAFEGUARD_TEST_QUOTED", "")
	t.Setenv("SAFEGUARD_TEST_EXISTING", "from-real-env")

	path := filepath.Join(t.TempDir(), ".env")
	content := "SAFEGUARD_TEST_FROM_FILE=file-value\n" +
		"SAFEGUARD_TEST_QUOTED=\"quoted value\"\n" +
		"SAFEGUARD_TEST_EXISTING=from-file\n" +
		"# 注释行\n" +
		"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写临时 .env 失败: %v", err)
	}

	applied, err := LoadDotEnv(path)
	if err != nil {
		t.Fatalf("LoadDotEnv 失败: %v", err)
	}
	if applied != 2 {
		t.Errorf("注入键数 = %d, 期望 2（已存在的真实环境变量不应被覆盖）", applied)
	}
	if got := os.Getenv("SAFEGUARD_TEST_FROM_FILE"); got != "file-value" {
		t.Errorf("SAFEGUARD_TEST_FROM_FILE = %q, 期望 file-value", got)
	}
	if got := os.Getenv("SAFEGUARD_TEST_QUOTED"); got != "quoted value" {
		t.Errorf("SAFEGUARD_TEST_QUOTED = %q, 期望 quoted value", got)
	}
	// 真实环境变量优先：线上靠环境变量注入，本地 .env 不应该把它盖掉。
	if got := os.Getenv("SAFEGUARD_TEST_EXISTING"); got != "from-real-env" {
		t.Errorf("SAFEGUARD_TEST_EXISTING = %q, 期望 from-real-env", got)
	}
}

func TestLoadDotEnvMissingFileIsNotAnError(t *testing.T) {
	applied, err := LoadDotEnv(filepath.Join(t.TempDir(), "not-exists.env"))
	if err != nil {
		t.Fatalf("文件不存在不应报错，得到: %v", err)
	}
	if applied != 0 {
		t.Errorf("applied = %d, 期望 0", applied)
	}
}

func TestLoadDefaultsAndValidation(t *testing.T) {
	// 清掉可能存在的同名环境变量，验证默认值路径。
	for _, k := range []string{"SAFEGUARD_LLM_BASE_URL", "SAFEGUARD_LLM_MODEL", "SAFEGUARD_LLM_TIMEOUT", "SAFEGUARD_DB_DSN"} {
		t.Setenv(k, "")
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if cfg.LLM.BaseURL == "" || cfg.LLM.Model == "" {
		t.Errorf("默认 BaseURL/Model 不应为空: %+v", cfg.LLM)
	}
	if cfg.LLM.Timeout <= 0 {
		t.Errorf("默认超时应为正数, 得到 %s", cfg.LLM.Timeout)
	}

	// 无 key 但端点与模型齐全时应当通过：本地模型不需要密钥。
	if err := cfg.LLM.Validate(); err != nil {
		t.Errorf("本地无密钥端点应通过校验, 得到: %v", err)
	}
	// 没有 DSN 时必须明确报错，而不是等到连接超时才暴露。
	if err := cfg.DB.Validate(); err == nil {
		t.Error("缺少 DSN 时应报错")
	}
}
