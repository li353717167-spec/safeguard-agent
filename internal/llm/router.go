package llm

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Router 把多个模型端点包成一个 Client：监控稳定性，并在失败时**自动切换**。
//
// 它完全实现 Client 接口，所以编排层、评测、CLI 一行都不用改——
// 这是 D1 "业务只依赖接口" 那条决定真正兑现的地方。
//
// 三个设计点（面试会被追问）：
//
//  1. **切换发生在调用内部**：某个端点失败时，同一次 Chat 立刻用下一个端点重发。
//     对上层来说这就是一次成功调用，用户感知不到"换了个模型"。
//  2. **粘性优先于负载均衡**：一旦切到某个端点，后续请求继续用它，直到它不健康。
//     不轮询的原因是模型行为与 prompt 缓存都跟端点绑定，同一任务内来回换会让行为跳变。
//  3. **全挂时 fail-open**：所有端点都在冷却时，仍然选"最早恢复"的那个去试。
//     宁可多试一次，也不要让任务因为"监控判定"直接失败。
type Router struct {
	mu    sync.Mutex
	cfg   RouterConfig
	eps   []endpoint
	state map[string]*endpointState
	// active 是当前粘住的端点名；空表示还没定（下次按策略选）。
	active string
}

type endpoint struct {
	name   string
	client Client
}

// RouterConfig 是路由与健康判定参数。
type RouterConfig struct {
	// Endpoints 至少一个；第一个是**首选端点**（primary）。
	Endpoints []Endpoint
	// FailThreshold 是"连续失败多少次判定为不健康"。
	FailThreshold int
	// Cooldown 是判定不健康后的冷却时间；冷却期内不再选它（除非全都冷却）。
	Cooldown time.Duration
	// Window 是统计成功率的滑动窗口（只影响展示，不影响判定）。
	Window time.Duration
	// OnFailover 在切换时回调（日志/审计用），可为空。
	OnFailover func(from, to, reason string)
	// Now 注入时钟（测试用），为空则用 time.Now。
	Now func() time.Time
}

// Endpoint 是一个模型端点。
type Endpoint struct {
	// Name 是展示名（默认 host/model），用于日志与 `-models`。
	Name   string
	Client Client
}

const (
	defaultFailThreshold = 3
	defaultCooldown      = 30 * time.Second
	defaultWindow        = 2 * time.Minute
)

// NewRouter 构造路由器。
func NewRouter(cfg RouterConfig) (*Router, error) {
	if len(cfg.Endpoints) == 0 {
		return nil, errors.New("llm: 路由器至少需要一个端点")
	}
	if cfg.FailThreshold <= 0 {
		cfg.FailThreshold = defaultFailThreshold
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = defaultCooldown
	}
	if cfg.Window <= 0 {
		cfg.Window = defaultWindow
	}
	if cfg.Now == nil {
		now := time.Now
		cfg.Now = func() time.Time { return now().UTC() }
	}

	r := &Router{cfg: cfg, state: make(map[string]*endpointState, len(cfg.Endpoints))}
	for _, ep := range cfg.Endpoints {
		if ep.Client == nil {
			return nil, fmt.Errorf("llm: 端点 %q 的 client 为空", ep.Name)
		}
		name := ep.Name
		if name == "" {
			name = ep.Client.Model()
		}
		r.eps = append(r.eps, endpoint{name: name, client: ep.Client})
		r.state[name] = &endpointState{}
	}
	return r, nil
}

// Chat 实现 Client：选端点 → 调 → 失败就换下一个（同一次请求内）。
func (r *Router) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	order := r.candidates()

	var lastErr error
	for i, ep := range order {
		// 父上下文已经结束（用户取消/整体超时）：不要再换端点重试。
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return ChatResponse{}, lastErr
			}
			return ChatResponse{}, err
		}

		started := time.Now()
		resp, err := ep.client.Chat(ctx, req)
		if err == nil {
			r.onSuccess(ep.name, time.Since(started))
			if i > 0 {
				r.notifyFailover(order[0].name, ep.name, fmt.Sprintf("%v", lastErr))
			}
			return resp, nil
		}

		// ctx 主动取消不算端点的问题：不计健康度、不切换。
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return ChatResponse{}, err
		}

		r.onFailure(ep.name, err)
		lastErr = err
	}

	if lastErr == nil {
		lastErr = errors.New("llm: 没有可用的模型端点")
	}
	return ChatResponse{}, lastErr
}

