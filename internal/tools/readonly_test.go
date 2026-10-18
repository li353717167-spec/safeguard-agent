package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeDataSource 是内存数据源：验证"工具层怎么用数据源"，不需要数据库。
// 这正是 DataSource 接口定义在消费方的好处——评测（D11）也会用同样的方式替身。
type fakeDataSource struct {
	services  []ServiceInfo
	listErr   error
	metric    MetricSummary
	metricErr error
	configs   []ConfigItem
	configErr error

	// 记录最后一次调用，用于断言参数确实被透传下去了。
	gotService, gotMetric string
	gotWindow             time.Duration
}

func (f *fakeDataSource) ListServices(context.Context) ([]ServiceInfo, error) {
	return f.services, f.listErr
}

func (f *fakeDataSource) QueryMetrics(_ context.Context, service, metric string, window time.Duration) (MetricSummary, error) {
	f.gotService, f.gotMetric, f.gotWindow = service, metric, window
	if f.metricErr != nil {
		return MetricSummary{}, f.metricErr
	}
	return f.metric, nil
}

func (f *fakeDataSource) QueryConfig(_ context.Context, service string) ([]ConfigItem, error) {
	f.gotService = service
	if f.configErr != nil {
		return nil, f.configErr
	}
	return f.configs, nil
}

func dataOf(t *testing.T, res Result) map[string]any {
	t.Helper()
	m, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("Result.Data 期望 map[string]any, 实际 %T", res.Data)
	}
	return m
}

func invoke(t *testing.T, reg *Registry, name, args string) Result {
	t.Helper()
	tool, ok := reg.Get(name)
	if !ok {
		t.Fatalf("找不到工具 %q", name)
	}
	res, err := tool.Invoke(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("调用 %s 失败: %v", name, err)
	}
	return res
}

func newRegistryWith(t *testing.T, ds DataSource) *Registry {
	t.Helper()
	reg, err := NewDefaultRegistry(ds)
	if err != nil {
		t.Fatalf("构造注册中心失败: %v", err)
	}
	return reg
}

func TestQueryMetricsToolHappyPath(t *testing.T) {
	ds := &fakeDataSource{metric: MetricSummary{
		Service: "payment-service", Metric: "error_rate", Unit: "%",
		SampleCount: 5, Latest: 0.1, Average: 0.122, Max: 0.15,
		LatestAt: time.Date(2026, 10, 15, 14, 30, 0, 0, time.UTC),
		Window:   5 * time.Minute,
	}}
	reg := newRegistryWith(t, ds)

	res := invoke(t, reg, "query_metrics", `{"service":"payment-service","metric":"error_rate","window":"5m"}`)
	if ds.gotService != "payment-service" || ds.gotMetric != "error_rate" {
		t.Errorf("参数未透传到数据源: service=%q metric=%q", ds.gotService, ds.gotMetric)
	}
	if ds.gotWindow != 5*time.Minute {
		t.Errorf("window 未透传: %v", ds.gotWindow)
	}

	data := dataOf(t, res)
	if data["found"] != true {
		t.Errorf("found = %v, 期望 true", data["found"])
	}
	if data["unit"] != "%" {
		t.Errorf("unit = %v, 期望 %%", data["unit"])
	}
	if data["latest"] != 0.1 {
		t.Errorf("latest = %v, 期望 0.1", data["latest"])
	}
	if data["window"] != "5m" {
		t.Errorf("window = %v, 期望 5m（原样回喂模型，不要 5m0s 这种）", data["window"])
	}
	if !strings.Contains(res.Summary, "payment-service") {
		t.Errorf("Summary 应包含服务名: %q", res.Summary)
	}
}

func TestQueryMetricsToolDefaultsWindow(t *testing.T) {
	ds := &fakeDataSource{metric: MetricSummary{Service: "a", Metric: "qps", SampleCount: 1, Window: defaultQueryWindow}}
	reg := newRegistryWith(t, ds)

	invoke(t, reg, "query_metrics", `{"service":"a","metric":"qps"}`)
	if ds.gotWindow != defaultQueryWindow {
		t.Errorf("未指定 window 时应使用默认值 %v，实际 %v", defaultQueryWindow, ds.gotWindow)
	}
}

// TestNotFoundIsSuccessfulResult 钉住 D11 幻觉陷阱题的前提：
// "服务不存在"必须作为一次**成功**的工具调用回喂模型（内容里写明 found=false），
// 而不是抛错误。否则模型看到的是失败信号，更容易去编一个像样的数字。
func TestNotFoundIsSuccessfulResult(t *testing.T) {
	ds := &fakeDataSource{metricErr: &NotFoundError{
		Kind: "service", Name: "order-service", Hint: "可用 list_services 获取全部服务名",
	}}
	reg := newRegistryWith(t, ds)

	res := invoke(t, reg, "query_metrics", `{"service":"order-service","metric":"error_rate"}`)
	data := dataOf(t, res)
	if data["found"] != false {
		t.Fatalf("found = %v, 期望 false", data["found"])
	}
	if data["target"] != "order-service" {
		t.Errorf("target = %v, 期望 order-service", data["target"])
	}
	if data["hint"] == nil || data["hint"] == "" {
		t.Error("必须给出 hint：让模型知道下一步该做什么（去 list_services），而不是自己编")
	}
	if !strings.Contains(res.Summary, "未找到") {
		t.Errorf("Summary 应说明未找到: %q", res.Summary)
	}
}

