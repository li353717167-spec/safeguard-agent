// Command fake-llm 是一个脚本化的 OpenAI 兼容端点，只用于**演示与联调**。
//
// 它存在的理由：演示"人工审批 + 断点续跑"这条链需要模型先发起一次高危调用，
// 而真实模型不可控（也可能没配额）。用脚本模型，同一条命令每次都能复现同样的轨迹，
// 但走的是**真实的编排、权限、审批、幂等、审计与数据库**——只有"模型说什么"是脚本。
//
// 用法：
//
//	go run ./cmd/fake-llm -addr 127.0.0.1:18090
//	SAFEGUARD_LLM_BASE_URL=http://127.0.0.1:18090/v1 SAFEGUARD_LLM_MODEL=demo-scripted \
//	  go run ./cmd/agent -ask "重启 checkout-service"
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// 脚本规则（刻意简单，只覆盖演示需要的两个状态）：
//   - 对话里还没有工具结果 → 发起一次 restart_service（会被权限层拦下、挂起等审批）
//   - 已经有工具结果 → 给一句最终答复
func decide(req chatRequest) map[string]any {
	hasToolResult := false
	lastUser := ""
	for _, m := range req.Messages {
		if m.Role == "tool" {
			hasToolResult = true
		}
		if m.Role == "user" {
			lastUser = m.Content
		}
	}

	if !hasToolResult {
		if !strings.Contains(lastUser, "重启") {
			return textResponse("我只能演示'重启服务'这一类请求；试试：重启 checkout-service")
		}
		return toolCallResponse("restart_service",
			`{"service":"checkout-service","reason":"演示：用户要求重启"}`)
	}
	return textResponse("checkout-service 已重启完成，健康检查通过（这是脚本模型给的答复，演示用）。")
}

func toolCallResponse(name, args string) map[string]any {
	return map[string]any{
		"id": "chatcmpl-demo", "object": "chat.completion", "created": time.Now().Unix(),
		"model": "demo-scripted",
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{map[string]any{
					"id": "call_demo_1", "type": "function",
					"function": map[string]any{"name": name, "arguments": args},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]any{"prompt_tokens": 120, "completion_tokens": 20, "total_tokens": 140},
	}
}

func textResponse(text string) map[string]any {
	return map[string]any{
		"id": "chatcmpl-demo", "object": "chat.completion", "created": time.Now().Unix(),
		"model": "demo-scripted",
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 200, "completion_tokens": 30, "total_tokens": 230},
	}
}

func main() {
	addr := flag.String("addr", "127.0.0.1:18090", "监听地址")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"object": "list", "data": []any{
			map[string]any{"id": "demo-scripted", "object": "model"},
		}})
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":{"message":"bad json"}}`, http.StatusBadRequest)
			return
		}
		log.Printf("scripted call: messages=%d", len(req.Messages))
		writeJSON(w, decide(req))
	})

	log.Printf("脚本模型已启动：http://%s/v1 （仅用于演示）", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Printf("启动失败: %v", err)
		os.Exit(1)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
