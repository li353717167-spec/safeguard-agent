package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// toolCallBody 是"真实世界"的响应样本：字段名与结构照抄本机本地模型
// （qwen3 系，llama.cpp 风格端点）的实际返回，包括：
//   - 思考型模型才有的 reasoning_content
//   - 参数是 JSON 字符串而不是对象
//   - usage 里缓存命中藏在 OpenAI 风格的嵌套结构中
//
// 把这些真实细节钉进测试，等于把"适配层为什么必须存在"变成了可执行的证据。
const toolCallBody = `{
  "id": "resp-1",
  "model": "test-model",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "",
        "reasoning_content": "先查监控指标",
        "tool_calls": [
          {
            "id": "call_1",
            "type": "function",
            "function": {
              "name": "query_metrics",
              "arguments": "{\"service\":\"payment-service\",\"metric\":\"error_rate\"}"
            }
          }
        ]
      },
      "finish_reason": "tool_calls"
    }
  ],
  "usage": {
    "prompt_tokens": 58,
    "completion_tokens": 59,
    "total_tokens": 117,
    "prompt_tokens_details": {"cached_tokens": 32},
    "completion_tokens_details": {"reasoning_tokens": 40}
  }
}`

func TestChatParsesRealWorldResponse(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(toolCallBody))
	}))
	defer srv.Close()

	client, err := NewOpenAICompat(OpenAICompatConfig{BaseURL: srv.URL, Model: "test-model"})
	if err != nil {
		t.Fatalf("构造客户端失败: %v", err)
	}

	resp, err := client.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "payment-service 最近 5 分钟的错误率是多少？"}},
	})
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}

	if gotPath != "/chat/completions" {
		t.Errorf("请求路径 = %q, 期望 /chat/completions", gotPath)
	}
	if gotAuth != "" {
		t.Errorf("未配置 APIKey 时不应发送 Authorization, 实际 = %q", gotAuth)
	}

	if !resp.HasToolCalls() {
		t.Fatalf("应当识别出工具调用 (finish_reason=%q, content=%q)", resp.FinishReason, resp.Message.Content)
	}
	if resp.FinishReason != FinishToolCalls {
		t.Errorf("FinishReason = %q, 期望 %q", resp.FinishReason, FinishToolCalls)
	}
	if resp.Reasoning != "先查监控指标" {
		t.Errorf("Reasoning = %q, 期望 先查监控指标", resp.Reasoning)
	}
	if resp.Provider != "openai-compat" {
		t.Errorf("Provider = %q", resp.Provider)
	}
	if resp.RequestID != "resp-1" {
		t.Errorf("RequestID = %q, 期望 resp-1（响应头缺失时应回退到 body 的 id）", resp.RequestID)
	}
	if len(resp.Raw) == 0 {
		t.Error("Raw 原始响应不应为空：评测定位\"模型到底返回了什么\"时要靠它")
	}

	// usage：两种缓存字段写法都要能识别
	if resp.Usage.CacheHitTokens != 32 {
		t.Errorf("CacheHitTokens = %d, 期望 32（OpenAI 风格嵌套字段未被归一化）", resp.Usage.CacheHitTokens)
	}
	if resp.Usage.ReasoningTokens != 40 {
		t.Errorf("ReasoningTokens = %d, 期望 40", resp.Usage.ReasoningTokens)
	}
	if resp.Usage.TotalTokens != 117 {
		t.Errorf("TotalTokens = %d, 期望 117", resp.Usage.TotalTokens)
	}

	tc := resp.Message.ToolCalls[0]
	if tc.Function.Name != "query_metrics" {
		t.Errorf("工具名 = %q, 期望 query_metrics", tc.Function.Name)
	}
	if tc.ID != "call_1" {
		t.Errorf("call id = %q, 期望 call_1", tc.ID)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		t.Fatalf("工具参数不是合法 JSON: %v (原始值 %q)", err, tc.Function.Arguments)
	}
	if args["service"] != "payment-service" || args["metric"] != "error_rate" {
		t.Errorf("工具参数解析结果不对: %v", args)
	}
}

func TestChatSendsAuthorizationOnlyWhenAPIKeySet(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	client, err := NewOpenAICompat(OpenAICompatConfig{BaseURL: srv.URL, Model: "m", APIKey: "sk-test"})
	if err != nil {
		t.Fatalf("构造客户端失败: %v", err)
	}
	if _, err := client.Chat(context.Background(), ChatRequest{}); err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, 期望 Bearer sk-test", gotAuth)
	}
}