// Provider / Model 返回**当前粘住**端点（没有则首选端点）的标识。
// 单次响应的真实来源看 ChatResponse.Provider —— 切换时两者可能不同。
func (r *Router) Provider() string {
	ep := r.current()
	return ep.client.Provider()
}

func (r *Router) Model() string {
	ep := r.current()
	return ep.client.Model()
}

// Statuses 返回各端点的健康快照（`-models` 与测试用）。
func (r *Router) Statuses() []EndpointStatus {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.cfg.Now()
	out := make([]EndpointStatus, 0, len(r.eps))
	for _, ep := range r.eps {
		st := r.state[ep.name]
		st.prune(now, r.cfg.Window)

		status := EndpointStatus{
			Name:                ep.name,
			Healthy:             st.healthy(now),
			ConsecutiveFailures: st.consecutive,
			Successes:           st.successes,
			Failures:            st.failures,
			P95:                 st.p95(),
			Active:              r.active == ep.name,
			LastError:           st.lastError,
		}
		if status.Successes+status.Failures > 0 {
			status.SuccessRate = float64(status.Successes) / float64(status.Successes+status.Failures)
		}
		if now.Before(st.coolUntil) {
			status.CooldownRemaining = st.coolUntil.Sub(now).Round(time.Second)
		}
		out = append(out, status)
	}
	return out
}

// EndpointStatus 是一个端点的健康快照。
type EndpointStatus struct {
	Name                string
	Healthy             bool
	Active              bool
	ConsecutiveFailures int
	Successes           int
	Failures            int
	SuccessRate         float64
	P95                 time.Duration
	CooldownRemaining   time.Duration
	LastError           string
}

// ProbeAll 主动探测所有端点：各发一次最小请求，并记录健康度。
//
// "动态监控"不只是被动记账：空闲时也可以主动探一次，
// 免得"上次失败已经是 10 分钟前"这种陈旧判断迟迟不恢复。
func (r *Router) ProbeAll(ctx context.Context) []EndpointStatus {
	for _, ep := range r.eps {
		started := time.Now()
		_, err := ep.client.Chat(ctx, ChatRequest{
			Messages: []Message{{Role: RoleUser, Content: "ping"}},
		})
		if err != nil {
			r.onFailure(ep.name, err)
			continue
		}
		r.onSuccess(ep.name, time.Since(started))
	}
	return r.Statuses()
}

// candidates 按策略给出尝试顺序。
//
// 顺序：粘住的端点 → 首选端点 → 其他健康端点（按成功率/延迟打分）→ 全挂时按"最早恢复"。
func (r *Router) candidates() []endpoint {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.cfg.Now()
	var healthy []endpoint
	all := make([]endpoint, 0, len(r.eps))
	all = append(all, r.eps...)

	for _, ep := range r.eps {
		if r.state[ep.name].healthy(now) {
			healthy = append(healthy, ep)
		}
	}

	if len(healthy) == 0 {
		// 全挂：选最早恢复的那个试一次（fail-open）。
		sort.SliceStable(all, func(i, j int) bool {
			return r.state[all[i].name].coolUntil.Before(r.state[all[j].name].coolUntil)
		})
		return all
	}

	out := make([]endpoint, 0, len(all))
	used := make(map[string]bool, len(all))

	add := func(ep endpoint) {
		if used[ep.name] {
			return
		}
		used[ep.name] = true
		out = append(out, ep)
	}

	// ① 粘住的端点（如果它健康）
	for _, ep := range healthy {
		if ep.name == r.active {
			add(ep)
		}
	}
	// ② 首选端点（如果健康）
	for _, ep := range healthy {
		if ep.name == r.eps[0].name {
			add(ep)
		}
	}
	// ③ 其他健康端点：成功率高的优先，其次延迟低的
	rest := make([]endpoint, 0, len(healthy))
	for _, ep := range healthy {
		if !used[ep.name] {
			rest = append(rest, ep)
		}
	}
	sort.SliceStable(rest, func(i, j int) bool {
		return r.state[rest[i].name].score() > r.state[rest[j].name].score()
	})
	for _, ep := range rest {
		add(ep)
	}
	// ④ 不健康的排最后：真要全试一遍时也别漏掉它们
	for _, ep := range all {
		add(ep)
	}
	return out
}

func (r *Router) current() endpoint {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ep := range r.eps {
		if ep.name == r.active {
			return ep
		}
	}
	return r.eps[0]
}

