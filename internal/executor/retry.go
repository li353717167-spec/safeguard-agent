package executor

import (
	"context"
	"math/rand"
	"time"
)

// RetryPolicy 是退避重试策略。
//
// 具体数字：
//
//	第 1 次失败 → 等 100ms + 抖动(0~50ms)
//	第 2 次失败 → 等 200ms + 抖动
//	第 3 次失败 → 等 400ms + 抖动
//
// 抖动的作用：避免多个实例/多个请求在同一刻一起重试，把刚缓过来的下游再打一次（惊群）。
// MaxDelay 兜住指数增长的尾巴。
type RetryPolicy struct {
	BaseDelay time.Duration
	MaxDelay  time.Duration
	// Jitter 返回 [0, max) 内的抖动。抽成函数是为了测试里能确定性地断言退避曲线。
	Jitter func(max time.Duration) time.Duration
}

// DefaultRetryPolicy 返回默认退避策略。
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		BaseDelay: 100 * time.Millisecond,
		MaxDelay:  2 * time.Second,
		Jitter:    defaultJitter,
	}
}

func defaultJitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(max)))
}

// delay 返回第 n 次失败后的等待时长（n 从 1 开始）。
func (p RetryPolicy) delay(n int) time.Duration {
	base := p.BaseDelay
	if base <= 0 {
		base = 100 * time.Millisecond
	}

	shift := n - 1
	if shift < 0 {
		shift = 0
	}
	// 上限截断，避免 int64 左移溢出（实际上 MaxDelay 会先把它压住）。
	if shift > 10 {
		shift = 10
	}
	d := base * time.Duration(1<<shift)
	if p.MaxDelay > 0 && d > p.MaxDelay {
		d = p.MaxDelay
	}
	// 抖动上限取基础延迟的一半：让抖动修饰退避，而不是反过来主导退避。
	if p.Jitter != nil {
		d += p.Jitter(base / 2)
	}
	return d
}

// sleepCtx 是可被取消的等待。
//
// 用它而不是 time.Sleep：任务被取消时，退避等待必须立刻结束——
// 否则进程要在一个已经没人要结果的任务上空等几百毫秒（乃至更久）。
func sleepCtx(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