func TestNewOpenAICompatRejectsMissingEndpointOrModel(t *testing.T) {
	cases := []struct {
		name string
		cfg  OpenAICompatConfig
	}{
		{name: "缺 BaseURL", cfg: OpenAICompatConfig{Model: "m"}},
		{name: "缺 Model", cfg: OpenAICompatConfig{BaseURL: "http://127.0.0.1:1/v1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewOpenAICompat(tc.cfg); err == nil {
				t.Fatal("期望构造失败")
			}
		})
	}
}

func TestChatErrorClassification(t *testing.T) {
	cases := []struct {
		name           string
		status         int
		retryAfter     string
		body           string
		wantCode       string
		wantRetryable  bool
		wantBadRequest bool
		wantRetryAfter time.Duration
	}{
		{
			name: "429 限流可重试", status: http.StatusTooManyRequests,
			retryAfter: "3",
			body:       `{"error":{"message":"rate limited","type":"rate_limit_error","code":"rate_limit_exceeded"}}`,
			wantCode:   "rate_limit_exceeded", wantRetryable: true, wantRetryAfter: 3 * time.Second,
		},
		{
			name: "503 下游不可用可重试", status: http.StatusServiceUnavailable,
			body:     `{"error":{"message":"upstream unavailable"}}`,
			wantCode: "provider_error", wantRetryable: true,
		},
		{
			name: "401 凭据错误不可重试", status: http.StatusUnauthorized,
			body:     `{"error":{"message":"invalid api key"}}`,
			wantCode: "provider_error", wantRetryable: false,
		},
		{
			name: "400 参数错误归类为坏输入", status: http.StatusBadRequest,
			body:           `{"error":{"message":"bad request"}}`,
			wantCode:       "provider_error",
			wantBadRequest: true,
		},
		{
			name: "200 里包着业务错误也必须当失败", status: http.StatusOK,
			body:     `{"error":{"message":"insufficient balance","code":"insufficient_balance"}}`,
			wantCode: "insufficient_balance",
		},
		{
			name: "200 但 choices 为空", status: http.StatusOK,
			body:     `{"choices":[]}`,
			wantCode: "empty_choices",
		},
		{
			name: "非 JSON 的网关错误页", status: http.StatusBadGateway,
			body:     `<html>502 Bad Gateway</html>`,
			wantCode: "http_error", wantRetryable: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			client, err := NewOpenAICompat(OpenAICompatConfig{BaseURL: srv.URL, Model: "m"})
			if err != nil {
				t.Fatalf("构造客户端失败: %v", err)
			}
			_, err = client.Chat(context.Background(), ChatRequest{})
			if err == nil {
				t.Fatal("期望返回错误")
			}

			apiErr, ok := AsAPIError(err)
			if !ok {
				t.Fatalf("期望 *APIError, 实际 %T: %v", err, err)
			}
			if apiErr.Code != tc.wantCode {
				t.Errorf("Code = %q, 期望 %q (message=%q)", apiErr.Code, tc.wantCode, apiErr.Message)
			}
			if apiErr.Retryable() != tc.wantRetryable {
				t.Errorf("Retryable = %v, 期望 %v (status=%d)", apiErr.Retryable(), tc.wantRetryable, apiErr.Status)
			}
			if apiErr.IsBadRequest() != tc.wantBadRequest {
				t.Errorf("IsBadRequest = %v, 期望 %v", apiErr.IsBadRequest(), tc.wantBadRequest)
			}
			if apiErr.RetryAfter != tc.wantRetryAfter {
				t.Errorf("RetryAfter = %s, 期望 %s", apiErr.RetryAfter, tc.wantRetryAfter)
			}
			// 错误可读性是排障成本：必须带上是哪一类失败，而不是一个裸的 "error"。
			if apiErr.Message == "" {
				t.Error("APIError.Message 不应为空")
			}
		})
	}
}

func TestChatTransportErrorIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // 关掉服务，制造连接失败

	client, err := NewOpenAICompat(OpenAICompatConfig{BaseURL: url, Model: "m"})
	if err != nil {
		t.Fatalf("构造客户端失败: %v", err)
	}
	_, err = client.Chat(context.Background(), ChatRequest{})
	apiErr, ok := AsAPIError(err)
	if !ok {
		t.Fatalf("期望 *APIError, 实际 %T: %v", err, err)
	}
	if apiErr.Status != 0 {
		t.Errorf("连接失败时 Status 应为 0, 实际 %d", apiErr.Status)
	}
	if !apiErr.Retryable() {
		t.Error("连接失败应当可重试")
	}
}

