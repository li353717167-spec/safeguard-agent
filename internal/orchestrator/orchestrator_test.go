package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/mechrevo/safeguard-agent/internal/approval"
	"github.com/mechrevo/safeguard-agent/internal/executor"
	"github.com/mechrevo/safeguard-agent/internal/llm"
	"github.com/mechrevo/safeguard-agent/internal/store"
	"github.com/mechrevo/safeguard-agent/internal/tools"
)

// ---------------------------------------------------------------------------
// 测试替身：假模型、假状态存储、假数据源
// ---------------------------------------------------------------------------

// fakeLLM 按脚本依次返回响应（或错误），并记录每次请求。
// 脚本用尽后返回错误，让测试"响亮地失败"而不是悄悄跑飞。
type fakeLLM struct {
	responses []llm.ChatResponse
	errs      []error
	requests  []llm.ChatRequest
}

func (f *fakeLLM) Chat(_ context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	// 必须深拷贝消息切片：调用方会在请求之后继续 append，
	// 直接存切片头会让断言看到"未来"的内容。
	msgs := make([]llm.Message, len(req.Messages))
	copy(msgs, req.Messages)
	toolDefs := make([]llm.ToolDef, len(req.Tools))
	copy(toolDefs, req.Tools)
	f.requests = append(f.requests, llm.ChatRequest{Messages: msgs, Tools: toolDefs})

	i := len(f.requests) - 1
	if i < len(f.errs) && f.errs[i] != nil {
		return llm.ChatResponse{}, f.errs[i]
	}
	if i >= len(f.responses) {
		return llm.ChatResponse{}, fmt.Errorf("fakeLLM: 脚本里没有第 %d 个响应", i+1)
	}
	return f.responses[i], nil
}

func (f *fakeLLM) Provider() string { return "fake" }
func (f *fakeLLM) Model() string    { return "fake-model" }

type progressUpdate struct {
	Status      string
	CurrentStep int
	TotalRounds int
}

type fakeState struct {
	tasks        []store.Task
	progress     []progressUpdate
	steps        []store.TaskStep
	failOnAppend bool
}

func (s *fakeState) Create(_ context.Context, task *store.Task) error {
	s.tasks = append(s.tasks, *task)
	return nil
}

func (s *fakeState) UpdateProgress(_ context.Context, _ string, status string, currentStep, totalRounds int) error {
	s.progress = append(s.progress, progressUpdate{Status: status, CurrentStep: currentStep, TotalRounds: totalRounds})
	return nil
}

func (s *fakeState) AppendStep(_ context.Context, step *store.TaskStep) error {
	if s.failOnAppend {
		return errors.New("fakeState: 落库失败")
	}
	s.steps = append(s.steps, *step)
	return nil
}

// Get / Steps 供断点恢复读取（D7）。
func (s *fakeState) Get(_ context.Context, taskID string) (store.Task, error) {
	for _, t := range s.tasks {
		if t.TaskID == taskID {
			return t, nil
		}
	}
	return store.Task{}, errors.New("fakeState: 任务不存在")
}

