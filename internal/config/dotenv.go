package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// 这里刻意不引入 godotenv 之类的依赖：
// 需求只有"读 KEY=VALUE 并注入环境变量"，几十行标准库代码就够，
// 而且省下一个依赖就少一处供应链风险。

// LoadDotEnv 读取 .env 文件并把其中的键值注入当前进程环境。
//
// 规则：
//   - 文件不存在不是错误（本地没配 .env 时应当照常启动）；
//   - 已存在的环境变量优先，.env 不覆盖真实环境（线上靠环境变量注入，本地靠 .env）；
//   - 支持 `#` 注释与 `export ` 前缀，支持单/双引号包裹的值。
//
// 返回实际注入的键数量。
func LoadDotEnv(path string) (int, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("config: 读取 %s 失败: %w", path, err)
	}

	applied := 0
	for i, line := range strings.Split(string(content), "\n") {
		key, value, ok, err := parseDotEnvLine(line)
		if err != nil {
			return applied, fmt.Errorf("config: %s 第 %d 行: %w", filepath.Base(path), i+1, err)
		}
		if !ok {
			continue
		}
		if existing, exists := os.LookupEnv(key); exists && existing != "" {
			continue // 真实环境变量优先
		}
		if err := os.Setenv(key, value); err != nil {
			return applied, fmt.Errorf("config: 设置环境变量 %s 失败: %w", key, err)
		}
		applied++
	}
	return applied, nil
}

func parseDotEnvLine(line string) (key, value string, ok bool, err error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false, nil
	}
	if rest, found := strings.CutPrefix(line, "export"); found {
		line = strings.TrimSpace(rest)
	}

	key, value, found := strings.Cut(line, "=")
	if !found {
		return "", "", false, fmt.Errorf("缺少 '='，应为 KEY=VALUE")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", "", false, fmt.Errorf("键名为空")
	}
	for _, r := range key {
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return "", "", false, fmt.Errorf("键名 %q 含非法字符（只允许字母、数字、下划线）", key)
		}
	}

	value = strings.TrimSpace(value)

	// 引号值：先找配对的结束引号，之后的内容（行尾注释）直接丢弃。
	// 注意顺序——必须先处理引号再找行尾注释，否则 `KEY="a # b"` 会被从中间切断。
	if len(value) > 0 && (value[0] == '"' || value[0] == '\'') {
		quote := value[0]
		if end := strings.IndexByte(value[1:], quote); end >= 0 {
			return key, value[1 : 1+end], true, nil
		}
		return "", "", false, fmt.Errorf("引号未闭合")
	}

	// 非引号值：只在 "空格+#" 处截断行尾注释，避免误伤值里的 #（例如密码）。
	if idx := strings.Index(value, " #"); idx >= 0 {
		value = strings.TrimSpace(value[:idx])
	}
	return key, value, true, nil
}
