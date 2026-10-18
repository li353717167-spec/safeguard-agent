// Package llm 是模型抽象层。
//
// 它只做一件事：把"对话 + 工具定义 → 模型响应"这件事，收敛成一个不依赖任何厂商的接口。
// 业务代码（orchestrator / tools / executor）只认这个包里的类型，
// 于是"换模型"变成换一个实现，而不是改业务代码——这是简历上那句
// "模型走统一接口，换模型不需要改业务代码"的真实支撑。
//
// 边界约定（很重要）：
//   - 本包不做业务解析。工具参数保持原始 JSON 字符串，合法性校验与参数反序列化
//     属于工具层（D2），因为"参数是否可信"是业务判断，不是传输层职责。
//   - 本包不做重试与熔断。重试策略需要结合异常分类（D4）与幂等（D5），
//     放在执行器里统一处理，模型调用只是它的一个被调用方。
package llm

import (
	"encoding/json"
	"time"
)

// Role 是对话角色。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message 是一条对话消息。
// 一个结构同时承载三种形态：普通文本、模型发起的工具调用、工具的执行结果，
// 因为 OpenAI 兼容协议就是这么定义的——把差异留在适配层，不要泄漏给业务层。
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`

	// Assistant 发起工具调用时填充。
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`

	// Tool 角色回填结果时必须带上对应的 call id，否则模型无法把结果和请求对上。
	ToolCallID string `json:"tool_call_id,omitempty"`
	Name       string `json:"name,omitempty"`
}

// ToolCall 是模型发起的一次工具调用。
type ToolCall struct {
	// ID 由模型生成，用于把"调用"与"结果"配对。注意它不等于幂等 key：
	// ID 每次调用都可能不同，幂等 key 必须由 (task, step, tool, 参数) 确定性地算出来。
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall 是被调用的函数名与参数。
type FunctionCall struct {
	Name string `json:"name"`
	// Arguments 保持原始 JSON 字符串，不在这里反序列化。
	// 模型返回的 JSON 可能是非法的、带多余字段的、甚至是提示注入构造的，
	// 这些都必须由工具层带着"不信任"的态度去校验。
	Arguments string `json:"arguments"`
}

// ToolDef 是注入给模型的工具定义。
// 注意 Description 与 Parameters 的质量直接决定模型选不选得对工具——
// 工具描述本身就是 prompt 的一部分。
type ToolDef struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction 描述一个函数的对外契约。
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ChatRequest 是一次对话请求。它是厂商无关的：任何厂商私有字段都必须由适配层消化。
type ChatRequest struct {
	Messages    []Message
	Tools       []ToolDef
	ToolChoice  any // "auto" / "none" / {"type":"function",...}；nil 表示交给服务端默认
	Temperature *float64
	MaxTokens   *int
}

// Usage 是 token 用量。缓存命中字段来自支持 prompt 缓存的厂商（如 DeepSeek），
// 没有该能力的厂商留 0——成本核算与"上下文有没有被缓存"都看这里。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CacheHitTokens   int `json:"prompt_cache_hit_tokens"`
	CacheMissTokens  int `json:"prompt_cache_miss_tokens"`
	// ReasoningTokens 是"思考"消耗的 token（推理模型才有），它同样计费，
	// 所以必须能从上报里看到——否则成本对不上账。
	ReasoningTokens int `json:"reasoning_tokens"`
}

// 结束原因。这些字符串在不同厂商间存在拼写差异，适配层负责归一化到这里。
const (
	FinishStop          = "stop"
	FinishLength        = "length"
	FinishToolCalls     = "tool_calls"
	FinishContentFilter = "content_filter"
)

// ChatResponse 是一次对话响应。
type ChatResponse struct {
	Message      Message
	Reasoning    string // 思维链（如有）。刻意不并入 Message：它不进入下一轮上下文
	FinishReason string
	Usage        Usage

	// 以下字段用于可观测性与排障，不参与业务判断。
	Provider  string
	Model     string
	RequestID string
	Latency   time.Duration
	Raw       json.RawMessage // 原始响应体，评测定位"模型到底返回了什么"时要用
}

// HasToolCalls 表示模型要求调用工具。
// 判断依据是"有没有 tool_calls"，而不是 finish_reason ——
// 部分厂商在有多轮工具调用时 finish_reason 仍返回 "stop"，
// 只认 finish_reason 的循环会提前退出（这是很常见的坑）。
func (r ChatResponse) HasToolCalls() bool {
	return len(r.Message.ToolCalls) > 0
}