func TestChatContextCanceledIsNotRetryable(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // 拖住响应，确保取消发生在返回之前
	}))
	defer srv.Close()
	defer close(release)

	client, err := NewOpenAICompat(OpenAICompatConfig{BaseURL: srv.URL, Model: "m"})
	if err != nil {
		t.Fatalf("构造客户端失败: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	_, err = client.Chat(ctx, ChatRequest{})
	apiErr, ok := AsAPIError(err)
	if !ok {
		t.Fatalf("期望 *APIError, 实际 %T: %v", err, err)
	}
	// 这条区分很关键：超时可以重试，但"调用方主动取消"再重试就是浪费配额、
	// 甚至会在任务已被取消后继续产生副作用。
	if apiErr.Retryable() {
		t.Error("ctx 被主动取消不应被视为可重试")
	}
}

func TestNormalizeArguments(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "空参数补成空对象", in: ``, want: `{}`},
		{name: "协议规定的字符串形态", in: `"{\"a\":1}"`, want: `{"a":1}`},
		{name: "空字符串也补成空对象", in: `""`, want: `{}`},
		{name: "网关把参数直接展开成对象", in: `{ "a" : 1 }`, want: `{"a":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeArguments(json.RawMessage(tc.in)); got != tc.want {
				t.Errorf("normalizeArguments(%s) = %s, 期望 %s", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeToolCallsSkipsNameless(t *testing.T) {
	var withName openAIToolCall
	withName.ID = "call_1"
	withName.Function.Name = "query_metrics"
	withName.Function.Arguments = json.RawMessage(`"{\"service\":\"a\"}"`)

	var nameless openAIToolCall
	nameless.ID = "call_broken"
	nameless.Function.Arguments = json.RawMessage(`"{}"`)

	out := normalizeToolCalls([]openAIToolCall{nameless, withName})
	if len(out) != 1 {
		t.Fatalf("没有函数名的调用应被丢弃, 得到 %d 条: %+v", len(out), out)
	}
	if out[0].Function.Name != "query_metrics" {
		t.Errorf("保留的调用名 = %q", out[0].Function.Name)
	}
	if out[0].Type != "function" {
		t.Errorf("Type 应补默认值 function, 实际 %q", out[0].Type)
	}
	if out[0].Function.Arguments != `{"service":"a"}` {
		t.Errorf("参数未归一化: %q", out[0].Function.Arguments)
	}
	if normalizeToolCalls(nil) != nil {
		t.Error("无工具调用时应返回 nil")
	}
}

func TestNormalizeUsage(t *testing.T) {
	t.Run("DeepSeek 风格直接给字段", func(t *testing.T) {
		var u openAIUsage
		u.PromptTokens, u.CompletionTokens, u.TotalTokens = 10, 20, 30
		u.PromptCacheHitTokens = 7
		got := normalizeUsage(u)
		if got.CacheHitTokens != 7 || got.TotalTokens != 30 {
			t.Errorf("归一化结果不对: %+v", got)
		}
	})

	t.Run("OpenAI 风格嵌套字段", func(t *testing.T) {
		var u openAIUsage
		u.PromptTokens, u.CompletionTokens = 10, 20
		u.PromptTokensDetails.CachedTokens = 5
		got := normalizeUsage(u)
		if got.CacheHitTokens != 5 {
			t.Errorf("CacheHitTokens = %d, 期望 5", got.CacheHitTokens)
		}
		// total 缺失时要能补出来，否则报表里会出现总量小于分项。
		if got.TotalTokens != 30 {
			t.Errorf("TotalTokens = %d, 期望补成 30", got.TotalTokens)
		}
	})
}

func TestNormalizeFinishReason(t *testing.T) {
	cases := map[string]string{
		"stop":          FinishStop,
		"tool_calls":    FinishToolCalls,
		"function_call": FinishToolCalls, // 老端点写法
		"":              FinishStop,
		"length":        FinishLength,
	}
	for in, want := range cases {
		if got := normalizeFinishReason(in); got != want {
			t.Errorf("normalizeFinishReason(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	if got := parseRetryAfter(""); got != 0 {
		t.Errorf("空值应为 0, 实际 %s", got)
	}
	if got := parseRetryAfter("2"); got != 2*time.Second {
		t.Errorf("秒数格式解析错误: %s", got)
	}
	if got := parseRetryAfter("not-a-date"); got != 0 {
		t.Errorf("非法值应为 0, 实际 %s", got)
	}
	date := time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(date); got <= 0 || got > 3*time.Second {
		t.Errorf("HTTP 日期格式解析异常: %s", got)
	}
}

func TestTruncateStringKeepsValidUTF8(t *testing.T) {
	got := truncateString("中文超长文本需要被安全截断", 4)
	if len([]rune(got)) <= 4 {
		t.Errorf("截断后应当带上截断标记: %q", got)
	}
	short := "短"
	if truncateString(short, 4) != short {
		t.Error("未超长时不应改动原字符串")
	}
}
