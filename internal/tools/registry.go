package tools

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mechrevo/safeguard-agent/internal/llm"
)

const (
	// minDescriptionRunes 是工具描述的长度下限。
	// 工具描述本身就是 prompt：写不清楚，模型就会选错工具或编造参数，
	// 而这类问题在代码 review 里几乎看不出来——所以把它做成启动期断言。
	minDescriptionRunes = 20
	// maxToolRetry 是允许配置的重试上限，与 D4 的重试策略保持一致（重试 2~3 次）。
	maxToolRetry = 3
)

// toolNamePattern 是工具名规则。
// OpenAI 兼容端点对函数名有字符与长度限制；名字同时也是模型判断"这是干什么的"的一部分。
var toolNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{2,63}$`)

// Registry 是工具注册中心。
//
// 它最重要的职责不是"存工具"，而是**在启动期把安全约束断言出来**：
// 名字规范、描述质量、schema 合法、风险等级与审批策略不矛盾、非幂等工具不许自动重试。
// 这些一旦写错，运行时才暴露的代价极高（模型选错工具、危险操作绕过审批），
// 所以宁可在启动时就起不来。
type Registry struct {
	tools map[string]Tool
}

// NewRegistry 构造一个空注册中心。
func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}}
}

// Register 注册一个工具，并在注册时执行全部门禁检查。
func (r *Registry) Register(t Tool) error {
	spec := t.Spec()

	if !toolNamePattern.MatchString(spec.Name) {
		return fmt.Errorf("tools: 工具名 %q 不合法（要求 ^[a-z][a-z0-9_]{2,63}$）", spec.Name)
	}
	if _, dup := r.tools[spec.Name]; dup {
		return fmt.Errorf("tools: 工具 %q 重复注册", spec.Name)
	}
	if n := utf8.RuneCountInString(strings.TrimSpace(spec.Description)); n < minDescriptionRunes {
		return fmt.Errorf("tools: 工具 %q 的描述只有 %d 字（至少 %d 字）：工具描述本身就是 prompt，写不清楚模型必然选错",
			spec.Name, n, minDescriptionRunes)
	}
	if len(spec.Parameters) == 0 || !json.Valid(spec.Parameters) {
		return fmt.Errorf("tools: 工具 %q 的参数 schema 不是合法 JSON", spec.Name)
	}

	// 权限与风险等级不允许自相矛盾。
	if spec.Tier == TierDangerous && !spec.RequireApproval {
		return fmt.Errorf("tools: 高危工具 %q 必须 RequireApproval=true——权限是工具的静态属性，不能自相矛盾", spec.Name)
	}
	if spec.RequireApproval && spec.ApprovalTTL <= 0 {
		return fmt.Errorf("tools: 需要审批的工具 %q 必须设置 ApprovalTTL：挂起不能无限期占用", spec.Name)
	}

	// 执行策略的合法性。
	if spec.Timeout <= 0 {
		return fmt.Errorf("tools: 工具 %q 必须设置 Timeout（没有超时的调用会把整个循环拖死）", spec.Name)
	}
	if spec.MaxRetry < 0 || spec.MaxRetry > maxToolRetry {
		return fmt.Errorf("tools: 工具 %q 的 MaxRetry=%d 超出 [0,%d]", spec.Name, spec.MaxRetry, maxToolRetry)
	}
	if !spec.Idempotent && spec.MaxRetry > 0 {
		return fmt.Errorf("tools: 非幂等工具 %q 不允许配置 MaxRetry=%d——重试会重复产生副作用，重试的前提是幂等",
			spec.Name, spec.MaxRetry)
	}

	r.tools[spec.Name] = t
	return nil
}

// Get 按名字取工具。
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Len 返回工具数量。
func (r *Registry) Len() int { return len(r.tools) }

// Names 返回按字典序排序的工具名。
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Schemas 生成给模型看的工具定义。
//
// ⚠️ 这里**只导出** name / description / parameters。
// Tier、RequireApproval、ApprovalTTL、重试与超时策略永远不进 prompt：
// 模型知道"能做什么"，但不知道也不需要知道"这事要不要审批"。
// 把审批标记放进 schema，等于把权限判断的线索交给了概率模型。
//
// 顺序按工具名排序：prompt 稳定 → 前缀缓存命中率稳定 → 评测可复现。
func (r *Registry) Schemas() []llm.ToolDef {
	names := r.Names()
	out := make([]llm.ToolDef, 0, len(names))
	for _, name := range names {
		spec := r.tools[name].Spec()
		out = append(out, llm.ToolDef{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        spec.Name,
				Description: spec.Description,
				Parameters:  spec.Parameters,
			},
		})
	}
	return out
}

// Entry 是工具清单的一行，供 CLI（-tools）与文档展示。
type Entry struct {
	Name            string
	Tier            string
	RequireApproval bool
	ApprovalRole    string
	ApprovalTTL     time.Duration
	Timeout         time.Duration
	MaxRetry        int
	Idempotent      bool
	DryRun          bool
	Description     string
}

// Inventory 返回全部工具的清单（含权限与执行策略），按名字排序。
func (r *Registry) Inventory() []Entry {
	names := r.Names()
	out := make([]Entry, 0, len(names))
	for _, name := range names {
		spec := r.tools[name].Spec()
		out = append(out, Entry{
			Name:            spec.Name,
			Tier:            spec.Tier.String(),
			RequireApproval: spec.RequireApproval,
			ApprovalRole:    spec.ApprovalRole,
			ApprovalTTL:     spec.ApprovalTTL,
			Timeout:         spec.Timeout,
			MaxRetry:        spec.MaxRetry,
			Idempotent:      spec.Idempotent,
			DryRun:          spec.DryRun,
			Description:     spec.Description,
		})
	}
	return out
}
