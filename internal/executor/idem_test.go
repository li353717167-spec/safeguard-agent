package executor

import (
	"encoding/json"
	"testing"
)

const (
	testTask  = "task_0123456789abcdef"
	testStep  = 3
	testCall  = 1
	testTool  = "query_metrics"
	testArgs  = `{"service":"payment-service","metric":"error_rate","window":"5m"}`
	testArgs2 = `{"window":"5m","metric":"error_rate","service":"payment-service"}`
)

func mustKey(t *testing.T, taskID string, stepNo, callIndex int, tool string, args string) string {
	t.Helper()
	key, err := BuildIdempotencyKey(taskID, stepNo, callIndex, tool, json.RawMessage(args))
	if err != nil {
		t.Fatalf("生成 key 失败: %v", err)
	}
	return key
}

// 幂等 key 的第一要求：同样的输入永远得到同样的输出。
func TestIdemKeyIsDeterministic(t *testing.T) {
	first := mustKey(t, testTask, testStep, testCall, testTool, testArgs)
	for i := 0; i < 50; i++ {
		if got := mustKey(t, testTask, testStep, testCall, testTool, testArgs); got != first {
			t.Fatalf("第 %d 次生成的 key 不一致: %s != %s", i+2, got, first)
		}
	}
	if len(first) != idemKeyLen {
		t.Errorf("key 长度 = %d, 期望 %d", len(first), idemKeyLen)
	}
	for _, r := range first {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Fatalf("key 里出现非十六进制字符 %q: %s", r, first)
		}
	}
}

// 参数顺序不同但语义相同 → 必须是同一个 key（否则幂等失效）。
func TestIdemKeyIgnoresArgumentKeyOrder(t *testing.T) {
	a := mustKey(t, testTask, testStep, testCall, testTool, testArgs)
	b := mustKey(t, testTask, testStep, testCall, testTool, testArgs2)
	if a != b {
		t.Errorf("参数键顺序不应影响 key:\n%s\n%s", a, b)
	}
}

// 任何一个"位置"或"内容"变了，都必须是不同的 key。
func TestIdemKeySensitivity(t *testing.T) {
	base := mustKey(t, testTask, testStep, testCall, testTool, testArgs)

	cases := []struct{ name, key string }{
		{"task 不同", mustKey(t, "task_fedcba9876543210", testStep, testCall, testTool, testArgs)},
		{"step 不同", mustKey(t, testTask, testStep+1, testCall, testTool, testArgs)},
		{"call 不同", mustKey(t, testTask, testStep, testCall+1, testTool, testArgs)},
		{"工具不同", mustKey(t, testTask, testStep, testCall, "query_config", testArgs)},
		{"参数不同", mustKey(t, testTask, testStep, testCall, testTool, `{"service":"payment-service","metric":"qps"}`)},
	}

	seen := map[string]string{base: "base"}
	for _, tc := range cases {
		if tc.key == base {
			t.Errorf("%s 时 key 没有变化：%s", tc.name, tc.key)
			continue
		}
		if prev, dup := seen[tc.key]; dup {
			t.Errorf("%s 与 %s 算出了同一个 key", tc.name, prev)
		}
		seen[tc.key] = tc.name
	}
}

// 字段之间必须有边界：否则 ("ab","c") 与 ("a","bc") 会拼出同一个串。
func TestIdemKeyHasFieldBoundaries(t *testing.T) {
	a := mustKey(t, "ab", 1, 1, "c", `{}`)
	b := mustKey(t, "a", 1, 1, "bc", `{}`)
	if a == b {
		t.Error("字段拼接缺少边界，不同输入算出了同一个 key")
	}
}

func TestIdemKeyRejectsInvalidArgs(t *testing.T) {
	if _, err := BuildIdempotencyKey(testTask, 1, 1, testTool, json.RawMessage(`{`)); err == nil {
		t.Error("非法参数应当在生成 key 时就失败，而不是带着坏参数继续执行")
	}
}
