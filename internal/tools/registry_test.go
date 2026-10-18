package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// stubTool 只携带定义，用来验证注册门禁——门禁检查的是 Spec，不是执行。
type stubTool struct{ spec Spec }

func (s stubTool) Spec() Spec { return s.spec }

func (s stubTool) Invoke(context.Context, json.RawMessage) (Result, error) {
	return Result{}, nil
}

func (s stubTool) Validate(json.RawMessage) error { return nil }

func validSpec() Spec {
	return Spec{
		Name:        "some_tool",
		Description: "这是一个长度足够的工具描述，用来验证注册门禁本身是否生效。",
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
		Tier:        TierHarmless,
		Timeout:     5 * time.Second,
		MaxRetry:    2,
		Idempotent:  true,
	}
}

// TestRegisterGate 把"安全约束做成启动期断言"逐条钉住。
// 这些约束若只在运行时暴露，代价是模型选错工具或危险操作绕过审批。
func TestRegisterGate(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(*Spec)
		wantSubstr string
	}{
		{"名字不符合规则", func(s *Spec) { s.Name = "Bad-Name" }, "不合法"},
		{"名字过长", func(s *Spec) { s.Name = strings.Repeat("a", 65) }, "不合法"},
		{"描述太短", func(s *Spec) { s.Description = "太短了" }, "描述"},
		{"参数 schema 不是合法 JSON", func(s *Spec) { s.Parameters = json.RawMessage(`{`) }, "schema"},
		{"参数 schema 为空", func(s *Spec) { s.Parameters = nil }, "schema"},
		{"高危工具却不需要审批", func(s *Spec) { s.Tier = TierDangerous }, "RequireApproval"},
		{"需要审批但没有 TTL", func(s *Spec) { s.RequireApproval = true }, "ApprovalTTL"},
		{"没有设置超时", func(s *Spec) { s.Timeout = 0 }, "Timeout"},
		{"重试次数超出上限", func(s *Spec) { s.MaxRetry = maxToolRetry + 1 }, "MaxRetry"},
		{"重试次数为负", func(s *Spec) { s.MaxRetry = -1 }, "MaxRetry"},
		{"非幂等工具却配置了自动重试", func(s *Spec) { s.Idempotent = false; s.MaxRetry = 1 }, "非幂等"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := validSpec()
			tc.mutate(&spec)
			err := NewRegistry().Register(stubTool{spec: spec})
			if err == nil {
				t.Fatalf("期望注册失败，实际通过了：%+v", spec)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("错误信息里应提到 %q，实际: %v", tc.wantSubstr, err)
			}
		})
	}
}

func TestRegisterAcceptsValidToolAndRejectsDuplicate(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(stubTool{spec: validSpec()}); err != nil {
		t.Fatalf("合法工具不应被拒绝: %v", err)
	}
	if reg.Len() != 1 {
		t.Errorf("Len = %d, 期望 1", reg.Len())
	}
	err := reg.Register(stubTool{spec: validSpec()})
	if err == nil || !strings.Contains(err.Error(), "重复注册") {
		t.Fatalf("重复注册必须被拒绝，实际: %v", err)
	}
}

// TestDefaultRegistryToolsAreSane 让内置工具自己过一遍门禁。
func TestDefaultRegistryToolsAreSane(t *testing.T) {
	// ds 为 nil：这里只要定义，不执行任何工具。
	reg, err := NewDefaultRegistry(nil)
	if err != nil {
		t.Fatalf("内置工具未通过注册门禁: %v", err)
	}
	if reg.Len() != 3 {
		t.Fatalf("期望 3 个内置只读工具，实际 %d", reg.Len())
	}
	for _, name := range []string{"list_services", "query_config", "query_metrics"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("缺少内置工具 %q", name)
		}
	}
	// 三个只读工具的等级都应当是无害，且不要求审批。
	for _, e := range reg.Inventory() {
		if e.Tier != TierHarmless.String() {
			t.Errorf("工具 %s 的等级 = %s, 期望 harmless", e.Name, e.Tier)
		}
		if e.RequireApproval {
			t.Errorf("只读工具 %s 不应该要求审批", e.Name)
		}
		if !e.Idempotent {
			t.Errorf("只读工具 %s 应当是幂等的（D4 的重试前提）", e.Name)
		}
	}
}

// TestSchemasDoNotLeakPolicy 是权限边界测试：
// 给模型看的 schema 里绝不能出现 Tier / RequireApproval / 重试超时等策略字段。
// 一旦泄漏，等于把"要不要审批"的判断线索交给了概率模型。
func TestSchemasDoNotLeakPolicy(t *testing.T) {
	reg, err := NewDefaultRegistry(nil)
	if err != nil {
		t.Fatalf("构造注册中心失败: %v", err)
	}

	raw, err := json.Marshal(reg.Schemas())
	if err != nil {
		t.Fatalf("序列化 schema 失败: %v", err)
	}

	// 结构检查：外层只能是 {type, function}，function 只能是 {name, description, parameters}。
	// 这是最可靠的一条——多出任何字段都说明有人把策略塞进来了。
	var decoded []map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("解析 schema 失败: %v", err)
	}
	if len(decoded) == 0 {
		t.Fatal("schema 为空")
	}
	for _, item := range decoded {
		if len(item) != 2 {
			t.Errorf("schema 外层字段数 = %d, 期望 2（type/function）：%v", len(item), item)
		}
		fn, ok := item["function"].(map[string]any)
		if !ok {
			t.Fatalf("schema 结构异常: %v", item)
		}
		if len(fn) != 3 {
			t.Errorf("function 字段数 = %d, 期望 3（name/description/parameters）: %v", len(fn), fn)
		}
	}

	// 文本检查：只扫 parameters，不扫 description。
	// 描述是散文，合理地可能出现 "timeout_ms" 这类配置项名字；
	// 而 parameters 里出现策略字样就是真泄漏。
	policyTokens := []string{"tier", "approval", "max_retry", "maxretry", "idempotent", "dry_run", "dryrun", "backoff", "timeout"}
	for _, item := range decoded {
		fn := item["function"].(map[string]any)
		params, err := json.Marshal(fn["parameters"])
		if err != nil {
			t.Fatalf("序列化 parameters 失败: %v", err)
		}
		lower := strings.ToLower(string(params))
		for _, token := range policyTokens {
			if strings.Contains(lower, token) {
				t.Errorf("工具 %v 的 parameters 里出现了策略字段 %q：%s", fn["name"], token, params)
			}
		}
	}
}

// TestSchemasAreStableAndSorted 钉住确定性：
// prompt 稳定 → 前缀缓存命中稳定 → 评测结果可复现。
func TestSchemasAreStableAndSorted(t *testing.T) {
	reg, err := NewDefaultRegistry(nil)
	if err != nil {
		t.Fatalf("构造注册中心失败: %v", err)
	}
	first, _ := json.Marshal(reg.Schemas())
	second, _ := json.Marshal(reg.Schemas())
	if string(first) != string(second) {
		t.Error("两次生成的 schema 不一致：顺序不稳定会让缓存与评测都失去意义")
	}

	names := reg.Names()
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Errorf("工具名未排序: %v", names)
			break
		}
	}
	if len(reg.Schemas()) != len(names) {
		t.Errorf("schema 数量 = %d, 工具数量 = %d", len(reg.Schemas()), len(names))
	}
}
