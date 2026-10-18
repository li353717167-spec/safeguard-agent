package executor

import (
	"sync"
	"time"
)

// BreakerConfig 是熔断的判定参数。
//
// 默认值对应的是"下游整体不可用"这个判断：
// 60 秒窗口内累计 5 次**可重试失败** → 打开；打开 30 秒后进入半开，只放 1 个试探请求。
type BreakerConfig struct {
	Window    time.Duration
	Threshold int
	OpenFor   time.Duration
}

// DefaultBreakerConfig 返回默认熔断参数。
func DefaultBreakerConfig() BreakerConfig {
	return BreakerConfig{Window: 60 * time.Second, Threshold: 5, OpenFor: 30 * time.Second}
}

func (c BreakerConfig) normalized() BreakerConfig {
	if c.Window <= 0 {
		c.Window = 60 * time.Second
	}
	if c.Threshold <= 0 {
		c.Threshold = 5
	}
	if c.OpenFor <= 0 {
		c.OpenFor = 30 * time.Second
	}
	return c
}

type breakerState int

const (
	stateClosed breakerState = iota
	stateOpen
	stateHalfOpen
)

func (s breakerState) String() string {
	switch s {
	case stateClosed:
		return "closed"
	case stateOpen:
		return "open"
	case stateHalfOpen:
		return "half_open"
	default:
		return "unknown"
	}
}

// Breaker 是单个工具的熔断器（内存状态）。
//
// 为什么状态放内存，而任务状态（D1）放 MySQL：
//   - 熔断描述的是"**这个进程当前看到的**对端健康度"，是运行期信号，不是需要持久化的业务状态；
//   - 重启后"忘了下游刚挂过"，代价只是多试几次；而每次调用都读写一次熔断状态，
//     等于在热路径上给每个请求加一次 IO。
//
// 代价也要说清楚：**多实例之间不共享熔断状态**，每个实例各自判断、各自打开。
// 如果以后需要全局熔断，把 BreakerSet 换成一个共享存储即可，调用方不用改。
type Breaker struct {
	tool string
	cfg  BreakerConfig
	now  func() time.Time

	mu            sync.Mutex
	state         breakerState
	failures      []time.Time // 窗口内的"可重试失败"时间戳
	openedAt      time.Time
	probeInFlight bool
}

func newBreaker(tool string, cfg BreakerConfig, now func() time.Time) *Breaker {
	if now == nil {
		now = time.Now
	}
	return &Breaker{tool: tool, cfg: cfg.normalized(), now: now}
}

// State 返回当前状态（供日志与测试观察）。
func (b *Breaker) State() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneLocked(b.now())
	return b.state.String()
}

// Allow 判断本次调用是否放行。
//
// 三种情况：
//   - 关闭：放行；
//   - 打开且未到时间：拒绝（快速失败，不真的打下游）；
//   - 打开且已到时间 → 转半开，只放**一个**试探请求，其余继续拒绝。
func (b *Breaker) Allow() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	b.pruneLocked(now)

	switch b.state {
	case stateClosed:
		return nil

	case stateOpen:
		if now.Sub(b.openedAt) < b.cfg.OpenFor {
			return b.openErrorLocked(now)
		}
		// 打开时长已到：半开，放一个试探。
		b.state = stateHalfOpen
		b.probeInFlight = true
		return nil

	case stateHalfOpen:
		if b.probeInFlight {
			// 已经有一个试探在飞，其余请求继续快速失败。
			return b.openErrorLocked(now)
		}
		b.probeInFlight = true
		return nil
	}
	return nil
}

// OnSuccess 记录一次成功。半开状态下成功即关闭熔断。
func (b *Breaker) OnSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case stateHalfOpen:
		b.state = stateClosed
		b.failures = nil
		b.probeInFlight = false
	case stateClosed:
		// 关闭状态下成功不必记账：窗口内的旧失败会随时间自然过期。
	}
}

// OnRetryableFailure 记录一次**可重试**的失败。
//
// 只有这类失败计入熔断：它意味着"下游整体可能不可用"。
// 参数错误、认证失败不算——那是调用方的问题，不代表对端健康度。
func (b *Breaker) OnRetryableFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	b.pruneLocked(now)

	if b.state == stateHalfOpen {
		// 试探失败：继续打开，重新计时。
		b.state = stateOpen
		b.openedAt = now
		b.probeInFlight = false
		return
	}

	b.failures = append(b.failures, now)
	if len(b.failures) >= b.cfg.Threshold {
		b.state = stateOpen
		b.openedAt = now
	}
}

// OnNeutral 记录一次"与下游健康度无关"的结果（参数错误、认证失败、成功但业务拒绝……）。
//
// 它只做一件事：把半开状态占用的试探名额还回去。
// 少了这一步，一次参数错误就能让半开永久卡在"试探在飞"，等于把工具彻底封死。
func (b *Breaker) OnNeutral() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state == stateHalfOpen {
		b.probeInFlight = false
	}
}

func (b *Breaker) openErrorLocked(now time.Time) error {
	remaining := b.cfg.OpenFor - now.Sub(b.openedAt)
	if remaining < 0 {
		remaining = 0
	}
	return &CircuitOpenError{
		Tool:       b.tool,
		Failures:   len(b.failures),
		Window:     b.cfg.Window,
		RetryAfter: remaining.Round(time.Second),
	}
}

// pruneLocked 丢掉窗口之外的失败记录。
func (b *Breaker) pruneLocked(now time.Time) {
	if len(b.failures) == 0 {
		return
	}
	cutoff := now.Add(-b.cfg.Window)
	kept := b.failures[:0]
	for _, ts := range b.failures {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	b.failures = kept
}

// BreakerSet 按工具名维护独立的熔断器。
//
// 为什么按工具隔离：不同工具背后是**不同的下游**。数据库查询挂了，不该把
// 一个读配置的本地工具也一起熔断掉。
type BreakerSet struct {
	cfg BreakerConfig
	now func() time.Time

	mu sync.Mutex
	m  map[string]*Breaker
}

// NewBreakerSet 构造熔断器集合。
func NewBreakerSet(cfg BreakerConfig) *BreakerSet { return newBreakerSet(cfg, nil) }

func newBreakerSet(cfg BreakerConfig, now func() time.Time) *BreakerSet {
	if now == nil {
		now = time.Now
	}
	return &BreakerSet{cfg: cfg.normalized(), now: now, m: map[string]*Breaker{}}
}

// For 取某个工具的熔断器（不存在则创建）。
func (s *BreakerSet) For(tool string) *Breaker {
	s.mu.Lock()
	defer s.mu.Unlock()

	breaker, ok := s.m[tool]
	if !ok {
		breaker = newBreaker(tool, s.cfg, s.now)
		s.m[tool] = breaker
	}
	return breaker
}

// States 返回各工具的熔断状态（日志/测试用）。
func (s *BreakerSet) States() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make(map[string]string, len(s.m))
	for name, breaker := range s.m {
		out[name] = breaker.State()
	}
	return out
}
