package executor

import (
	"context"
	"encoding/json"
	"time"

	"github.com/mechrevo/safeguard-agent/internal/tools"
)

// defaultToolTimeout 是工具没配超时时的兜底。
// 注册门禁要求每个工具都必须配 Timeout，这里只是热路径上的防御性默认值。
const defaultToolTimeout = 30 * time.Second

// Config 是执行器配置。
type Config struct {
	Breakers BreakerConfig
	Retry    RetryPolicy

	// now / sleep 可注入：测试里不该真的等 300ms 退避。
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

// DefaultConfig 返回默认配置。
func DefaultConfig() Config {
	return Config{Breakers: DefaultBreakerConfig(), Retry: DefaultRetryPolicy()}
}

func (c Config) normalized() Config {
	c.Breakers = c.Breakers.normalized()
	if c.Retry.BaseDelay <= 0 {
		c.Retry.BaseDelay = 100 * time.Millisecond
	}
	if c.Retry.Jitter == nil {
		c.Retry.Jitter = defaultJitter
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.sleep == nil {
		c.sleep = sleepCtx
	}
	return c
}

// Executor 是工具执行的统一入口。
type Executor struct {
	cfg      Config
	breakers *BreakerSet
}

// New 构造执行器。
func New(cfg Config) *Executor {
	cfg = cfg.normalized()
	return &Executor{cfg: cfg, breakers: newBreakerSet(cfg.Breakers, cfg.now)}
}

// Breakers 暴露熔断器集合（日志、测试，以及以后接指标上报）。
func (e *Executor) Breakers() *BreakerSet { return e.breakers }

// Attempt 记录一次尝试。
type Attempt struct {
	Number   int
	Class    ErrClass
	Err      error
	Duration time.Duration
}

// Outcome 是一次工具执行的最终结果。
type Outcome struct {
	Result   tools.Result
	Error    error
	Class    ErrClass
	Attempts []Attempt
	// Duration 是总耗时（含退避等待）——"这次调用到底拖了多久"要看它。
	Duration time.Duration
	// IdemKey 是这次调用对应的幂等 key（仅落库路径会填）。
	IdemKey string
	// Replayed 表示这次没有真的打下游，而是回放了此前已成功的结果（幂等命中的标志）。
	Replayed bool
	// InProgress 表示这次调用正由另一个执行者处理（没抢到执行权）。
	// 它不是故障也不是参数问题：调用方该做的不是重试，而是稍后再查结果。
	InProgress bool
}

// OK 表示执行成功。
func (o Outcome) OK() bool { return o.Error == nil }

// Retried 表示发生过重试。
func (o Outcome) Retried() bool { return len(o.Attempts) > 1 }

// Run 让 *Executor 满足编排层的 ToolRunner 接口（不落库的简化路径）。
// ref 被忽略：不落库时没有"任务里的位置"这个概念。
func (e *Executor) Run(ctx context.Context, tool tools.Tool, rawArgs json.RawMessage, _ CallRef) Outcome {
	return e.Execute(ctx, tool, rawArgs)
}

// Execute 执行一次工具调用：熔断判定 → 执行 → 分类 → 退避重试。
//
// 顺序有讲究：**先问熔断器，再打下游**。下游已确认不可用时，连第一次请求都不该发出去——
// 重试三次只是让每个请求多等 700ms，还把它打得更死。
func (e *Executor) Execute(ctx context.Context, tool tools.Tool, rawArgs json.RawMessage) Outcome {
	spec := tool.Spec()
	started := e.cfg.now()
	breaker := e.breakers.For(spec.Name)

	// 允许的尝试次数 = 1 + 工具配置的重试次数。
	// 非幂等工具一律只试一次：注册门禁已经拦过，这里是热路径上的第二道保险——
	// 重试一个非幂等操作，就是在重复产生副作用。
	maxAttempts := 1 + spec.MaxRetry
	if !spec.Idempotent {
		maxAttempts = 1
	}

	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = defaultToolTimeout
	}

	var attempts []Attempt
	for n := 1; ; n++ {
		if err := breaker.Allow(); err != nil {
			return Outcome{Error: err, Class: ClassCircuitOpen, Attempts: attempts, Duration: e.cfg.now().Sub(started)}
		}

		callStart := e.cfg.now()
		callCtx, cancel := context.WithTimeout(ctx, timeout)
		result, err := tool.Invoke(callCtx, rawArgs)
		cancel()
		callDuration := e.cfg.now().Sub(callStart)

		if err == nil {
			breaker.OnSuccess()
			attempts = append(attempts, Attempt{Number: n, Class: ClassOK, Duration: callDuration})
			return Outcome{Result: result, Class: ClassOK, Attempts: attempts, Duration: e.cfg.now().Sub(started)}
		}

		class := Classify(err)
		attempts = append(attempts, Attempt{Number: n, Class: class, Err: err, Duration: callDuration})

		if class == ClassRetryable {
			breaker.OnRetryableFailure()
		} else {
			// 参数错误/致命错误说明"对端还活着，只是这条路走不通"：
			// 不记失败（否则模型反复发错参数会把工具熔断掉），但要把半开的试探名额还回去。
			breaker.OnNeutral()
		}

		if class != ClassRetryable || n >= maxAttempts {
			return Outcome{Error: err, Class: class, Attempts: attempts, Duration: e.cfg.now().Sub(started)}
		}

		if err := e.cfg.sleep(ctx, e.cfg.Retry.delay(n)); err != nil {
			// 退避等待中被取消：立刻停手，不再重试。
			return Outcome{Error: err, Class: Classify(err), Attempts: attempts, Duration: e.cfg.now().Sub(started)}
		}
	}
}
