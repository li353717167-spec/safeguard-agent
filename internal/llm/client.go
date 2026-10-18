package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Client 是模型抽象层的唯一入口。
//
// 业务层依赖这个接口，而不是任何具体实现：换模型 = 换实现，业务代码零改动。
// 接口刻意保持最小——只有 Chat。多一个方法就多一处需要所有厂商适配的负担，
// 而"对话"已经是能力上限（工具调用是对话的一部分，不是独立能力）。
type Client interface {
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
	// Provider 返回实现标识，用于日志、指标与评测报告归因（"这条结论是哪个模型跑的"）。
	Provider() string
	// Model 返回当前模型名。
	Model() string
}

// APIError 是模型调用失败的类型化错误。
//
// 为什么要类型化：重试与熔断的策略完全取决于"这是哪一类失败"。
// 靠 strings.Contains(err.Error(), "timeout") 判断的代码，会在某个厂商
// 调整错误文案的那天静默失效——分类依据必须是状态码和结构化字段。
// D4 的 Classify() 直接对这个类型做 errors.As。
type APIError struct {
	Provider string
	// Status 是 HTTP 状态码；0 表示请求根本没到服务端（DNS/连接/超时）。
	Status     int
	Code       string
	Type       string
	Message    string
	Param      string
	RequestID  string
	RetryAfter time.Duration // 服务端显式给的退避建议（429/503 常见）
	// Err 保留底层错误（网络错误、context 取消等），使 errors.Is 仍然可用。
	Err error
}

func (e *APIError) Error() string {
	var b strings.Builder
	b.WriteString("llm: ")
	if e.Provider != "" {
		b.WriteString(e.Provider)
		b.WriteString(" ")
	}
	if e.Status > 0 {
		fmt.Fprintf(&b, "HTTP %d ", e.Status)
	}
	if e.Code != "" {
		fmt.Fprintf(&b, "code=%s ", e.Code)
	}
	b.WriteString(e.Message)
	if e.RequestID != "" {
		fmt.Fprintf(&b, " (request_id=%s)", e.RequestID)
	}
	if e.Err != nil {
		fmt.Fprintf(&b, ": %v", e.Err)
	}
	return b.String()
}

// Unwrap 让 errors.Is / errors.As 能穿透到网络层错误，
// 例如 errors.Is(err, context.DeadlineExceeded) 依然成立。
func (e *APIError) Unwrap() error { return e.Err }

// Retryable 给出"这一层"的重试建议：网络未达、429、5xx 可重试。
//
// 注意边界：这里只有"传输层是否值得再试一次"，不包含业务语义与幂等判断。
// 完整的四分类（可重试/致命/参数错/熔断）是执行器的职责（D4），
// 因为同一个 400 在"模型调用"和"重启服务"两个场景下含义完全不同。
func (e *APIError) Retryable() bool {
	if e.Status == 0 {
		// 网络层问题：连接被拒、超时、连接被重置。ctx 主动取消不算可重试。
		return !errors.Is(e.Err, context.Canceled)
	}
	switch {
	case e.Status == 429:
		return true
	case e.Status >= 500 && e.Status <= 599:
		return true
	default:
		return false
	}
}

// IsUnauthorized 表示凭据问题：重试没有意义，必须换 key 或修配置。
func (e *APIError) IsUnauthorized() bool { return e.Status == 401 || e.Status == 403 }

// IsBadRequest 表示请求本身有问题：参数、schema、上下文超长等，同样不该重试。
func (e *APIError) IsBadRequest() bool { return e.Status == 400 || e.Status == 404 || e.Status == 422 }

// AsAPIError 便于调用方做 errors.As 的语法糖。
func AsAPIError(err error) (*APIError, bool) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr, true
	}
	return nil, false
}

// ErrNoChoices 表示服务端返回了 200 但没有可用结果，属于协议层面的异常响应。
var ErrNoChoices = errors.New("llm: 响应中没有 choices")