func (r *Router) onSuccess(name string, dur time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.cfg.Now()
	st := r.state[name]
	st.record(now, true, dur, "")
	st.consecutive = 0
	st.coolUntil = time.Time{}
	// 成功即"粘住"：后续请求继续用它，避免在健康端点之间来回跳。
	r.active = name
}

func (r *Router) onFailure(name string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.cfg.Now()
	st := r.state[name]
	st.record(now, false, 0, err.Error())
	st.consecutive++
	if st.consecutive >= r.cfg.FailThreshold {
		st.coolUntil = now.Add(r.cfg.Cooldown)
		// 粘住的端点坏了就松开，让下一次重新选。
		if r.active == name {
			r.active = ""
		}
	}
}

func (r *Router) notifyFailover(from, to, reason string) {
	if r.cfg.OnFailover != nil {
		r.cfg.OnFailover(from, to, reason)
	}
}

// endpointState 是单个端点的健康状态。
type endpointState struct {
	consecutive int
	coolUntil   time.Time
	lastError   string

	successes int
	failures  int
	samples   []sample // 窗口内的调用样本（成功率 / P95）
}

type sample struct {
	at  time.Time
	ok  bool
	dur time.Duration
}

func (s *endpointState) record(now time.Time, ok bool, dur time.Duration, errMsg string) {
	if ok {
		s.successes++
	} else {
		s.failures++
		s.lastError = errMsg
	}
	s.samples = append(s.samples, sample{at: now, ok: ok, dur: dur})
}

// healthy 判断端点是否可用。
//
// 语义是"冷却到期即可再试"，而不是"连续失败必须清零"：
// 如果要求 consecutive 先归零，那么端点一旦被判不健康就**永远无法恢复**
// （没人会再调它，自然也不会有成功来清零）——这是熔断器最常见的死锁写法。
// 冷却到期后允许一次试探：成功则复位，失败则立刻重新冷却（半开）。
func (s *endpointState) healthy(now time.Time) bool {
	return !now.Before(s.coolUntil)
}

func (s *endpointState) prune(now time.Time, window time.Duration) {
	cutoff := now.Add(-window)
	kept := s.samples[:0]
	for _, smp := range s.samples {
		if smp.at.After(cutoff) {
			kept = append(kept, smp)
		}
	}
	s.samples = kept

	// successes/failures 是"窗口内"的计数：同步重算，避免只增不减。
	s.successes, s.failures = 0, 0
	for _, smp := range kept {
		if smp.ok {
			s.successes++
		} else {
			s.failures++
		}
	}
}

// p95 是窗口内的 P95 延迟（样本量小时取最大值，不假装精确）。
func (s *endpointState) p95() time.Duration {
	if len(s.samples) == 0 {
		return 0
	}
	durs := make([]time.Duration, 0, len(s.samples))
	for _, smp := range s.samples {
		if smp.ok {
			durs = append(durs, smp.dur)
		}
	}
	if len(durs) == 0 {
		return 0
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	idx := int(float64(len(durs)-1) * 0.95)
	return durs[idx]
}

// score 用于在多个健康端点里挑"更好的那个"：成功率 7 成 + 延迟 3 成。
func (s *endpointState) score() float64 {
	total := s.successes + s.failures
	if total == 0 {
		return 0.5 // 还没样本：给中性分，让首选端点之外的端点也可能被优先
	}
	rate := float64(s.successes) / float64(total)
	latency := float64(s.p95().Milliseconds())
	latencyScore := 1.0
	if latency > 0 {
		latencyScore = 1.0 / (1.0 + latency/1000.0) // 1s 时约 0.5
	}
	return rate*0.7 + latencyScore*0.3
}

// DescribeStatuses 把状态渲染成多行文本（CLI 用）。
func DescribeStatuses(statuses []EndpointStatus) string {
	var b strings.Builder
	for _, s := range statuses {
		state := "healthy"
		switch {
		case !s.Healthy && s.CooldownRemaining > 0:
			state = fmt.Sprintf("cooldown(%s)", s.CooldownRemaining)
		case !s.Healthy:
			state = "unhealthy"
		}
		mark := " "
		if s.Active {
			mark = "*"
		}
		fmt.Fprintf(&b, "%s %-32s %-14s 连续失败=%d 窗口成功=%d/失败=%d 成功率=%.0f%% p95=%s",
			mark, s.Name, state, s.ConsecutiveFailures, s.Successes, s.Failures, s.SuccessRate*100, s.P95.Round(time.Millisecond))
		if s.LastError != "" {
			fmt.Fprintf(&b, "\n    最后错误: %s", truncate(s.LastError, 120))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