func (s *fakeState) Steps(_ context.Context, taskID string) ([]store.TaskStep, error) {
	var out []store.TaskStep
	for _, st := range s.steps {
		if st.TaskID == taskID {
			out = append(out, st)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("fakeState: 没有步骤")
	}
	return out, nil
}

func (s *fakeState) lastStatus() string {
	if len(s.progress) == 0 {
		return ""
	}
	return s.progress[len(s.progress)-1].Status
}

// stubSource 是内存数据源。
type stubSource struct {
	metric    tools.MetricSummary
	metricErr error
	// configs 非空时用它：用来构造"被污染的数据源"（间接提示注入）。
	configs []tools.ConfigItem
}

func (s stubSource) ListServices(context.Context) ([]tools.ServiceInfo, error) {
	return []tools.ServiceInfo{{Name: "payment-service", Status: "healthy"}}, nil
}

func (s stubSource) QueryMetrics(context.Context, string, string, time.Duration) (tools.MetricSummary, error) {
	if s.metricErr != nil {
		return tools.MetricSummary{}, s.metricErr
	}
	return s.metric, nil
}

func (s stubSource) QueryConfig(context.Context, string) ([]tools.ConfigItem, error) {
	if len(s.configs) > 0 {
		return s.configs, nil
	}
	return []tools.ConfigItem{{Key: "timeout_ms", Value: "800"}}, nil
}

// stubTool 用于构造"非无害工具"，验证越权拦截。
type stubTool struct {
	spec   tools.Spec
	called bool
}

func (s *stubTool) Spec() tools.Spec { return s.spec }

func (s *stubTool) Invoke(context.Context, json.RawMessage) (tools.Result, error) {
	s.called = true
	return tools.Result{Summary: "不该被执行"}, nil
}

func (s *stubTool) Validate(json.RawMessage) error { return nil }

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

func okMetric() tools.MetricSummary {
	return tools.MetricSummary{
		Service: "payment-service", Metric: "error_rate", Unit: "%",
		SampleCount: 5, Latest: 0.1, Average: 0.12, Max: 0.15,
		LatestAt: time.Date(2026, 10, 15, 15, 21, 0, 0, time.UTC),
		Window:   5 * time.Minute,
	}
}

func defaultRegistry(t *testing.T, src tools.DataSource) *tools.Registry {
	t.Helper()
	reg, err := tools.NewDefaultRegistry(src)
	if err != nil {
		t.Fatalf("构造注册中心失败: %v", err)
	}
	return reg
}

func toolCallResponse(calls ...llm.ToolCall) llm.ChatResponse {
	return llm.ChatResponse{
		Message:      llm.Message{Role: llm.RoleAssistant, ToolCalls: calls},
		FinishReason: llm.FinishToolCalls,
		Usage:        llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}
}

func answerResponse(text string) llm.ChatResponse {
	return llm.ChatResponse{
		Message:      llm.Message{Role: llm.RoleAssistant, Content: text},
		FinishReason: llm.FinishStop,
		Usage:        llm.Usage{PromptTokens: 20, CompletionTokens: 8, TotalTokens: 28},
	}
}

func call(id, name, args string) llm.ToolCall {
	return llm.ToolCall{ID: id, Type: "function", Function: llm.FunctionCall{Name: name, Arguments: args}}
}

// toolMessagesOf 找出请求里所有 role=tool 的消息。
func toolMessagesOf(req llm.ChatRequest) []llm.Message {
	var out []llm.Message
	for _, m := range req.Messages {
		if m.Role == llm.RoleTool {
			out = append(out, m)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 用例
// ---------------------------------------------------------------------------

func TestRunAnswersWithoutTools(t *testing.T) {
	client := &fakeLLM{responses: []llm.ChatResponse{answerResponse("payment-service 一切正常")}}
	state := &fakeState{}
	o := New(client, defaultRegistry(t, stubSource{metric: okMetric()}), state, Options{})

	res, err := o.Run(context.Background(), "payment-service 有问题吗？")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if res.Stop != StopAnswered {
		t.Errorf("Stop = %s, 期望 %s", res.Stop, StopAnswered)
	}
	if res.Answer != "payment-service 一切正常" {
		t.Errorf("Answer = %q", res.Answer)
	}
	if res.Rounds != 1 || res.ToolCalls != 0 {
		t.Errorf("Rounds=%d ToolCalls=%d, 期望 1/0", res.Rounds, res.ToolCalls)
	}
	if res.LimitTriggered() {
		t.Error("正常回答不应被标记为撞上限")
	}
	if len(state.steps) != 0 {
		t.Errorf("没有工具调用就不该有步骤，实际 %d 条", len(state.steps))
	}
	if len(state.tasks) != 1 || state.tasks[0].Status != store.TaskStatusRunning {
		t.Errorf("任务创建不完整: %+v", state.tasks)
	}
	if got := state.lastStatus(); got != store.TaskStatusSuccess {
		t.Errorf("最终状态 = %q, 期望 success", got)
	}
	// 第一轮就该带上工具 schema（ReAct 的"手"）。
	if len(client.requests[0].Tools) != 3 {
		t.Errorf("首轮工具数 = %d, 期望 3", len(client.requests[0].Tools))
	}
}

func TestRunExecutesToolAndFeedsResultBack(t *testing.T) {
	client := &fakeLLM{responses: []llm.ChatResponse{
		toolCallResponse(call("call_1", "query_metrics", `{"service":"payment-service","metric":"error_rate","window":"5m"}`)),
		answerResponse("payment-service 最近 5 分钟错误率 0.1%，正常。"),
	}}
	state := &fakeState{}
	o := New(client, defaultRegistry(t, stubSource{metric: okMetric()}), state, Options{})

	res, err := o.Run(context.Background(), "payment-service 最近 5 分钟的错误率？")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if res.Stop != StopAnswered || res.Rounds != 2 || res.ToolCalls != 1 {
		t.Errorf("Stop=%s Rounds=%d ToolCalls=%d, 期望 answered/2/1", res.Stop, res.Rounds, res.ToolCalls)
	}
	if len(res.Traces) != 1 || res.Traces[0].ToolName != "query_metrics" {
		t.Fatalf("轨迹异常: %+v", res.Traces)
	}
	if res.Traces[0].Error != "" || res.Traces[0].Rejected {
		t.Errorf("这次调用本应成功: %+v", res.Traces[0])
	}
	if !strings.Contains(res.Traces[0].Summary, "payment-service") {
		t.Errorf("轨迹摘要应含服务名: %q", res.Traces[0].Summary)
	}

	// 第二轮请求里必须带上 role=tool 的结果，且 call_id 与请求对得上。
	second := client.requests[1]
	toolMsgs := toolMessagesOf(second)
	if len(toolMsgs) != 1 {
		t.Fatalf("第二轮应有 1 条工具结果，实际 %d 条", len(toolMsgs))
	}
	if toolMsgs[0].ToolCallID != "call_1" {
		t.Errorf("tool_call_id = %q, 期望 call_1（配对不上模型会报错）", toolMsgs[0].ToolCallID)
	}
	if !strings.Contains(toolMsgs[0].Content, `"found":true`) {
		t.Errorf("工具结果应含真实数据: %s", toolMsgs[0].Content)
	}

	// 步骤落库：step_no 与工具调用序号一一对应。
	if len(state.steps) != 1 {
		t.Fatalf("应落 1 条步骤，实际 %d 条", len(state.steps))
	}
	step := state.steps[0]
	if step.StepNo != 1 || step.RoundNo != 1 {
		t.Errorf("步骤号/轮次 = %d/%d, 期望 1/1", step.StepNo, step.RoundNo)
	}
	if step.Status != store.StepStatusDone {
		t.Errorf("步骤状态 = %q, 期望 done", step.Status)
	}
	if step.Plan == nil || !strings.Contains(*step.Plan, "query_metrics") {
		t.Errorf("步骤 plan 应记录工具与参数: %v", step.Plan)
	}
}

func TestArgErrorIsFedBackForSelfCorrection(t *testing.T) {
	client := &fakeLLM{responses: []llm.ChatResponse{
		// 指标名不在枚举内 → 参数校验失败
		toolCallResponse(call("call_1", "query_metrics", `{"service":"payment-service","metric":"cpu_usage"}`)),
		answerResponse("抱歉，我查错了指标。"),
	}}
	state := &fakeState{}
	o := New(client, defaultRegistry(t, stubSource{metric: okMetric()}), state, Options{})

	res, err := o.Run(context.Background(), "看看 CPU")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if res.Stop != StopAnswered {
		t.Errorf("参数错误不应终止任务，Stop = %s", res.Stop)
	}
	toolMsgs := toolMessagesOf(client.requests[1])
	if len(toolMsgs) != 1 || !strings.Contains(toolMsgs[0].Content, "bad_args") {
		t.Fatalf("参数错误必须以 bad_args 回喂: %+v", toolMsgs)
	}
	if !strings.Contains(toolMsgs[0].Content, "metric") {
		t.Errorf("回喂内容要说清哪个参数错了: %s", toolMsgs[0].Content)
	}
	if len(state.steps) != 1 || state.steps[0].Status != store.StepStatusFailed {
		t.Errorf("参数错误的步骤应记为 failed: %+v", state.steps)
	}
}

func TestSystemErrorIsFedBackAsSystemErrorNotBadArgs(t *testing.T) {
	client := &fakeLLM{responses: []llm.ChatResponse{
		toolCallResponse(call("call_1", "query_metrics", `{"service":"payment-service","metric":"error_rate"}`)),
		answerResponse("查询暂时不可用。"),
	}}
	o := New(client, defaultRegistry(t, stubSource{metricErr: errors.New("dial tcp 127.0.0.1:3306: connection refused")}), &fakeState{}, Options{})

	if _, err := o.Run(context.Background(), "错误率？"); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	toolMsgs := toolMessagesOf(client.requests[1])
	if len(toolMsgs) != 1 {
		t.Fatalf("应有 1 条工具错误结果: %+v", toolMsgs)
	}
	content := toolMsgs[0].Content
	if !strings.Contains(content, "system_error") {
		t.Errorf("系统错误必须以 system_error 回喂: %s", content)
	}
	if !strings.Contains(content, "不是你参数的问题") {
		t.Errorf("必须明确告诉模型这不是参数问题，否则它会去改参数: %s", content)
	}
}

func TestMaxRoundsTriggersSummaryWithoutTools(t *testing.T) {
	client := &fakeLLM{responses: []llm.ChatResponse{
		toolCallResponse(call("call_1", "query_metrics", `{"service":"payment-service","metric":"error_rate"}`)),
		toolCallResponse(call("call_2", "query_config", `{"service":"payment-service"}`)),
		answerResponse("已知：错误率正常；未确认：配置是否被改过。"),
	}}
	state := &fakeState{}
	o := New(client, defaultRegistry(t, stubSource{metric: okMetric()}), state,
		Options{Limits: Limits{MaxRounds: 2, MaxToolCalls: 10}})

	res, err := o.Run(context.Background(), "payment-service 怎么了？")
	if err != nil {
		t.Fatalf("撞上限不该返回错误: %v", err)
	}
	if res.Stop != StopMaxRounds {
		t.Errorf("Stop = %s, 期望 max_rounds", res.Stop)
	}
	if !res.LimitTriggered() {
		t.Error("LimitTriggered 应为 true（上限触发率靠它统计）")
	}
	if !strings.Contains(res.Answer, "未确认") {
		t.Errorf("应返回总结而不是报错，Answer = %q", res.Answer)
	}
	if len(client.requests) != 3 {
		t.Fatalf("应有 3 次模型调用（2 轮 + 1 次总结），实际 %d", len(client.requests))
	}
	// 关键：总结阶段不能带工具，否则模型会继续发起调用，上限形同虚设。
	if n := len(client.requests[2].Tools); n != 0 {
		t.Errorf("总结调用不应带工具 schema，实际 %d 个", n)
	}
	if got := state.lastStatus(); got != store.TaskStatusSuccess {
		t.Errorf("撞上限并给出总结后状态应为 success，实际 %q", got)
	}
}

// TestMaxToolCallsNeverLeavesDanglingToolCall 钉住一个很容易漏的协议细节：
// 一旦因为预算耗尽而少执行了某个调用，**仍然必须为它补一条 tool 结果**，
// 否则消息序列里出现"有 tool_calls 却没有对应结果"的残缺，服务端会直接拒绝整个请求。
func TestMaxToolCallsNeverLeavesDanglingToolCall(t *testing.T) {
	client := &fakeLLM{responses: []llm.ChatResponse{
		toolCallResponse(
			call("call_1", "query_metrics", `{"service":"payment-service","metric":"error_rate"}`),
			call("call_2", "query_metrics", `{"service":"payment-service","metric":"qps"}`),
		),
		answerResponse("只查到错误率，QPS 因预算耗尽未查。"),
	}}
	state := &fakeState{}
	o := New(client, defaultRegistry(t, stubSource{metric: okMetric()}), state,
		Options{Limits: Limits{MaxRounds: 5, MaxToolCalls: 1}})

	res, err := o.Run(context.Background(), "把 payment-service 的指标都查一遍")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if res.Stop != StopMaxToolCalls {
		t.Errorf("Stop = %s, 期望 max_tool_calls", res.Stop)
	}
	if res.ToolCalls != 1 {
		t.Errorf("ToolCalls = %d, 期望 1（预算只允许一次）", res.ToolCalls)
	}
	if len(res.Traces) != 2 {
		t.Fatalf("两次调用都应留痕，实际 %d 条", len(res.Traces))
	}
	if !res.Traces[1].Rejected {
		t.Error("第二次调用应被标记为未执行")
	}

	// 总结请求里，两个 call_id 都必须在 tool 消息里出现。
	summaryReq := client.requests[len(client.requests)-1]
	got := map[string]bool{}
	for _, m := range toolMessagesOf(summaryReq) {
		got[m.ToolCallID] = true
	}
	for _, want := range []string{"call_1", "call_2"} {
		if !got[want] {
			t.Errorf("call_id %s 没有对应的工具结果，消息序列是残缺的", want)
		}
	}
	if len(state.steps) != 1 {
		t.Errorf("未执行的调用不应落步骤，实际 %d 条", len(state.steps))
	}
}

func TestSummaryFailureFallsBackToLocalSummary(t *testing.T) {
	client := &fakeLLM{
		responses: []llm.ChatResponse{
			toolCallResponse(call("call_1", "query_metrics", `{"service":"payment-service","metric":"error_rate"}`)),
		},
		errs: []error{nil, errors.New("model unreachable")}, // 第 2 次（总结）失败
	}
	state := &fakeState{}
	o := New(client, defaultRegistry(t, stubSource{metric: okMetric()}), state,
		Options{Limits: Limits{MaxRounds: 1, MaxToolCalls: 5}})

	res, err := o.Run(context.Background(), "错误率？")
	if err != nil {
		t.Fatalf("总结失败也不该让整轮任务失败: %v", err)
	}
	if res.Stop != StopMaxRounds {
		t.Errorf("Stop = %s, 期望 max_rounds", res.Stop)
	}
	if !strings.Contains(res.Answer, "总结调用失败") {
		t.Errorf("应退化为本地摘要: %q", res.Answer)
	}
	if !strings.Contains(res.Answer, "query_metrics") {
		t.Errorf("本地摘要应列出已收集到的进展: %q", res.Answer)
	}
	if got := state.lastStatus(); got != store.TaskStatusSuccess {
		t.Errorf("状态 = %q, 期望 success", got)
	}
}

func TestNonHarmlessToolIsRefusedBeforeExecution(t *testing.T) {
	restart := &stubTool{spec: tools.Spec{
		Name:            "restart_service",
		Description:     "重启指定服务。属于高危操作，必须经过人工审批。",
		Parameters:      json.RawMessage(`{"type":"object","properties":{"service":{"type":"string"}},"required":["service"]}`),
		Tier:            tools.TierDangerous,
		RequireApproval: true,
		ApprovalRole:    "sre",
		ApprovalTTL:     10 * time.Minute,
		Timeout:         5 * time.Second,
		Idempotent:      false,
		MaxRetry:        0,
	}}
	reg := tools.NewRegistry()
	if err := reg.Register(restart); err != nil {
		t.Fatalf("注册高危工具失败: %v", err)
	}

	client := &fakeLLM{responses: []llm.ChatResponse{
		toolCallResponse(call("call_1", "restart_service", `{"service":"payment-service"}`)),
		answerResponse("该操作需要审批。"),
	}}
	o := New(client, reg, &fakeState{}, Options{})

	res, err := o.Run(context.Background(), "重启 payment-service")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if restart.called {
		t.Fatal("高危工具绝不能在 D6 之前被执行")
	}
	if len(res.Traces) != 1 || !res.Traces[0].Rejected {
		t.Fatalf("应留下一条「被拒绝」的轨迹: %+v", res.Traces)
	}
	toolMsgs := toolMessagesOf(client.requests[1])
	if len(toolMsgs) != 1 || !strings.Contains(toolMsgs[0].Content, "needs_approval") {
		t.Fatalf("必须以 needs_approval 回喂: %+v", toolMsgs)
	}
}

func TestHallucinatedToolNameIsReported(t *testing.T) {
	client := &fakeLLM{responses: []llm.ChatResponse{
		toolCallResponse(call("call_1", "delete_everything", `{}`)),
		answerResponse("没有这个工具。"),
	}}
	o := New(client, defaultRegistry(t, stubSource{metric: okMetric()}), &fakeState{}, Options{})

	if _, err := o.Run(context.Background(), "把一切都删掉"); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	toolMsgs := toolMessagesOf(client.requests[1])
	if len(toolMsgs) != 1 {
		t.Fatalf("应回喂一条错误结果: %+v", toolMsgs)
	}
	content := toolMsgs[0].Content
	if !strings.Contains(content, "unknown_tool") {
		t.Errorf("应标记为 unknown_tool: %s", content)
	}
	for _, name := range []string{"query_metrics", "query_config", "list_services"} {
		if !strings.Contains(content, name) {
			t.Errorf("应把可用工具清单告诉模型，缺 %s: %s", name, content)
		}
	}
}

func TestStepPersistFailureFailsTheRun(t *testing.T) {
	client := &fakeLLM{responses: []llm.ChatResponse{
		toolCallResponse(call("call_1", "query_metrics", `{"service":"payment-service","metric":"error_rate"}`)),
		answerResponse("不该走到这里"),
	}}
	state := &fakeState{failOnAppend: true}
	o := New(client, defaultRegistry(t, stubSource{metric: okMetric()}), state, Options{})

	_, err := o.Run(context.Background(), "错误率？")
	if err == nil {
		t.Fatal("状态机写不进去时必须让本次运行失败，而不是带着不完整的记录继续跑")
	}
	if got := state.lastStatus(); got != store.TaskStatusFailed {
		t.Errorf("状态 = %q, 期望 failed", got)
	}
}

func TestModelFailureMarksTaskFailed(t *testing.T) {
	client := &fakeLLM{errs: []error{errors.New("model unreachable")}}
	state := &fakeState{}
	o := New(client, defaultRegistry(t, stubSource{metric: okMetric()}), state, Options{})

	_, err := o.Run(context.Background(), "任意问题")
	if err == nil {
		t.Fatal("模型调不通时应返回错误")
	}
	if got := state.lastStatus(); got != store.TaskStatusFailed {
		t.Errorf("状态 = %q, 期望 failed", got)
	}
}

func TestEventsAreEmittedInOrder(t *testing.T) {
	client := &fakeLLM{responses: []llm.ChatResponse{
		toolCallResponse(call("call_1", "query_metrics", `{"service":"payment-service","metric":"error_rate"}`)),
		answerResponse("结论。"),
	}}
	var kinds []string
	o := New(client, defaultRegistry(t, stubSource{metric: okMetric()}), &fakeState{}, Options{
		OnEvent: func(ev Event) { kinds = append(kinds, ev.Kind) },
	})

	if _, err := o.Run(context.Background(), "错误率？"); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	want := []string{"assistant", "tool", "assistant", "final"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Errorf("事件序列 = %v, 期望 %v", kinds, want)
	}
}

// ---- D4：执行器接线 ----

// flakySource 前 failures 次查询返回"可重试"的网络错误，之后成功。
type flakySource struct {
	stubSource
	failures int
	calls    int
}

func (s *flakySource) QueryMetrics(ctx context.Context, service, metric string, window time.Duration) (tools.MetricSummary, error) {
	s.calls++
	if s.calls <= s.failures {
		return tools.MetricSummary{}, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	}
	return s.stubSource.QueryMetrics(ctx, service, metric, window)
}

// fastExecutor 用毫秒级退避，避免测试真的等 300ms。
func fastExecutor() *executor.Executor {
	return executor.New(executor.Config{
		Breakers: executor.BreakerConfig{Window: time.Minute, Threshold: 5, OpenFor: 30 * time.Second},
		Retry: executor.RetryPolicy{
			BaseDelay: time.Millisecond,
			MaxDelay:  5 * time.Millisecond,
			Jitter:    func(time.Duration) time.Duration { return 0 },
		},
	})
}

// TestRetryableToolFailureIsRetriedThenRecorded 验证 D4 接线是否真的生效：
// 可重试失败要被重试，且轨迹里要能看出"这次成功是重试换来的"。
func TestRetryableToolFailureIsRetriedThenRecorded(t *testing.T) {
	src := &flakySource{failures: 2, stubSource: stubSource{metric: okMetric()}}
	client := &fakeLLM{responses: []llm.ChatResponse{
		toolCallResponse(call("call_1", "query_metrics", `{"service":"payment-service","metric":"error_rate"}`)),
		answerResponse("重试后拿到了数据。"),
	}}
	state := &fakeState{}
	o := New(client, defaultRegistry(t, src), state, Options{Runner: fastExecutor()})

	res, err := o.Run(context.Background(), "错误率？")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if src.calls != 3 {
		t.Errorf("数据源被调用 %d 次, 期望 3（2 次失败 + 1 次成功）", src.calls)
	}
	if len(res.Traces) != 1 {
		t.Fatalf("轨迹条数 = %d, 期望 1", len(res.Traces))
	}
	trace := res.Traces[0]
	if trace.Attempts != 3 {
		t.Errorf("Attempts = %d, 期望 3", trace.Attempts)
	}
	if trace.Class != "ok" {
		t.Errorf("Class = %q, 期望 ok", trace.Class)
	}
	if !strings.Contains(trace.Summary, "重试 2 次后成功") {
		t.Errorf("摘要应说明重试过: %q", trace.Summary)
	}
	// 步骤里也要能看出尝试次数（排障时最有用的一条信息）
	if len(state.steps) != 1 || state.steps[0].Status != store.StepStatusDone {
		t.Fatalf("步骤状态异常: %+v", state.steps)
	}
	if plan := *state.steps[0].Plan; !strings.Contains(plan, `"class":"ok"`) {
		t.Errorf("plan 里应记录分类: %s", plan)
	}
}

// TestRetryableExhaustedIsFedBackAsUpstreamUnavailable：
// 重试也救不回来时，要告诉模型"是系统侧问题、已重试 N 次"，而不是让它去改参数。
func TestRetryableExhaustedIsFedBackAsUpstreamUnavailable(t *testing.T) {
	src := &flakySource{failures: 99, stubSource: stubSource{metric: okMetric()}}
	client := &fakeLLM{responses: []llm.ChatResponse{
		toolCallResponse(call("call_1", "query_metrics", `{"service":"payment-service","metric":"error_rate"}`)),
		answerResponse("查询暂不可用。"),
	}}
	o := New(client, defaultRegistry(t, src), &fakeState{}, Options{Runner: fastExecutor()})

	res, err := o.Run(context.Background(), "错误率？")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if res.Traces[0].Class != "retryable" {
		t.Errorf("Class = %q, 期望 retryable", res.Traces[0].Class)
	}
	toolMsgs := toolMessagesOf(client.requests[1])
	if len(toolMsgs) != 1 {
		t.Fatalf("应有 1 条工具错误结果: %+v", toolMsgs)
	}
	content := toolMsgs[0].Content
	if !strings.Contains(content, "upstream_unavailable") {
		t.Errorf("应以 upstream_unavailable 回喂: %s", content)
	}
	if !strings.Contains(content, "已重试 2 次") {
		t.Errorf("要告诉模型重试过（避免它立刻再试一遍）: %s", content)
	}
	if !strings.Contains(content, "不是你参数的问题") {
		t.Errorf("要明确不是参数问题: %s", content)
	}
}

// ---- D6：审批挂起 ----

// memIntents / memTickets 是审批门禁的最小内存替身。
type memIntents struct{ rows map[string]*store.ToolCall }

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

func (m *memTickets) ListPending(context.Context) ([]store.Approval, error) {
	var out []store.Approval
	for _, t := range m.rows {
		if t.Status == store.ApprovalStatusPending {
			out = append(out, *t)
		}
	}
	return out, nil
}

func (m *memTickets) ExpireOverdue(context.Context, time.Time) (int64, error) { return 0, nil }

func newParkingGate() (*approval.Gate, *memIntents, *memTickets) {
	intents := &memIntents{rows: map[string]*store.ToolCall{}}
	tickets := &memTickets{rows: map[string]*store.Approval{}, idem: map[string]string{}}
	return approval.NewGate(intents, tickets), intents, tickets
}

// TestApprovalParkingEndsRunWithoutExecuting 是 D6 的核心验收：
// 高危工具被拦住 → 先落库（意图 + 工单）→ 本次运行以 waiting_approval 结束。
func TestApprovalParkingEndsRunWithoutExecuting(t *testing.T) {
	restart := &stubTool{spec: tools.Spec{
		Name:            "restart_service",
		Description:     "重启指定服务。属于高危操作，必须经过人工审批。",
		Parameters:      json.RawMessage(`{"type":"object","properties":{"service":{"type":"string"}},"required":["service"]}`),
		Tier:            tools.TierDangerous,
		RequireApproval: true,
		ApprovalRole:    "sre",
		ApprovalTTL:     15 * time.Minute,
		Timeout:         5 * time.Second,
		Idempotent:      false,
		MaxRetry:        0,
	}}
	reg := tools.NewRegistry()
	if err := reg.Register(restart); err != nil {
		t.Fatalf("注册高危工具失败: %v", err)
	}

	gate, intents, tickets := newParkingGate()
	client := &fakeLLM{responses: []llm.ChatResponse{
		toolCallResponse(call("call_1", "restart_service", `{"service":"payment-service"}`)),
		answerResponse("不该走到这里"),
	}}
	state := &fakeState{}
	o := New(client, reg, state, Options{Gate: gate})

	res, err := o.Run(context.Background(), "重启 payment-service")
	if err != nil {
		t.Fatalf("挂起不应返回错误: %v", err)
	}

	if res.Stop != StopWaitingApproval {
		t.Errorf("Stop = %s, 期望 waiting_approval", res.Stop)
	}
	if !res.WaitingApproval() {
		t.Error("WaitingApproval 应为 true")
	}
	if res.LimitTriggered() {
		t.Error("等待审批不是上限触发——两者是完全不同的信号")
	}
	if res.Ticket == nil {
		t.Fatal("应当带回审批工单")
	}
	if !strings.HasPrefix(res.Ticket.TicketID, "tk_") {
		t.Errorf("工单号异常: %s", res.Ticket.TicketID)
	}
	if res.Ticket.Preview == nil || *res.Ticket.Preview == "" {
		t.Error("工单必须带预览")
	}

	// 高危工具绝不能被真的执行
	if restart.called {
		t.Error("需要审批的工具不能被直接执行")
	}
	// 挂起后不应继续调模型
	if len(client.requests) != 1 {
		t.Errorf("模型被调用 %d 次, 期望 1（挂起后应停止）", len(client.requests))
	}

	// 先落库：意图 + 工单
	if len(intents.rows) != 1 {
		t.Errorf("执行意图未落库：%d", len(intents.rows))
	}
	for _, row := range intents.rows {
		if row.Status != store.ToolCallStatusWaitingApproval {
			t.Errorf("意图状态 = %s, 期望 waiting_approval", row.Status)
		}
	}
	if len(tickets.rows) != 1 {
		t.Errorf("审批工单未落库：%d", len(tickets.rows))
	}

	// 任务状态与步骤
	if got := state.lastStatus(); got != store.TaskStatusWaitingApproval {
		t.Errorf("任务状态 = %s, 期望 waiting_approval", got)
	}
	if len(state.steps) != 1 || state.steps[0].Status != store.StepStatusSkipped {
		t.Errorf("步骤应记为 skipped: %+v", state.steps)
	}
	if len(res.Traces) != 1 || res.Traces[0].Class != "waiting_approval" {
		t.Errorf("轨迹应标记 waiting_approval: %+v", res.Traces)
	}
}

// stubWrite 是内存版写数据源（续跑测试要真的执行一次写操作，但不能碰真库）。
type stubWrite struct {
	applied     map[string]string
	statusCalls int
}

func newStubWrite() *stubWrite { return &stubWrite{applied: map[string]string{}} }

func (s *stubWrite) ServiceExists(context.Context, string) (bool, error) { return true, nil }

func (s *stubWrite) ConfigValue(context.Context, string, string) (string, bool, error) {
	return "1500", true, nil
}

// ApplyConfig 按幂等 key 做下游幂等：同一个 key 再来一次不会重复生效。
func (s *stubWrite) ApplyConfig(_ context.Context, idemKey, service, key, newValue string) (string, bool, error) {
	if _, seen := s.applied[idemKey]; seen {
		return "1500", false, nil
	}
	s.applied[idemKey] = service + "|" + key + "=" + newValue
	return "1500", true, nil
}

func (s *stubWrite) SetServiceStatus(context.Context, string, string) error {
	s.statusCalls++
	return nil
}

// TestInvalidArgsDoNotCreateTicket 钉住审批前的参数校验：
// 参数本身就不合法的调用，不该生成一张"批了也执行不了"的工单。
func TestInvalidArgsDoNotCreateTicket(t *testing.T) {
	// 用真实工具定义：只用到 Spec 与参数校验，不会真的执行（数据源传 nil 也无妨）。
	reg, err := tools.NewFullRegistry(nil, nil)
	if err != nil {
		t.Fatalf("注册工具失败: %v", err)
	}
	gate, intents, tickets := newParkingGate()

	client := &fakeLLM{responses: []llm.ChatResponse{
		toolCallResponse(call("call_1", "restart_service", `{}`)), // 缺少必填的 service
		answerResponse("参数缺少服务名，请补充。"),
	}}
	o := New(client, reg, &fakeState{}, Options{Gate: gate})

	res, err := o.Run(context.Background(), "重启一下")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if res.WaitingApproval() {
		t.Error("参数不合法时不应挂起")
	}
	if len(tickets.rows) != 0 || len(intents.rows) != 0 {
		t.Errorf("参数不合法时不该产生工单/意图：tickets=%d intents=%d", len(tickets.rows), len(intents.rows))
	}
	if res.Traces[0].Class != "bad_input" {
		t.Errorf("轨迹分类 = %q, 期望 bad_input", res.Traces[0].Class)
	}
}

// TestResumeExecutesApprovedCallOnOriginalStep 是 D7 的核心验收：
// 挂起 → 审批 → 恢复，被挂起的调用必须在**原 step_no** 上执行，且只执行一次。
func TestResumeExecutesApprovedCallOnOriginalStep(t *testing.T) {
	write := newStubWrite()
	reg, err := tools.NewFullRegistry(stubSource{}, write)
	if err != nil {
		t.Fatalf("注册工具失败: %v", err)
	}
	gate, intents, tickets := newParkingGate()

	client := &fakeLLM{responses: []llm.ChatResponse{
		toolCallResponse(call("call_1", "restart_service", `{"service":"payment-service","reason":"错误率上升"}`)),
		answerResponse("已重启并确认恢复。"),
	}}
	state := &fakeState{}
	o := New(client, reg, state, Options{TaskID: "task_resume", Gate: gate})

	// ① 首次运行：被拦住并挂起
	res, err := o.Run(context.Background(), "重启 payment-service")
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if !res.WaitingApproval() || res.Ticket == nil {
		t.Fatalf("应当挂起，实际 stop=%s", res.Stop)
	}
	if write.statusCalls != 0 {
		t.Fatal("挂起时不该执行任何写操作")
	}
	parkedStepNo := res.Traces[0].StepNo
	parkedIdemKey := res.Traces[0].IdemKey
	if parkedStepNo == 0 || parkedIdemKey == "" {
		t.Fatalf("挂起轨迹缺少 step_no/idem_key: %+v", res.Traces[0])
	}

	// ② 人工审批
	if _, err := gate.Decide(context.Background(), res.Ticket.TicketID, store.ApprovalStatusApproved, "tester", "同意"); err != nil {
		t.Fatalf("审批失败: %v", err)
	}

	// ③ 从断点恢复
	res2, err := o.Resume(context.Background(), "task_resume")
	if err != nil {
		t.Fatalf("Resume 失败: %v", err)
	}
	if res2.Stop != StopAnswered {
		t.Errorf("恢复后应正常结束，实际 stop=%s", res2.Stop)
	}
	if len(res2.Traces) == 0 {
		t.Fatal("恢复后应当有轨迹")
	}
	// 原步骤号 = 原幂等 key（key 由 task/step/tool/canonical args 算出来）。
	// 注意：默认执行器（不落库）不产生 idem_key，所以这里只在有值时比对；
	// "库里那一行被复用"由真实数据库集成测试来证明。
	if got := res2.Traces[0]; got.StepNo != parkedStepNo {
		t.Errorf("恢复时必须在原步骤号上执行：step %d, 期望 %d", got.StepNo, parkedStepNo)
	}
	if got := res2.Traces[0]; got.IdemKey != "" && got.IdemKey != parkedIdemKey {
		t.Errorf("恢复时的幂等 key 变了：%s != %s", got.IdemKey, parkedIdemKey)
	}

	// ④ 副作用只发生一次（重启不是幂等操作，重复执行就是事故）
	if write.statusCalls != 1 {
		t.Errorf("重启被调用了 %d 次，期望 1 次", write.statusCalls)
	}
	if len(intents.rows) != 1 {
		t.Errorf("执行意图应只有 1 条，实际 %d", len(intents.rows))
	}
	if len(tickets.rows) != 1 {
		t.Errorf("工单应只有 1 张，实际 %d", len(tickets.rows))
	}
	if got := state.lastStatus(); got != store.TaskStatusSuccess {
		t.Errorf("任务状态 = %s, 期望 success", got)
	}
}

// TestResumeFeedsRejectionBack 审批被拒时，恢复应把结论回喂给模型，而不是执行。
func TestResumeFeedsRejectionBack(t *testing.T) {
	write := newStubWrite()
	reg, err := tools.NewFullRegistry(stubSource{}, write)
	if err != nil {
		t.Fatalf("注册工具失败: %v", err)
	}
	gate, _, _ := newParkingGate()

	client := &fakeLLM{responses: []llm.ChatResponse{
		toolCallResponse(call("call_1", "restart_service", `{"service":"payment-service"}`)),
		answerResponse("已按你的要求不做重启，改为观察指标。"),
	}}
	o := New(client, reg, &fakeState{}, Options{TaskID: "task_reject", Gate: gate})

	res, err := o.Run(context.Background(), "重启 payment-service")
	if err != nil || !res.WaitingApproval() {
		t.Fatalf("应当挂起: err=%v stop=%s", err, res.Stop)
	}
	if _, err := gate.Decide(context.Background(), res.Ticket.TicketID,
		store.ApprovalStatusRejected, "tester", "影响面太大，先观察"); err != nil {
		t.Fatalf("拒绝失败: %v", err)
	}

	res2, err := o.Resume(context.Background(), "task_reject")
	if err != nil {
		t.Fatalf("Resume 失败: %v", err)
	}
	if write.statusCalls != 0 {
		t.Errorf("被拒绝的操作绝不能执行，实际调用 %d 次", write.statusCalls)
	}
	if res2.Stop != StopAnswered {
		t.Errorf("恢复后应正常结束，实际 stop=%s", res2.Stop)
	}
	if got := res2.Traces[0]; got.Class != "approval_rejected" || !got.Rejected {
		t.Errorf("轨迹应标记 approval_rejected: %+v", got)
	}
	msgs := toolMessagesOf(client.requests[len(client.requests)-1])
	if len(msgs) == 0 || !strings.Contains(msgs[len(msgs)-1].Content, "影响面太大") {
		t.Errorf("拒绝理由应回喂给模型: %+v", msgs)
	}
}
