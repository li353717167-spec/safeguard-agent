package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	defaultHTTPTimeout = 90 * time.Second
	// 响应体上限：防止异常响应（或恶意网关）把内存吃满。
	maxResponseBytes = 4 << 20
	// 错误详情只保留一小段：完整的错误页/HTML 对定位问题没有帮助，只会污染日志。
	maxErrorSnippet = 512
)

// OpenAICompatConfig 描述一个 OpenAI 兼容端点。
//
// 换模型只改这三个字段（BaseURL / APIKey / Model）：
// DeepSeek、通义百炼兼容模式、智谱 GLM、以及任何自建网关都走同一份代码。
type OpenAICompatConfig struct {
	BaseURL string
	APIKey  string
	Model   string
	// HTTPClient 可注入自定义客户端（代理、连接池、测试用的 mock transport）。
	HTTPClient *http.Client
	// ExtraHeaders 用于少数网关需要的额外头（租户标识、路由标签等）。
	ExtraHeaders map[string]string
}

// OpenAICompat 是 OpenAI 兼容协议的适配实现。
//
// 它存在的意义就是把"厂商差异"关在这一层里，让上层只面对 llm 包的稳定类型：
//   - 参数既可能是 JSON 字符串，也可能是被网关直接解析成对象；
//   - 思维链字段名各家不同（DeepSeek 是 reasoning_content）；
//   - 错误信封的 code 有时是字符串有时是数字；
//   - 200 也可能带业务错误体。
//
// 这些都属于"适配层的脏活"，泄漏到业务层就会长出一堆 if provider == "xxx"。
type OpenAICompat struct {
	cfg        OpenAICompatConfig
	httpClient *http.Client
	endpoint   string
}

// NewOpenAICompat 构造适配器，并对配置做 fail-fast 校验。
func NewOpenAICompat(cfg OpenAICompatConfig) (*OpenAICompat, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	apiKey := strings.TrimSpace(cfg.APIKey)
	model := strings.TrimSpace(cfg.Model)

	switch {
	case base == "":
		return nil, errors.New("llm: BaseURL 不能为空")
	case model == "":
		return nil, errors.New("llm: Model 不能为空")
	}
	// APIKey 刻意允许为空：协议本身不要求 Authorization，
	// 本地 llama.cpp / vLLM / LM Studio 以及内网网关通常不校验密钥。
	// 把 key 当必填，是适配层最常见的"写死"。

	hc := cfg.HTTPClient
	if hc == nil {
		// 超时是兜底：正常路径由 ctx 控制。没有超时的 http.Client 是事故源头。
		hc = &http.Client{Timeout: defaultHTTPTimeout}
	}

	cfg.BaseURL = base
	cfg.APIKey = apiKey
	cfg.Model = model
	return &OpenAICompat{
		cfg:        cfg,
		httpClient: hc,
		endpoint:   base + "/chat/completions",
	}, nil
}

// Provider 返回实现标识。
func (c *OpenAICompat) Provider() string { return "openai-compat" }

// Model 返回模型名。
func (c *OpenAICompat) Model() string { return c.cfg.Model }

