package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// openAIServer 起一个最小可用的 OpenAI 兼容端点。
// fail=true 时返回 503，用来模拟"这个端点挂了"。
func openAIServer(t *testing.T, model string, fail bool) (*httptest.Server, *int) {
	t.Helper()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{"message": "upstream unavailable", "type": "server_error"},
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-test",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "来自 " + model},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// 用**真实 HTTP、真实客户端**验证切换：首选端点 503，备用端点正常。
//
// 这比 mock 更有说服力：它走完了整个 HTTP 往返、错误解析与重试路径，
// 证明"换端点"不是只在假对象之间转发。
func TestRouterFailoverOverRealHTTP(t *testing.T) {
	dead, deadHits := openAIServer(t, "primary-model", true)
	live, liveHits := openAIServer(t, "backup-model", false)

	primary, err := NewOpenAICompat(OpenAICompatConfig{
		BaseURL: dead.URL + "/v1", Model: "primary-model",
	})
	if err != nil {
		t.Fatalf("构造首选客户端失败: %v", err)
	}
	backup, err := NewOpenAICompat(OpenAICompatConfig{
		BaseURL: live.URL + "/v1", Model: "backup-model",
	})
	if err != nil {
		t.Fatalf("构造备用客户端失败: %v", err)
	}

	var failovers int
	router, err := NewRouter(RouterConfig{
		Endpoints: []Endpoint{
			{Name: "primary", Client: primary},
			{Name: "backup", Client: backup},
		},
		FailThreshold: 1, // 一次失败就切，让测试确定
		Cooldown:      30 * time.Second,
		OnFailover:    func(from, to, reason string) { failovers++ },
	})
	if err != nil {
		t.Fatalf("构造路由器失败: %v", err)
	}

	resp, err := router.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "你好"}},
	})
	if err != nil {
		t.Fatalf("应自动切换到备用端点，实际失败: %v", err)
	}
	if resp.Message.Content != "来自 backup-model" {
		t.Errorf("应答内容 = %q，期望来自备用端点", resp.Message.Content)
	}
	if resp.Model != "backup-model" {
		t.Errorf("响应模型 = %q，期望 backup-model（归因要准确）", resp.Model)
	}
	if *deadHits != 1 || *liveHits != 1 {
		t.Errorf("调用次数 primary=%d backup=%d", *deadHits, *liveHits)
	}
	if failovers != 1 {
		t.Errorf("切换回调应触发 1 次，实际 %d", failovers)
	}

	// 粘性：第二次直接走备用，不再打已经挂掉的首选
	if _, err := router.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "再问一次"}},
	}); err != nil {
		t.Fatalf("第二次调用失败: %v", err)
	}
	if *deadHits != 1 {
		t.Errorf("粘性失效：已挂的首选被再次调用（%d 次）", *deadHits)
	}
	if *liveHits != 2 {
		t.Errorf("备用端点应被调用 2 次，实际 %d", *liveHits)
	}
}
