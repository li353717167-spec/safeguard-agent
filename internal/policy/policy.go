// Package policy 是策略引擎：**纯规则**判定一次工具调用是否需要人工审批。
//
// 它刻意只读工具的静态属性（Tier / RequireApproval / DryRun），不看参数、不看模型输出。
//
// 为什么不看参数：参数解释是工具的职责。策略层一旦开始理解业务参数，
// 就等于把业务判断搬进了权限系统——而权限系统恰恰应该是最简单、最不容易错的那一层。
// 需要"改动幅度超过阈值也强制审批"这类动态规则时，正确做法是：
//   - 工具把这类判断做进 dry-run 预览，让审批人看见 diff；
//   - 或者工具在自己的 Spec 里把 RequireApproval 收紧，
//
// 而不是让策略引擎去解析 args。
package policy

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mechrevo/safeguard-agent/internal/tools"
)

// Decision 是策略判定结果。
type Decision struct {
	RequireApproval bool
	// Reason 会写进审批工单，让人知道"为什么这次要审批"。
	Reason string
	Role   string
	TTL    time.Duration
}

// Engine 是策略引擎。
type Engine struct {
	// DefaultTTL 是工具没配 ApprovalTTL 时的兜底。
	DefaultTTL time.Duration
}

// New 构造策略引擎。
func New() *Engine { return &Engine{DefaultTTL: 15 * time.Minute} }

// NeedsApproval 判定是否需要人工审批。
//
// 判定顺序（high → low）：
//  1. **高危工具永远审批**，不接受任何钩子放宽——这是兜底，防止某个工具不小心把审批关掉；
//  2. 工具提供了 ApprovalCheck 钩子时，由**工具自己**按本次参数细化判定
//     （典型场景：dry-run 预览不需要审批，只有真执行才需要）；
//  3. 否则用静态规则：声明需要审批 / 写操作没有 dry-run 预览能力。
//
// 注意：引擎自己从不解析参数语义——它只把参数原样交给工具的钩子。
// 这样"权限是工具的属性"这条原则依然成立：知识在工具里，规则在引擎里。
func (e *Engine) NeedsApproval(spec tools.Spec, rawArgs json.RawMessage) Decision {
	ttl := spec.ApprovalTTL
	if ttl <= 0 {
		ttl = e.DefaultTTL
	}

	// ① 高危：永远审批
	if spec.Tier == tools.TierDangerous {
		return Decision{
			RequireApproval: true,
			Reason:          fmt.Sprintf("%s 的风险等级是高危（TierDangerous），默认必须审批", spec.Name),
			Role:            spec.ApprovalRole,
			TTL:             ttl,
		}
	}

	// ② 工具按参数细化
	if spec.ApprovalCheck != nil {
		require, reason := spec.ApprovalCheck(rawArgs)
		if !require {
			return Decision{}
		}
		if strings.TrimSpace(reason) == "" {
			reason = fmt.Sprintf("%s 判定本次调用需要人工审批", spec.Name)
		}
		return Decision{RequireApproval: true, Reason: reason, Role: spec.ApprovalRole, TTL: ttl}
	}

	// ③ 静态规则
	if spec.RequireApproval {
		return Decision{
			RequireApproval: true,
			Reason:          fmt.Sprintf("%s 声明为需要审批的操作（角色=%s）", spec.Name, spec.ApprovalRole),
			Role:            spec.ApprovalRole,
			TTL:             ttl,
		}
	}

	if spec.Tier == tools.TierWrite && !spec.DryRun {
		return Decision{
			RequireApproval: true,
			Reason:          fmt.Sprintf("%s 是写操作且没有 dry-run 预览，审批人无法判断影响面", spec.Name),
			Role:            spec.ApprovalRole,
			TTL:             ttl,
		}
	}

	return Decision{RequireApproval: false}
}