// Chat 发起一次对话请求。
func (c *OpenAICompat) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	started := time.Now()

	payload, err := json.Marshal(openAIRequest{
		Model:       c.cfg.Model,
		Messages:    req.Messages,
		Tools:       req.Tools,
		ToolChoice:  req.ToolChoice,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
	})
	if err != nil {
		return ChatResponse{}, fmt.Errorf("llm: 请求体序列化失败: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("llm: 构造请求失败: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if c.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
	httpReq.Header.Set("User-Agent", "safeguard-agent/0.1")
	for k, v := range c.cfg.ExtraHeaders {
		httpReq.Header.Set(k, v)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		// 请求没到服务端：分类为传输层错误，并保留底层 err 让 errors.Is 继续可用
		// （D4 会用它区分"超时可重试"与"ctx 被取消不该重试"）。
		return ChatResponse{}, &APIError{
			Provider: c.Provider(),
			Status:   0,
			Code:     "transport_error",
			Message:  "请求模型服务失败",
			Err:      err,
		}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return ChatResponse{}, &APIError{
			Provider: c.Provider(),
			Status:   resp.StatusCode,
			Code:     "read_body_error",
			Message:  "读取模型响应失败",
			Err:      err,
		}
	}
	if len(raw) > maxResponseBytes {
		return ChatResponse{}, &APIError{
			Provider: c.Provider(),
			Status:   resp.StatusCode,
			Code:     "response_too_large",
			Message:  fmt.Sprintf("模型响应超过 %d 字节上限", maxResponseBytes),
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return ChatResponse{}, c.errorFromResponse(resp.StatusCode, resp.Header, raw)
	}

	var parsed openAIResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ChatResponse{}, &APIError{
			Provider: c.Provider(),
			Status:   resp.StatusCode,
			Code:     "decode_error",
			Message:  fmt.Sprintf("响应不是合法 JSON: %v; body=%s", err, truncateString(string(raw), maxErrorSnippet)),
		}
	}

	// 有些网关用 HTTP 200 包着业务错误，只看状态码会把它当成成功。
	if parsed.Error != nil && parsed.Error.Message != "" {
		return ChatResponse{}, c.errorFromEnvelope(resp.StatusCode, resp.Header, parsed.Error)
	}

	if len(parsed.Choices) == 0 {
		return ChatResponse{}, &APIError{
			Provider: c.Provider(),
			Status:   resp.StatusCode,
			Code:     "empty_choices",
			Message:  fmt.Sprintf("%s; body=%s", ErrNoChoices.Error(), truncateString(string(raw), maxErrorSnippet)),
		}
	}

	choice := parsed.Choices[0]
	msg := Message{
		Role:      RoleAssistant,
		Content:   choice.Message.Content,
		ToolCalls: normalizeToolCalls(choice.Message.ToolCalls),
	}
	if choice.Message.Role != "" {
		msg.Role = Role(choice.Message.Role)
	}

	requestID := resp.Header.Get("x-request-id")
	if requestID == "" {
		requestID = parsed.ID
	}

	return ChatResponse{
		Message:      msg,
		Reasoning:    choice.Message.ReasoningContent,
		FinishReason: normalizeFinishReason(choice.FinishReason),
		Usage:        normalizeUsage(parsed.Usage),
		Provider:     c.Provider(),
		Model:        parsed.Model,
		RequestID:    requestID,
		Latency:      time.Since(started),
		Raw:          raw,
	}, nil
}

// errorFromResponse 把非 2xx 响应转成结构化错误。
func (c *OpenAICompat) errorFromResponse(status int, header http.Header, raw []byte) *APIError {
	var envelope openAIErrorEnvelope
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Error != nil {
		return c.errorFromEnvelope(status, header, envelope.Error)
	}

	apiErr := &APIError{
		Provider:   c.Provider(),
		Status:     status,
		Code:       "http_error",
		RequestID:  header.Get("x-request-id"),
		RetryAfter: parseRetryAfter(header.Get("Retry-After")),
		Message:    truncateString(strings.TrimSpace(string(raw)), maxErrorSnippet),
	}
	if apiErr.Message == "" {
		apiErr.Message = http.StatusText(status)
	}
	return apiErr
}

func (c *OpenAICompat) errorFromEnvelope(status int, header http.Header, e *openAIErrorBody) *APIError {
	apiErr := &APIError{
		Provider:   c.Provider(),
		Status:     status,
		Type:       e.Type,
		Code:       codeToString(e.Code),
		Message:    e.Message,
		Param:      e.Param,
		RequestID:  header.Get("x-request-id"),
		RetryAfter: parseRetryAfter(header.Get("Retry-After")),
	}
	if apiErr.Code == "" {
		apiErr.Code = "provider_error"
	}
	if apiErr.Message == "" {
		apiErr.Message = http.StatusText(status)
	}
	return apiErr
}

// ---------------------------------------------------------------------------
// 协议结构（仅本包内部使用，不导出：业务层永远不该直接面对厂商的 wire format）
// ---------------------------------------------------------------------------

type openAIRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Tools       []ToolDef `json:"tools,omitempty"`
	ToolChoice  any       `json:"tool_choice,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
	MaxTokens   *int      `json:"max_tokens,omitempty"`
	Stream      bool      `json:"stream"`
}

type openAIResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role             string           `json:"role"`
			Content          string           `json:"content"`
			ReasoningContent string           `json:"reasoning_content"`
			ToolCalls        []openAIToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage openAIUsage      `json:"usage"`
	Error *openAIErrorBody `json:"error"`
}

// openAIUsage 用命名类型而不是匿名结构体：归一化函数需要一个可引用的类型。
type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// DeepSeek 风格：缓存命中直接给字段。
	PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens int `json:"prompt_cache_miss_tokens"`
	// OpenAI 风格：缓存命中藏在嵌套结构里。
	// 同一件事两种写法，正是"厂商差异必须关在适配层"的实证。
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// openAIToolCall 的 Arguments 用 RawMessage 接收：
// 协议规定它是 JSON 字符串，但确实存在把它直接展开成对象的网关实现。
// 用 RawMessage 接住两种形态，再由 normalizeArguments 统一成字符串。
type openAIToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type openAIErrorEnvelope struct {
	Error *openAIErrorBody `json:"error"`
}

type openAIErrorBody struct {
	Message string          `json:"message"`
	Type    string          `json:"type"`
	Code    json.RawMessage `json:"code"`
	Param   string          `json:"param"`
}

// ---------------------------------------------------------------------------
// 归一化辅助
// ---------------------------------------------------------------------------

func normalizeToolCalls(in []openAIToolCall) []ToolCall {
	if len(in) == 0 {
		return nil
	}
	out := make([]ToolCall, 0, len(in))
	for _, tc := range in {
		if tc.Function.Name == "" {
			// 没有函数名的调用无法执行，直接丢弃：让上层的"没有工具调用"分支去处理，
			// 好过带着一个空名字走到执行器里炸在参数校验上。
			continue
		}
		typ := tc.Type
		if typ == "" {
			typ = "function"
		}
		out = append(out, ToolCall{
			ID:   tc.ID,
			Type: typ,
			Function: FunctionCall{
				Name:      tc.Function.Name,
				Arguments: normalizeArguments(tc.Function.Arguments),
			},
		})
	}
	return out
}

func normalizeArguments(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			if strings.TrimSpace(s) == "" {
				return "{}"
			}
			return s
		}
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err == nil {
		return buf.String()
	}
	return string(raw)
}

// normalizeUsage 把两种缓存字段写法归一，并补全缺失的 total。
func normalizeUsage(u openAIUsage) Usage {
	out := Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
		CacheHitTokens:   u.PromptCacheHitTokens,
		CacheMissTokens:  u.PromptCacheMissTokens,
		ReasoningTokens:  u.CompletionTokensDetails.ReasoningTokens,
	}
	if out.CacheHitTokens == 0 {
		out.CacheHitTokens = u.PromptTokensDetails.CachedTokens
	}
	if out.TotalTokens == 0 {
		out.TotalTokens = out.PromptTokens + out.CompletionTokens
	}
	return out
}

func normalizeFinishReason(in string) string {
	switch in {
	case "function_call": // 部分老端点的写法
		return FinishToolCalls
	case "":
		return FinishStop
	default:
		return in
	}
}

func codeToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return strings.Trim(string(raw), `"`)
}

// parseRetryAfter 支持 Retry-After 的两种合法格式：秒数或 HTTP 日期。
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func truncateString(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…(已截断)"
}
