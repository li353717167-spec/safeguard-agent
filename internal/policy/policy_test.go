package policy

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mechrevo/safeguard-agent/internal/tools"
)

func baseSpec() tools.Spec {
	return tools.Spec{
		Name:        "some_tool",
		Description: "测试用工具，描述长度足够通过注册门禁。",
		Tier:        tools.TierHarmless,
		Timeout:     5 * time.Second,
	}
}

func TestNeedsApproval(t *testing.T) {
	engine := New()

	cases := []struct {
		name      string
		mutate    func(*tools.Spec)
		want      bool
		wantRole  string
		wantInWhy string
	}{
		{
			name:   "只读工具不需要审批",
			mutate: func(s *tools.Spec) {},
			want:   false,
		},
		{
			name: "声明需要审批的工具必须审批",
			mutate: func(s *tools.Spec) {
				s.RequireApproval = true
				s.ApprovalRole = "sre"
				s.ApprovalTTL = 10 * time.Minute
			},
			want: true, wantRole: "sre", wantInWhy: "声明为需要审批",
		},
		{
			name: "高危工具即使漏配也兜底审批",
			mutate: func(s *tools.Spec) {
				s.Tier = tools.TierDangerous
				s.ApprovalRole = "sre"
			},
			want: true, wantRole: "sre", wantInWhy: "高危",
		},
		{
			name: "写操作没有 dry-run 也要审批",
			mutate: func(s *tools.Spec) {
				s.Tier = tools.TierWrite
				s.DryRun = false
			},
			want: true, wantInWhy: "dry-run",
		},
		{
			name: "写操作且支持 dry-run 预览时不强制审批",
			mutate: func(s *tools.Spec) {
				s.Tier = tools.TierWrite
				s.DryRun = true
			},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := baseSpec()
			tc.mutate(&spec)

			got := engine.NeedsApproval(spec, nil)
			if got.RequireApproval != tc.want {
				t.Fatalf("RequireApproval = %v, 期望 %v（reason=%q）", got.RequireApproval, tc.want, got.Reason)
			}
			if !tc.want {
				return
			}
			if tc.wantRole != "" && got.Role != tc.wantRole {
				t.Errorf("Role = %q, 期望 %q", got.Role, tc.wantRole)
			}
			if tc.wantInWhy != "" && !strings.Contains(got.Reason, tc.wantInWhy) {
				t.Errorf("Reason 应包含 %q，实际 %q", tc.wantInWhy, got.Reason)
			}
			if got.TTL <= 0 {
				t.Errorf("TTL 必须为正，实际 %s", got.TTL)
			}
		})
	}
}

// 工具没配 TTL 时用引擎的兜底值——挂起不能无限期有效。
func TestNeedsApprovalDefaultsTTL(t *testing.T) {
	engine := New()
	spec := baseSpec()
	spec.RequireApproval = true
	// 故意不设 ApprovalTTL

	got := engine.NeedsApproval(spec, nil)
	if got.TTL != engine.DefaultTTL {
		t.Errorf("TTL = %s, 期望兜底值 %s", got.TTL, engine.DefaultTTL)
	}
}

// 内置的写/高危工具必须全部被判为需要审批——这条防止有人改了工具定义却忘了权限。
// realWriteArgs 给出"真要执行"的参数：update_config 默认是预览（不审批），必须显式 dry_run=false。
func realWriteArgs(toolName string) []byte {
	switch toolName {
	case "update_config":
		return []byte(`{"service":"payment-service","key":"timeout_ms","value":"1500","dry_run":false,"confirm":"x"}`)
	case "scale_service":
		return []byte(`{"service":"payment-service","replicas":6}`)
	default:
		return []byte(`{"service":"payment-service"}`)
	}
}

func TestBuiltinWriteToolsRequireApproval(t *testing.T) {
	engine := New()
	for _, name := range []string{"update_config", "restart_service", "scale_service"} {
		// 工具的 Spec 是只读的定义，这里直接从注册表取。
		reg, err := tools.NewFullRegistry(nil, nil)
		if err != nil {
			t.Fatalf("注册工具失败: %v", err)
		}
		tool, ok := reg.Get(name)
		if !ok {
			t.Fatalf("找不到内置工具 %s", name)
		}
		if !engine.NeedsApproval(tool.Spec(), realWriteArgs(name)).RequireApproval {
			t.Errorf("内置工具 %s 必须要求人工审批", name)
		}
	}
}

// TestApprovalCheckRefinesByArguments 钉住一个真实的流程问题：
// update_config 的 dry-run 预览本身不该被审批，否则一次变更会被批两次。
func TestApprovalCheckRefinesByArguments(t *testing.T) {
	engine := New()

	reg, err := tools.NewFullRegistry(nil, nil)
	if err != nil {
		t.Fatalf("注册工具失败: %v", err)
	}
	update, ok := reg.Get("update_config")
	if !ok {
		t.Fatal("找不到 update_config")
	}
	spec := update.Spec()

	cases := []struct {
		name string
		args string
		want bool
	}{
		{"默认预览不需要审批", `{"service":"payment-service","key":"timeout_ms","value":"1500"}`, false},
		{"显式 dry_run=true 不需要审批", `{"service":"payment-service","key":"timeout_ms","value":"1500","dry_run":true}`, false},
		{"真执行需要审批", `{"service":"payment-service","key":"timeout_ms","value":"1500","dry_run":false,"confirm":"x"}`, true},
		{"参数坏掉时保守要求审批", `{"service":`, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := engine.NeedsApproval(spec, []byte(tc.args))
			if got.RequireApproval != tc.want {
				t.Errorf("RequireApproval = %v, 期望 %v（reason=%q）", got.RequireApproval, tc.want, got.Reason)
			}
		})
	}
}

// 高危工具不接受钩子放宽：即使工具写了 ApprovalCheck 返回 false，也必须审批。
func TestDangerousIgnoresApprovalCheck(t *testing.T) {
	engine := New()
	spec := tools.Spec{
		Name:        "restart_service",
		Description: "重启指定服务。属于高危操作，必须经过人工审批。",
		Tier:        tools.TierDangerous,
		Timeout:     5 * time.Second,
		ApprovalCheck: func(json.RawMessage) (bool, string) {
			return false, "" // 故意放宽
		},
	}
	if !engine.NeedsApproval(spec, nil).RequireApproval {
		t.Error("高危工具必须永远要求审批，不能被 ApprovalCheck 放过")
	}
}
