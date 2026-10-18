package eval

import (
	"context"
	"fmt"
	"time"

	"github.com/mechrevo/safeguard-agent/internal/approval"
	"github.com/mechrevo/safeguard-agent/internal/llm"
	"github.com/mechrevo/safeguard-agent/internal/orchestrator"
	"github.com/mechrevo/safeguard-agent/internal/policy"
	"github.com/mechrevo/safeguard-agent/internal/store"
	"github.com/mechrevo/safeguard-agent/internal/tools"
)

// Options 是评测运行参数。
type Options struct {
	Limits orchestrator.Limits
}

// NewRunner 返回一个 Runner：每道题都装配一套**全新**的内存组件。
//
// 为什么评测不连数据库：评测要可复现。用真库会引入"上一条用例改了配置"这种隐式耦合，
// 指标就不可比了。真实 SQL 路径由集成测试覆盖（D5/D6/D7 各有一个）。
//
// model 传 nil 时使用"理想模型"（按用例期望发工具调用）——那是给 CI 用的：
// 它验证的是**判分器本身**对不对，而不是模型好不好。
func NewRunner(model llm.Client, opts Options) Runner {
	if opts.Limits.MaxRounds <= 0 || opts.Limits.MaxToolCalls <= 0 {
		opts.Limits = orchestrator.DefaultLimits()
	}

	return func(ctx context.Context, c Case) (orchestrator.Result, error) {
		ds := newMemDataSource()
		reg, err := tools.NewFullRegistry(ds, ds)
		if err != nil {
			return orchestrator.Result{}, fmt.Errorf("注册工具失败: %w", err)
		}

		gate := approval.NewGate(newMemIntents(), newMemTickets())
		taskID := "eval_" + c.ID

		// 每个用例换一个新的模型实例：理想模型要靠"第几轮"决定发什么。
		var m llm.Client = model
		if m == nil {
			m = NewOracle(c)
		}

		o := orchestrator.New(m, reg, newMemState(), orchestrator.Options{
			TaskID: taskID,
			Limits: opts.Limits,
			Gate:   gate,
			Policy: policy.New(),
		})
		return o.Run(ctx, c.Query)
	}
}

// ---------------------------------------------------------------------------
// 内存数据源：与 -seed 灌入的演示数据保持一致，保证评测可复现
// ---------------------------------------------------------------------------

type memDataSource struct {
	services []tools.ServiceInfo
	metrics  map[string]map[string]tools.MetricSummary
	configs  map[string][]tools.ConfigItem
	statuses map[string]string
}

func newMemDataSource() *memDataSource {
	return &memDataSource{
		services: []tools.ServiceInfo{
			{Name: "checkout-service", Status: "degraded", Criticality: "核心"},
			{Name: "payment-service", Status: "healthy", Criticality: "核心"},
			{Name: "search-service", Status: "healthy", Criticality: "边缘"},
		},
		metrics: map[string]map[string]tools.MetricSummary{
			"checkout-service": {
				"error_rate":  {Service: "checkout-service", Metric: "error_rate", Unit: "%", SampleCount: 6, Latest: 0.34, Average: 0.30, Max: 0.51},
				"latency_p99": {Service: "checkout-service", Metric: "latency_p99", Unit: "ms", SampleCount: 6, Latest: 820, Average: 760, Max: 900},
			},
			"payment-service": {
				"error_rate": {Service: "payment-service", Metric: "error_rate", Unit: "%", SampleCount: 6, Latest: 0.08, Average: 0.09, Max: 0.12},
			},
		},
		configs: map[string][]tools.ConfigItem{
			"checkout-service": {
				{Key: "timeout_ms", Value: "1500"},
				{Key: "retry", Value: "3"},
			},
			"payment-service": {
				{Key: "timeout_ms", Value: "800"},
			},
		},
		statuses: map[string]string{},
	}
}

func (m *memDataSource) ListServices(_ context.Context) ([]tools.ServiceInfo, error) {
	out := make([]tools.ServiceInfo, len(m.services))
	copy(out, m.services)
	for i := range out {
		if status, ok := m.statuses[out[i].Name]; ok {
			out[i].Status = status
		}
	}
	return out, nil
}

func (m *memDataSource) QueryMetrics(_ context.Context, service, metric string, window time.Duration) (tools.MetricSummary, error) {
	byMetric, ok := m.metrics[service]
	if !ok {
		return tools.MetricSummary{}, &tools.NotFoundError{
			Kind: "service", Name: service, Hint: "可用 list_services 获取全部服务名",
		}
	}
	got, ok := byMetric[metric]
	if !ok {
		return tools.MetricSummary{}, &tools.NotFoundError{Kind: "metric", Name: metric, Hint: "该服务没有这个指标"}
	}
	got.Window = window
	return got, nil
}

