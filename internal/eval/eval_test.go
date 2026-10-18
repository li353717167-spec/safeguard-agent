package eval

import (
	"context"
	"os"
	"strings"
	"testing"
)

func mustLoadCases(t *testing.T) []Case {
	t.Helper()
	cases, err := LoadCases("cases.json")
	if err != nil {
		t.Fatalf("加载评测集失败: %v", err)
	}
	return cases
}

// TestOracleScoresPerfectly 是 D10 的核心：**判分器自己必须是对的**。
//
// 理想模型按用例期望行动，如果连它都判不过，那问题在判分逻辑，而不是模型。
// 这条测试不需要数据库、不需要真模型，所以可以进 CI。
func TestOracleScoresPerfectly(t *testing.T) {
	cases := mustLoadCases(t)
	results := Evaluate(context.Background(), cases, NewRunner(nil, Options{}))
	m := Aggregate(results)

	if m.Passed != m.Total {
		var fails []string
		for _, r := range results {
			if !r.Verdict.Pass {
				fails = append(fails, r.Case.ID+": "+strings.Join(r.Verdict.Failures, "；"))
			}
		}
		t.Fatalf("理想轨迹必须全部通过，实际 %d/%d：\n%s", m.Passed, m.Total, strings.Join(fails, "\n"))
	}
	if m.TrapPassed != m.TrapTotal {
		t.Errorf("陷阱题应全部通过：%d/%d", m.TrapPassed, m.TrapTotal)
	}
}

// TestBrokenModelGetsCaught 证明判分器**会**判失败——否则"全部通过"毫无意义。
func TestBrokenModelGetsCaught(t *testing.T) {
	cases := mustLoadCases(t)
	results := Evaluate(context.Background(), cases, NewRunner(NewBroken(), Options{}))
	m := Aggregate(results)

	if m.Passed == m.Total {
		t.Fatal("坏模型不该拿到满分：判分器太松")
	}
	// 它永远只调 restart_service，所以"该调 query_metrics 却没调"必须被抓出来。
	// 注意它**不会**被判"执行了禁止调用的工具"——因为审批门禁把它拦住了。
	// 这本身就是一条好结论：门禁在工作时，模型再乱来也动不了手。
	var selectionCaught bool
	for _, r := range results {
		if !r.Verdict.Pass && strings.Contains(strings.Join(r.Verdict.Failures, "；"), "没有调用必需的工具") {
			selectionCaught = true
			break
		}
	}
	if !selectionCaught {
		t.Errorf("应当至少有一条因'工具选择错误'被判失败，实际失败 %d 条", m.Total-m.Passed)
	}

	// 反过来确认：没有任何用例判它"执行了禁止调用的工具"，说明高危工具确实没被执行。
	for _, r := range results {
		if strings.Contains(strings.Join(r.Verdict.Failures, "；"), "执行了禁止调用的工具") {
			t.Errorf("审批门禁应当拦住高危工具，但 %s 报告它被执行了", r.Case.ID)
		}
	}
}

func TestLoadCasesRejectsBadInput(t *testing.T) {
	if _, err := LoadCases("does-not-exist.json"); err == nil {
		t.Error("文件不存在应当报错")
	}

	dir := t.TempDir()
	dup := dir + "/dup.json"
	body := `[{"id":"a","query":"x"},{"id":"a","query":"y"}]`
	if err := os.WriteFile(dup, []byte(body), 0o600); err != nil {
		t.Fatalf("写临时文件失败: %v", err)
	}
	if _, err := LoadCases(dup); err == nil {
		t.Error("id 重复应当报错")
	}
}

// 指标定义要能被验证：通过率 = 通过数/总数，陷阱题单独统计。
func TestAggregateMetrics(t *testing.T) {
	cases := mustLoadCases(t)
	results := Evaluate(context.Background(), cases, NewRunner(nil, Options{}))
	m := Aggregate(results)

	if m.Total != len(cases) {
		t.Errorf("总数 = %d, 期望 %d", m.Total, len(cases))
	}
	if m.GoodTotal+m.TrapTotal != m.Total {
		t.Errorf("正常题 + 陷阱题 = %d, 期望 %d", m.GoodTotal+m.TrapTotal, m.Total)
	}
	if m.PassRate() != 100 || m.TrapRate() != 100 {
		t.Errorf("理想轨迹的通过率/拒绝率都应是 100%%，实际 %.0f%% / %.0f%%", m.PassRate(), m.TrapRate())
	}
	if m.Redundant != 0 {
		t.Errorf("理想轨迹不该有冗余调用，实际 %d", m.Redundant)
	}
}

// 报告要包含全部用例与关键指标，否则没法交给别人看。
func TestRenderMarkdown(t *testing.T) {
	cases := mustLoadCases(t)
	results := Evaluate(context.Background(), cases, NewRunner(nil, Options{}))
	report := RenderMarkdown("测试报告", "oracle", "perfect", results, Aggregate(results))

	for _, want := range []string{"总通过率", "陷阱题拒绝率", "上限触发率", "逐题结果"} {
		if !strings.Contains(report, want) {
			t.Errorf("报告缺少 %q", want)
		}
	}
	for _, c := range cases {
		if !strings.Contains(report, c.ID) {
			t.Errorf("报告缺少用例 %s", c.ID)
		}
	}
}
