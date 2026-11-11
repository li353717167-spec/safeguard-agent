package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mechrevo/safeguard-agent/internal/llm"
	"github.com/mechrevo/safeguard-agent/internal/store"
)

// 执行策略：同一个编排器，两种"想"的方式。
const (
	// ModeReact 是默认的 ReAct：每一步都问模型"下一步做什么"。
	ModeReact = "react"
	// ModePlan 是先规划再执行：一次规划出 N 步，然后按计划逐条执行。
	ModePlan = "plan"
)

// planStep 是计划里的一步。
//
// 只允许"调用一个工具"这一种步骤：规划器不负责回答问题，
// 最终总结由执行器单独发一次（职责分开，计划的形状也更稳定）。
type PlanStep struct {
	Tool    string          `json:"tool"`
	Args    json.RawMessage `json:"args"`
	Purpose string          `json:"purpose"`
}

// Plan 是一份执行计划。
type Plan struct {
	Steps []PlanStep `json:"steps"`
	Note  string     `json:"note,omitempty"`
}

// maxPlanSteps 是一次规划允许的最大步数。
//
// 上限不是拍脑袋：计划越长，越可能在中途因为数据变化而失效，
// 而且一次生成 10 步的 JSON 本身就容易截断/跑偏。6 步覆盖绝大多数运维排查。
const maxPlanSteps = 6

// plannerSystemPrompt 是规划器的系统提示。
//
// 要求"只输出 JSON"并且在代码里做严格校验（工具必须存在、参数必须过工具自己的 Validate）。
// 提示词只是降低出错率，真正的约束在 parsePlan/validatePlan 里。
const plannerSystemPrompt = `你是任务规划器。根据用户请求和可用工具，输出一个**严格的 JSON 对象**，不要任何解释、不要 Markdown 代码块。

格式：
{"steps":[{"tool":"工具名","args":{...},"purpose":"这一步要查什么，一句中文"}],"note":"可选"}

规则：
1. steps 1..6 步；只使用给定的工具；args 必须符合该工具的参数 schema。
2. 数据已经有专用工具（服务列表/指标/配置）时，优先用它们，不要用 shell 代替。
3. 只有用户明确要求时才规划高危/变更类工具（restart/scale/update/shell_admin）。
4. 不要规划"总结/回答"这类步骤——执行完会单独总结。
5. 步骤之间不要重复查询同一数据。`

// ParsePlan 解析模型输出的计划，并把 ```json 围栏剥掉。
func ParsePlan(content string) (Plan, error) {
	s := strings.TrimSpace(content)
	// 容忍模型加围栏或前后寒暄：只取第一个 { 到最后一个 }。
	if i := strings.Index(s, "{"); i > 0 {
		s = s[i:]
	}
	if i := strings.LastIndex(s, "}"); i >= 0 {
		s = s[:i+1]
	}
	var plan Plan
	if err := json.Unmarshal([]byte(s), &plan); err != nil {
		return Plan{}, fmt.Errorf("计划不是合法 JSON：%w", err)
	}
	return plan, nil
}

// ValidatePlan 用**工具注册表**校验计划：工具存在、参数过工具自己的校验。
//
// 这是 Plan 模式最关键的一步：模型写的计划在真正执行前就被拒绝，
// 而不是等执行到第 3 步才发现工具名是编的。
func (o *Orchestrator) ValidatePlan(plan Plan) (Plan, error) {
	if len(plan.Steps) == 0 {
		return Plan{}, errors.New("计划为空：至少需要 1 步")
	}
	if len(plan.Steps) > maxPlanSteps {
		return Plan{}, fmt.Errorf("计划过长：%d 步（上限 %d）", len(plan.Steps), maxPlanSteps)
	}
	for i, step := range plan.Steps {
		tool, ok := o.registry.Get(step.Tool)
		if !ok {
			return Plan{}, fmt.Errorf("第 %d 步的工具 %q 不存在", i+1, step.Tool)
		}
		if len(step.Args) == 0 {
			step.Args = json.RawMessage(`{}`)
		}
		if err := tool.Validate(step.Args); err != nil {
			return Plan{}, fmt.Errorf("第 %d 步（%s）参数不合法：%w", i+1, step.Tool, err)
		}
		plan.Steps[i] = step
	}
	return plan, nil
}

// plan 调一次模型做规划，并做严格校验。
func (r *run) plan(ctx context.Context, query string) (Plan, error) {
	prompt, err := r.plannerPrompt(query)
	if err != nil {
		return Plan{}, err
	}
	resp, err := r.chatWith(ctx, prompt, nil)
	if err != nil {
		return Plan{}, fmt.Errorf("规划调用失败：%w", err)
	}
	plan, err := ParsePlan(resp.Message.Content)
	if err != nil {
		return Plan{}, err
	}
	return r.o.ValidatePlan(plan)
}

