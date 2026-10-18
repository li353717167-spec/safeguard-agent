package eval

import (
	"context"
	"fmt"
	"strings"

	"github.com/mechrevo/safeguard-agent/internal/llm"
)

// ---------------------------------------------------------------------------
// 理想模型（oracle）：按"这道题的期望"行动
// ---------------------------------------------------------------------------

// oracleModel 是一个"完美 Agent"：第一轮发出用例要求的全部工具调用，然后给出含关键信息的回答。
//
// 它的用途不是假装模型很强，而是**验证判分器本身**：
// 如果连理想轨迹都判不通过，那判分逻辑就是错的。这就是 D10 里"CI 用假模型"的含义。
type oracleModel struct {
	c      Case
	rounds int
}

// NewOracle 构造理想模型。
func NewOracle(c Case) llm.Client { return &oracleModel{c: c} }

func (m *oracleModel) Provider() string { return "oracle" }
func (m *oracleModel) Model() string    { return "oracle-perfect-agent" }

func (m *oracleModel) Chat(_ context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	m.rounds++

	// 第一轮：把必需的工具一次发出去（模拟"一次想清楚"的理想行为）。
	if m.rounds == 1 && len(m.c.Expect.MustCallTools) > 0 {
		calls := make([]llm.ToolCall, 0, len(m.c.Expect.MustCallTools))
		for i, name := range m.c.Expect.MustCallTools {
			calls = append(calls, llm.ToolCall{
				ID:       fmt.Sprintf("call_%d", i+1),
				Type:     "function",
				Function: llm.FunctionCall{Name: name, Arguments: oracleArgs(name)},
			})
		}
		return llm.ChatResponse{
			Message:      llm.Message{Role: llm.RoleAssistant, ToolCalls: calls},
			FinishReason: llm.FinishToolCalls,
		}, nil
	}

	// 之后：给出含关键信息的回答。
	return llm.ChatResponse{
		Message:      llm.Message{Role: llm.RoleAssistant, Content: oracleAnswer(m.c)},
		FinishReason: llm.FinishStop,
	}, nil
}

// oracleArgs 给出每个工具"能被校验通过"的参数。
func oracleArgs(tool string) string {
	switch tool {
	case "list_services":
		return `{}`
	case "query_metrics":
		return `{"service":"checkout-service","metric":"error_rate","window":"30m"}`
	case "query_config":
		return `{"service":"checkout-service"}`
	case "update_config":
		// 默认 dry-run：预览不需要审批，正好覆盖"写操作的预览不该被拦"。
		return `{"service":"checkout-service","key":"timeout_ms","value":"2000"}`
	case "restart_service":
		return `{"service":"checkout-service","reason":"错误率上升，按预案重启"}`
	case "scale_service":
		return `{"service":"checkout-service","replicas":10}`
	default:
		return `{}`
	}
}

// oracleAnswer 拼出一个"包含全部关键内容"的回答。
//
// 它只包含用例要求出现的内容，绝不包含禁止出现的内容——
// 所以它能通过 AnswerNotContains 的检查，这正是"理想轨迹"的含义。
func oracleAnswer(c Case) string {
	contains := c.Expect.AnswerContains
	if len(contains) == 0 && len(c.Expect.AnswerContainsAny) > 0 {
		// OR 断言：理想模型只取第一个说法就能满足。
		contains = c.Expect.AnswerContainsAny[:1]
	}
	if c.Expect.ExpectApproval != nil && *c.Expect.ExpectApproval {
		// 挂起时最终回答是由编排层生成的，这里不会被用到；留作兜底。
		return "该操作需要人工审批，已挂起，审批前不会执行。"
	}
	if len(contains) == 0 {
		return "已完成检查。"
	}
	return "根据查询结果：" + strings.Join(contains, "；")
}

// ---------------------------------------------------------------------------
// 坏模型：用来证明判分器**会**判失败
// ---------------------------------------------------------------------------

// brokenModel 永远尝试直接重启服务，并声称已经重启——
// 它应该把陷阱题全部判失败（绕过了审批、还谎称执行成功）。
type brokenModel struct{ rounds int }

// NewBroken 构造坏模型。
func NewBroken() llm.Client { return &brokenModel{} }

func (m *brokenModel) Provider() string { return "broken" }
func (m *brokenModel) Model() string    { return "broken-always-restart" }

func (m *brokenModel) Chat(_ context.Context, _ llm.ChatRequest) (llm.ChatResponse, error) {
	m.rounds++
	if m.rounds == 1 {
		return llm.ChatResponse{
			Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
				ID:       "call_1",
				Type:     "function",
				Function: llm.FunctionCall{Name: "restart_service", Arguments: `{"service":"payment-service"}`},
			}}},
			FinishReason: llm.FinishToolCalls,
		}, nil
	}
	return llm.ChatResponse{
		Message:      llm.Message{Role: llm.RoleAssistant, Content: "已重启 payment-service，服务已恢复。"},
		FinishReason: llm.FinishStop,
	}, nil
}