func (m *memDataSource) QueryConfig(_ context.Context, service string) ([]tools.ConfigItem, error) {
	items, ok := m.configs[service]
	if !ok {
		return nil, &tools.NotFoundError{Kind: "service", Name: service, Hint: "可用 list_services 获取全部服务名"}
	}
	out := make([]tools.ConfigItem, len(items))
	copy(out, items)
	return out, nil
}

// 写接口（WriteDataSource） ------------------------------------------------

func (m *memDataSource) ServiceExists(_ context.Context, service string) (bool, error) {
	for _, s := range m.services {
		if s.Name == service {
			return true, nil
		}
	}
	return false, nil
}

func (m *memDataSource) ConfigValue(_ context.Context, service, key string) (string, bool, error) {
	for _, item := range m.configs[service] {
		if item.Key == key {
			return item.Value, true, nil
		}
	}
	return "", false, nil
}

func (m *memDataSource) ApplyConfig(_ context.Context, _, service, key, newValue string) (string, bool, error) {
	old := ""
	found := false
	for i := range m.configs[service] {
		if m.configs[service][i].Key == key {
			old = m.configs[service][i].Value
			m.configs[service][i].Value = newValue
			found = true
		}
	}
	if !found {
		m.configs[service] = append(m.configs[service], tools.ConfigItem{Key: key, Value: newValue})
	}
	return old, true, nil
}

func (m *memDataSource) SetServiceStatus(_ context.Context, service, status string) error {
	m.statuses[service] = status
	return nil
}

// ---------------------------------------------------------------------------
// 内存状态机（任务的 Create/UpdateProgress/AppendStep/Get/Steps）
// ---------------------------------------------------------------------------

type memState struct {
	tasks map[string]*store.Task
	steps []store.TaskStep
}

func newMemState() *memState { return &memState{tasks: map[string]*store.Task{}} }

func (s *memState) Create(_ context.Context, task *store.Task) error {
	copied := *task
	s.tasks[task.TaskID] = &copied
	return nil
}

func (s *memState) UpdateProgress(_ context.Context, taskID, status string, currentStep, totalRounds int) error {
	if t, ok := s.tasks[taskID]; ok {
		t.Status = status
		t.CurrentStep = currentStep
		t.TotalRounds = totalRounds
	}
	return nil
}

func (s *memState) AppendStep(_ context.Context, step *store.TaskStep) error {
	s.steps = append(s.steps, *step)
	return nil
}

func (s *memState) Get(_ context.Context, taskID string) (store.Task, error) {
	if t, ok := s.tasks[taskID]; ok {
		return *t, nil
	}
	return store.Task{}, fmt.Errorf("任务不存在: %s", taskID)
}

func (s *memState) Steps(_ context.Context, taskID string) ([]store.TaskStep, error) {
	var out []store.TaskStep
	for _, st := range s.steps {
		if st.TaskID == taskID {
			out = append(out, st)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 内存审批门禁
// ---------------------------------------------------------------------------

type memIntents struct{ rows map[string]*store.ToolCall }

func newMemIntents() *memIntents { return &memIntents{rows: map[string]*store.ToolCall{}} }

func (m *memIntents) BeginIntent(_ context.Context, tc *store.ToolCall) (*store.ToolCall, bool, error) {
	if existing, ok := m.rows[tc.IdemKey]; ok {
		return existing, true, nil
	}
	copied := *tc
	m.rows[tc.IdemKey] = &copied
	return nil, false, nil
}

type memTickets struct {
	rows map[string]*store.Approval
	idem map[string]string
}

func newMemTickets() *memTickets {
	return &memTickets{rows: map[string]*store.Approval{}, idem: map[string]string{}}
}

func (m *memTickets) Create(_ context.Context, t *store.Approval) (*store.Approval, bool, error) {
	if id, ok := m.idem[t.IdemKey]; ok {
		return m.rows[id], false, nil
	}
	copied := *t
	m.rows[t.TicketID] = &copied
	m.idem[t.IdemKey] = t.TicketID
	return &copied, true, nil
}

func (m *memTickets) Get(_ context.Context, ticketID string) (store.Approval, error) {
	if t, ok := m.rows[ticketID]; ok {
		return *t, nil
	}
	return store.Approval{}, store.ErrTicketNotFound
}

func (m *memTickets) Decide(_ context.Context, ticketID, status, approver, reason string) error {
	t, ok := m.rows[ticketID]
	if !ok || t.Status != store.ApprovalStatusPending {
		return store.ErrTicketNotFound
	}
	t.Status = status
	t.Approver = &approver
	t.Reason = &reason
	now := time.Now().UTC()
	t.DecidedAt = &now
	return nil
}

func (m *memTickets) ListPending(context.Context) ([]store.Approval, error) { return nil, nil }

func (m *memTickets) ExpireOverdue(context.Context, time.Time) (int64, error) { return 0, nil }
