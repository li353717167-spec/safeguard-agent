// Package executor 是工具执行器：异常分类 → 熔断 → 退避重试 → 结果。
//
// 它把两个经常被混为一谈的机制分开放：
//
//	「这次失败要不要再试一次？」  → 异常分类（本文件）
//	「下游整体是不是挂了？」      → 熔断器（circuit.go）
//
// 以及一条纪律：**重试只对可重试的失败生效，并且只对幂等工具生效。**
package executor

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/mechrevo/safeguard-agent/internal/llm"
	"github.com/mechrevo/safeguard-agent/internal/tools"
)

// ErrClass 是异常四分类。
//
// 分类决定三件不同的事：
//   - 要不要重试：只有 ClassRetryable 重试；
//   - 要不要计入熔断：**只有 ClassRetryable 计入**（参数错误是模型的错，不是下游的健康度）；
//   - 回喂模型时怎么标注：bad_args 表示"你能修"，其余表示"你修不了，别乱改参数"。
type ErrClass int

const (
	// ClassOK 表示成功（记录用，不作为失败分类）。
	ClassOK ErrClass = iota
	// ClassRetryable：超时、5xx、429、连接类失败——单次抖动，值得再试。
	ClassRetryable
	// ClassFatal：认证失败、以及一切无法归类的失败——重试只是浪费时间。
	ClassFatal
	// ClassBadInput：参数错误、业务校验失败——模型能自己修。
	ClassBadInput
	// ClassCircuitOpen：熔断中——不再真的打下游，快速失败。
	ClassCircuitOpen
)

func (c ErrClass) String() string {
	switch c {
	case ClassOK:
		return "ok"
	case ClassRetryable:
		return "retryable"
	case ClassFatal:
		return "fatal"
	case ClassBadInput:
		return "bad_input"
	case ClassCircuitOpen:
		return "circuit_open"
	default:
		return "unknown"
	}
}

// CircuitOpenError 表示该工具的熔断器处于打开状态，请求被快速拒绝。
type CircuitOpenError struct {
	Tool       string
	Failures   int
	Window     time.Duration
	RetryAfter time.Duration
}

func (e *CircuitOpenError) Error() string {
	return fmt.Sprintf("工具 %s 的熔断器已打开（%s 窗口内失败 %d 次），快速失败以免继续打下游；约 %s 后进入半开试探",
		e.Tool, e.Window, e.Failures, e.RetryAfter)
}

// IsCircuitOpen 判断错误是否来自熔断器。
func IsCircuitOpen(err error) (*CircuitOpenError, bool) {
	var target *CircuitOpenError
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

// Classify 把错误归到四类之一。
//
// ⚠️ 判据一律是**错误类型**（errors.As / errors.Is），不是错误文案。
// 靠 strings.Contains(err.Error(), "timeout") 做重试的代码，会在某个依赖调整了
// 错误消息的那天静默失效——而且失效方式是"再也不重试"，没人会发现。
func Classify(err error) ErrClass {
	if err == nil {
		return ClassOK
	}

	// 熔断器自己抛出的快速失败。
	if _, ok := IsCircuitOpen(err); ok {
		return ClassCircuitOpen
	}

	// 参数问题：模型能自己修，回喂即可。
	var argErr *tools.ArgError
	if errors.As(err, &argErr) {
		return ClassBadInput
	}
	// 查不到：正常情况下工具层会把它转成成功结果（见 tools 包）。
	// 漏到这里说明有人绕过了那个约定——归类为坏输入，让模型调整查询，而不是当故障。
	if _, notFound := tools.IsNotFound(err); notFound {
		return ClassBadInput
	}

	// 调用方主动取消：**不可重试**。重试只会在一个已经不要结果的任务上继续产生副作用。
	if errors.Is(err, context.Canceled) {
		return ClassFatal
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ClassRetryable
	}

	// 模型侧的类型化错误（llm 包已把 HTTP 状态码收进去）。
	var apiErr *llm.APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.Status == 429:
			return ClassRetryable
		case apiErr.Status >= 500:
			return ClassRetryable
		case apiErr.Status == 401 || apiErr.Status == 403:
			return ClassFatal
		case apiErr.Status == 400 || apiErr.Status == 404 || apiErr.Status == 422:
			return ClassBadInput
		case apiErr.Status == 0:
			// 请求根本没到服务端：网络层问题。但底层若是"被取消"，就不是抖动。
			if errors.Is(apiErr.Err, context.Canceled) {
				return ClassFatal
			}
			return ClassRetryable
		default:
			return ClassFatal
		}
	}

	// 数据库连接层：连接已坏/已失效——换一条连接往往就好了。
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, mysql.ErrInvalidConn) {
		return ClassRetryable
	}
	// 网络层：连接被拒/被重置/读写超时都归到 *net.OpError。
	// 这恰恰是熔断要观察的信号——**下游整体不可用**，而不是某一次请求抖了一下。
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return ClassRetryable
	}

	// 兜底：不认识的错误一律当致命（fail closed）。
	// 宁可少重试几次，也不要把一个确定性失败重试三遍、把下游打得更死。
	return ClassFatal
}