// TestSystemErrorStaysAnError：数据源真出故障时不能被当成"查不到"吞掉。
// 这两种情况的处理完全不同：一个回喂模型，一个要触发 D4 的异常分类与重试。
func TestSystemErrorStaysAnError(t *testing.T) {
	ds := &fakeDataSource{metricErr: errors.New("dial tcp: connection refused")}
	reg := newRegistryWith(t, ds)

	tool, _ := reg.Get("query_metrics")
	_, err := tool.Invoke(context.Background(), json.RawMessage(`{"service":"a","metric":"qps"}`))
	if err == nil {
		t.Fatal("系统错误必须向上抛，不能被当成 not_found")
	}
	if _, ok := IsNotFound(err); ok {
		t.Error("系统错误不应被判定为 not_found")
	}
}

func TestQueryConfigTool(t *testing.T) {
	ds := &fakeDataSource{configs: []ConfigItem{
		{Key: "timeout_ms", Value: "800", UpdatedAt: time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)},
		{Key: "max_connections", Value: "512", UpdatedAt: time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)},
	}}
	reg := newRegistryWith(t, ds)

	res := invoke(t, reg, "query_config", `{"service":"payment-service"}`)
	data := dataOf(t, res)
	if data["count"] != 2 {
		t.Errorf("count = %v, 期望 2", data["count"])
	}
	configs, ok := data["configs"].([]map[string]any)
	if !ok || len(configs) != 2 {
		t.Fatalf("configs 结构异常: %#v", data["configs"])
	}
	if configs[0]["key"] != "timeout_ms" {
		t.Errorf("配置顺序应稳定（按 key 排序由数据源保证），实际首项 %v", configs[0]["key"])
	}
}

func TestListServicesToolCountsUnhealthy(t *testing.T) {
	ds := &fakeDataSource{services: []ServiceInfo{
		{Name: "payment-service", Criticality: "核心", Owner: "支付组", Status: "healthy"},
		{Name: "checkout-service", Criticality: "核心", Owner: "交易组", Status: "degraded"},
		{Name: "search-service", Criticality: "重要", Owner: "搜索组", Status: "degraded"},
	}}
	reg := newRegistryWith(t, ds)

	res := invoke(t, reg, "list_services", `{}`)
	data := dataOf(t, res)
	if data["count"] != 3 {
		t.Errorf("count = %v, 期望 3", data["count"])
	}
	if data["unhealthy"] != 2 {
		t.Errorf("unhealthy = %v, 期望 2", data["unhealthy"])
	}
	if !strings.Contains(res.Summary, "2 个非 healthy") {
		t.Errorf("Summary 应说明非健康数量: %q", res.Summary)
	}
}

// TestInvokeRejectsBadArgs 覆盖"模型给的参数不可信"这条主线。
func TestInvokeRejectsBadArgs(t *testing.T) {
	reg := newRegistryWith(t, &fakeDataSource{})

	cases := []struct {
		name      string
		tool      string
		args      string
		wantInErr string
	}{
		{"未知字段必须被拒绝", "query_metrics", `{"service":"a","metric":"qps","verbose":true}`, "verbose"},
		{"参数不是对象", "query_metrics", `["a","qps"]`, "JSON 对象"},
		{"对象后面有多余内容", "query_metrics", `{"service":"a","metric":"qps"} garbage`, "多余内容"},
		{"缺必填字段", "query_metrics", `{"metric":"qps"}`, "service"},
		{"枚举值非法", "query_metrics", `{"service":"a","metric":"cpu_usage"}`, "metric"},
		{"窗口格式非法", "query_metrics", `{"service":"a","metric":"qps","window":"五分钟"}`, "window"},
		{"窗口超出上限", "query_metrics", `{"service":"a","metric":"qps","window":"720h"}`, "window"},
		{"无参数工具不接受参数", "list_services", `{"verbose":true}`, "verbose"},
		{"空参数对象对无参工具是合法的", "list_services", `{}`, ""},
		{"空参数对无参工具等价于 {}（不会误判）", "list_services", ``, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool, ok := reg.Get(tc.tool)
			if !ok {
				t.Fatalf("找不到工具 %s", tc.tool)
			}
			_, err := tool.Invoke(context.Background(), json.RawMessage(tc.args))
			if tc.wantInErr == "" {
				if err != nil {
					t.Fatalf("期望成功，实际 %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("期望参数校验失败")
			}
			// 参数错误必须可识别：D3 会把它回喂模型自我修正，而不是终止任务。
			if !IsArgError(err) {
				t.Errorf("期望 *ArgError，实际 %T: %v", err, err)
			}
			if !strings.Contains(err.Error(), tc.wantInErr) {
				t.Errorf("错误信息应提到 %q，实际: %v", tc.wantInErr, err)
			}
		})
	}
}