// plannerPrompt 组装规划器消息：系统规则 + 用户请求 + 工具目录。
func (r *run) plannerPrompt(query string) ([]llm.Message, error) {
	schemas := r.o.registry.Schemas()
	var b strings.Builder
	b.WriteString("可用工具（name / description / parameters）：\n")
	for _, s := range schemas {
		desc := s.Function.Description
		if idx := strings.IndexByte(desc, '\n'); idx > 0 {
			desc = desc[:idx]
		}
		fmt.Fprintf(&b, "- %s：%s\n  params=%s\n", s.Function.Name, desc, string(s.Function.Parameters))
	}
	return []llm.Message{
		{Role: llm.RoleSystem, Content: plannerSystemPrompt},
		{Role: llm.RoleUser, Content: "用户请求：" + query + "\n\n" + b.String()},
	}, nil
}

// runPlan 是 Plan-and-Execute 的执行体。
//
// 与 ReAct 的关系：**每一步仍然走完全相同的安全链路**（策略判定 → 审批挂起 → 幂等落库 → 审计），
// 区别只在"谁决定下一步"：ReAct 每步问模型，Plan 一次问完、按计划走。
func (r *run) runPlan(ctx context.Context, query string, resume *resumeState) (Result, error) {
	plan, err := r.obtainPlan(ctx, query)
	if err != nil {
		r.markFailed(ctx)
		return r.res, err
	}

	r.emit(Event{Round: 0, Kind: "plan", Text: fmt.Sprintf("计划（%d 步）：%s", len(plan.Steps), summarizePlan(plan))})

	start := 0
	if resume != nil {
		// 恢复：task_steps 里已有的步骤会被幂等执行器**回放**（不打下游），
		// 所以从"已完成步数"继续即可，不需要额外的游标列。
		start = resume.toolCalls
	}

	for i := start; i < len(plan.Steps); i++ {
		if !r.guard.canCallTool() {
			return r.finishWithSummary(ctx, StopMaxToolCalls)
		}
		step := plan.Steps[i]
		call := llm.ToolCall{
			ID:   fmt.Sprintf("plan_%d", i+1),
			Type: "function",
			Function: llm.FunctionCall{
				Name:      step.Tool,
				Arguments: string(step.Args),
			},
		}
		stepNo := r.guard.countToolCall()
		trace, msg, exhausted, err := r.handleAndRecord(ctx, i+1, stepNo, call)
		if err != nil {
			r.markFailed(ctx)
			return r.res, err
		}
		r.res.Traces = append(r.res.Traces, trace)
		r.messages = append(r.messages, msg)
		last := r.res.Traces[len(r.res.Traces)-1]
		r.emit(Event{Round: i + 1, Kind: "tool", Text: trace.Summary, Trace: &last})

		if r.parked != nil {
			return r.finishParked(ctx, *r.parked)
		}
		if exhausted {
			return r.finishWithSummary(ctx, StopMaxToolCalls)
		}
	}
	return r.finalizePlan(ctx)
}

// obtainPlan 调一次规划器并校验。
//
// 计划**不落库**（v1 的取舍）：恢复一个挂了审批的 Plan 任务时，CLI 会按 ReAct 继续——
// 已完成的步骤仍然被幂等 key 挡住（不会重复执行），只是后续步骤不再按原计划走。
// 要"严格按原计划恢复"，需要把计划持久化 + 固定 step_no，属于下一步。
func (r *run) obtainPlan(ctx context.Context, query string) (Plan, error) {
	return r.plan(ctx, query)
}

// finalizePlan 执行完计划后做一次总结回答。
func (r *run) finalizePlan(ctx context.Context) (Result, error) {
	const instruction = "以上是按计划执行的结果。请基于工具返回的真实数据给出最终回答：" +
		"先给结论，再给关键数据；没查到或失败的步骤如实说明；不要编造数值，也不要声称执行了未执行的变更。"

	msgs := r.messages[:len(r.messages):len(r.messages)]
	msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: instruction})

	resp, err := r.chatWith(ctx, msgs, nil)
	r.res.Stop = StopAnswered
	r.syncCounts()
	if err != nil {
		slog.Warn("计划总结调用失败，退化为本地摘要", "task_id", r.taskID, "err", err)
		r.res.Answer = r.localSummary(StopAnswered)
	} else {
		r.res.Answer = strings.TrimSpace(resp.Message.Content)
	}
	if err := r.o.state.UpdateProgress(ctx, r.taskID, store.TaskStatusSuccess, r.res.ToolCalls, r.res.Rounds); err != nil {
		return r.res, err
	}
	r.emit(Event{Round: r.res.Rounds, Kind: "final", Text: r.res.Answer})
	return r.res, nil
}

func summarizePlan(plan Plan) string {
	parts := make([]string, 0, len(plan.Steps))
	for i, s := range plan.Steps {
		parts = append(parts, fmt.Sprintf("%d.%s", i+1, s.Tool))
	}
	return strings.Join(parts, " → ")
}
