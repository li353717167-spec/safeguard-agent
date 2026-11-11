package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/mechrevo/safeguard-agent/internal/llm"
	"github.com/mechrevo/safeguard-agent/internal/tools"
)

func TestParsePlanStripsFences(t *testing.T) {
	raw := "```json\n{\"steps\":[{\"tool\":\"query_metrics\",\"args\":{\"service\":\"a\"},\"purpose\":\"看指标\"}]}\n```"
	plan, err := ParsePlan(raw)
	if err != nil {
		t.Fatalf("应容忍 Markdown 围栏: %v", err)
	}
	if len(plan.Steps) != 1 || plan.Steps[0].Tool != "query_metrics" {
		t.Fatalf("解析结果不对: %+v", plan)
	}

	if _, err := ParsePlan("这不是 JSON"); err == nil {
		t.Error("非 JSON 应报错")
	}
}

func TestValidatePlanRejectsBadSteps(t *testing.T) {
	reg, err := tools.NewFullRegistry(stubSource{}, newStubWrite())
	if err != nil {
		t.Fatalf("注册工具失败: %v", err)
	}
	o := New(&fakeLLM{}, reg, &fakeState{}, Options{TaskID: "t"})

	// 工具不存在
	if _, err := o.ValidatePlan(Plan{Steps: []PlanStep{{Tool: "nope", Args: []byte(`{}`)}}}); err == nil {
		t.Error("不存在的工具应被拒绝")
	}
	// 参数不合法（query_metrics 需要 service）
	if _, err := o.ValidatePlan(Plan{Steps: []PlanStep{{Tool: "query_metrics", Args: []byte(`{}`)}}}); err == nil {
		t.Error("缺少必填参数应被拒绝")
	}
	// 空计划
	if _, err := o.ValidatePlan(Plan{}); err == nil {
		t.Error("空计划应被拒绝")
	}
	// 正常计划
	if _, err := o.ValidatePlan(Plan{Steps: []PlanStep{
		{Tool: "query_metrics", Args: []byte(`{"service":"payment-service","metric":"error_rate"}`)},
	}}); err != nil {
		t.Errorf("合法计划不该被拒: %v", err)
	}
}

// Plan 模式：一次规划 + 按计划执行 + 一次总结。
//
// 关键指标：**模型调用次数 = 2**，与计划步数无关；
// 同样 2 步的 ReAct 需要 3 次（每步一次 + 总结一次）。这就是 Plan 的结构性优势。
func TestPlanModeExecutesStepsWithTwoModelCalls(t *testing.T) {
	reg, err := tools.NewFullRegistry(stubSource{}, newStubWrite())
	if err != nil {
		t.Fatalf("注册工具失败: %v", err)
	}
	plannerJSON := `{"steps":[
		{"tool":"query_metrics","args":{"service":"payment-service","metric":"error_rate"},"purpose":"看错误率"},
		{"tool":"query_config","args":{"service":"payment-service"},"purpose":"看配置"}
	]}`
	client := &fakeLLM{responses: []llm.ChatResponse{
		answerResponse(plannerJSON), // 规划
		answerResponse("payment-service 错误率 0.1%，timeout_ms=800。"), // 总结
	}}

	var sawPlanEvent bool
	o := New(client, reg, &fakeState{}, Options{
		TaskID: "task_plan", Mode: ModePlan,
		OnEvent: func(ev Event) {
			if ev.Kind == "plan" {
				sawPlanEvent = true
			}
		},
	})
	res, err := o.Run(context.Background(), "payment-service 有什么问题")
	if err != nil {
		t.Fatalf("Plan 模式执行失败: %v", err)
	}

	if len(res.Traces) != 2 {
		t.Fatalf("应按计划执行 2 步，实际 %d", len(res.Traces))
	}
	if res.Traces[0].ToolName != "query_metrics" || res.Traces[1].ToolName != "query_config" {
		t.Errorf("执行顺序不对: %+v", res.Traces)
	}
	if res.Stop != StopAnswered {
		t.Errorf("停止原因 = %s，期望 answered", res.Stop)
	}
	if !strings.Contains(res.Answer, "0.1%") {
		t.Errorf("最终回答应来自总结调用: %q", res.Answer)
	}
	if got := len(client.requests); got != 2 {
		t.Errorf("模型调用次数 = %d，期望 2（规划 + 总结）", got)
	}

	// 计划事件必须可见：排障时要能回答"它当初打算怎么做"
	if !sawPlanEvent {
		t.Error("应发出 plan 事件")
	}
}

// 规划器返回坏计划：直接失败，不进入执行（不能"边猜边跑"）。
func TestPlanModeFailsOnInvalidPlan(t *testing.T) {
	reg, err := tools.NewFullRegistry(stubSource{}, newStubWrite())
	if err != nil {
		t.Fatalf("注册工具失败: %v", err)
	}
	client := &fakeLLM{responses: []llm.ChatResponse{
		answerResponse(`{"steps":[{"tool":"rm_rf","args":{}}]}`),
	}}
	o := New(client, reg, &fakeState{}, Options{TaskID: "task_plan_bad", Mode: ModePlan})
	if _, err := o.Run(context.Background(), "随便做点什么"); err == nil {
		t.Fatal("非法计划应让任务失败")
	}
}
