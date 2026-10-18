package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/mechrevo/safeguard-agent/internal/store"
)

// 本文件是 D2 的三个只读工具，正好覆盖最小可用的一组查询：
// 查指标（时间序列）、查配置（键值）、列服务（目录）。
//
// 每个工具的 Description 都是认真写过的——它不是注释，是 prompt：
// 说清"什么时候用它、返回什么、查不到会怎样"，模型才可能选对、并且不编数据。

const (
	defaultQueryWindow = 5 * time.Minute
	// maxQueryWindow 限制单次查询的时间范围。
	// 这是"工具层自己管住成本"的地方：窗口越宽扫的行越多，
	// 与其等数据库被拖慢，不如在参数校验这一层就拒绝。
	maxQueryWindow = 24 * time.Hour
)

// ---------------------------------------------------------------------------
// query_metrics
// ---------------------------------------------------------------------------

type queryMetricsArgs struct {
	Service string `json:"service"`
	Metric  string `json:"metric"`
	Window  string `json:"window"`
}

func (a queryMetricsArgs) Validate() error {
	if strings.TrimSpace(a.Service) == "" {
		return argErrf("service 不能为空")
	}
	if !slices.Contains(store.CatalogMetricNames, a.Metric) {
		return argErrf("metric 必须是 %s 之一，收到 %q",
			strings.Join(store.CatalogMetricNames, " / "), a.Metric)
	}
	if a.Window != "" {
		d, err := time.ParseDuration(a.Window)
		if err != nil {
			return argErrf("window 不是合法时长（示例：5m、30m、2h）：%v", err)
		}
		if d <= 0 || d > maxQueryWindow {
			return argErrf("window 必须在 (0, %s] 之间，收到 %q", maxQueryWindow, a.Window)
		}
	}
	return nil
}

func newQueryMetricsTool(ds DataSource) Tool {
	spec := Spec{
		Name: "query_metrics",
		Description: "查询某个服务在最近一段时间内的监控指标汇总（错误率 error_rate、吞吐 qps、P99 延迟 p99_latency）。" +
			"回答\"某服务最近表现如何/有没有异常/延迟是不是变高了\"这类问题时用它。" +
			"返回真实采样点的最新值、均值与峰值；如果服务不存在或该指标没有采样点，" +
			"会明确返回 found=false 与原因，请据此如实回答，不要推测或编造数值。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "service": {"type": "string", "description": "服务名，例如 payment-service"},
    "metric": {"type": "string", "enum": ["error_rate", "qps", "p99_latency"], "description": "要查询的指标"},
    "window": {"type": "string", "description": "时间窗口，如 5m / 30m / 2h，默认 5m"}
  },
  "required": ["service", "metric"]
}`),
		Tier:        TierHarmless,
		Timeout:     5 * time.Second,
		MaxRetry:    2,
		BackoffBase: 100 * time.Millisecond,
		Idempotent:  true,
	}

	return New(spec, func(ctx context.Context, args queryMetricsArgs) (Result, error) {
		window := defaultQueryWindow
		if args.Window != "" {
			d, err := time.ParseDuration(args.Window)
			if err != nil {
				// 到这一步还能失败说明 Validate 与这里的解析逻辑走偏了，属于代码 bug。
				return Result{}, argErrf("window 解析失败: %v", err)
			}
			window = d
		}

		summary, err := ds.QueryMetrics(ctx, args.Service, args.Metric, window)
		if err != nil {
			if nf, ok := IsNotFound(err); ok {
				// 查不到不是失败：作为一次成功的工具结果回喂，让模型据实回答。
				return Result{
					Data: map[string]any{
						"found":  false,
						"kind":   nf.Kind,
						"target": nf.Name,
						"hint":   nf.Hint,
					},
					Summary: fmt.Sprintf("未找到 %s（%s）", nf.Name, nf.Kind),
				}, nil
			}
			return Result{}, err
		}

		return Result{
			Data: map[string]any{
				"found":     true,
				"service":   summary.Service,
				"metric":    summary.Metric,
				"unit":      summary.Unit,
				"window":    formatWindow(summary.Window),
				"samples":   summary.SampleCount,
				"latest":    round3(summary.Latest),
				"average":   round3(summary.Average),
				"max":       round3(summary.Max),
				"latest_at": summary.LatestAt.Format(time.RFC3339),
			},
			Summary: fmt.Sprintf("%s.%s 最近 %s：最新 %s%s（%d 个采样点）",
				summary.Service, summary.Metric, formatWindow(summary.Window),
				formatValue(summary.Latest), summary.Unit, summary.SampleCount),
		}, nil
	})
}

// ---------------------------------------------------------------------------
// query_config
// ---------------------------------------------------------------------------

type queryConfigArgs struct {
	Service string `json:"service"`
}

func (a queryConfigArgs) Validate() error {
	if strings.TrimSpace(a.Service) == "" {
		return argErrf("service 不能为空")
	}
	return nil
}

func newQueryConfigTool(ds DataSource) Tool {
	spec := Spec{
		Name: "query_config",
		Description: "读取某个服务当前生效的配置项（键值对）。" +
			"回答\"某服务的超时/连接数/开关是多少\"这类问题时用它。" +
			"这是只读操作，不会修改任何配置；如果服务不存在或没有登记配置项，会返回 found=false 与原因。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "service": {"type": "string", "description": "服务名，例如 payment-service"}
  },
  "required": ["service"]
}`),
		Tier:        TierHarmless,
		Timeout:     5 * time.Second,
		MaxRetry:    2,
		BackoffBase: 100 * time.Millisecond,
		Idempotent:  true,
	}

	return New(spec, func(ctx context.Context, args queryConfigArgs) (Result, error) {
		items, err := ds.QueryConfig(ctx, args.Service)
		if err != nil {
			if nf, ok := IsNotFound(err); ok {
				return Result{
					Data: map[string]any{
						"found":  false,
						"kind":   nf.Kind,
						"target": nf.Name,
						"hint":   nf.Hint,
					},
					Summary: fmt.Sprintf("未找到 %s 的配置（%s）", nf.Name, nf.Kind),
				}, nil
			}
			return Result{}, err
		}

		configs := make([]map[string]any, 0, len(items))
		for _, it := range items {
			configs = append(configs, map[string]any{
				"key":        it.Key,
				"value":      it.Value,
				"updated_at": it.UpdatedAt.Format(time.RFC3339),
			})
		}
		return Result{
			Data: map[string]any{
				"found":   true,
				"service": args.Service,
				"count":   len(configs),
				"configs": configs,
			},
			Summary: fmt.Sprintf("%s 的配置共 %d 项", args.Service, len(configs)),
		}, nil
	})
}

// ---------------------------------------------------------------------------
// list_services
// ---------------------------------------------------------------------------

// listServicesArgs 刻意是空结构体：这个工具不需要参数。
// 模型仍可能塞参数进来，而 DisallowUnknownFields 会把它们全部拒绝——
// 这正是我们要的：参数契约清晰，幻觉参数无处可藏。
type listServicesArgs struct{}

func (listServicesArgs) Validate() error { return nil }

func newListServicesTool(ds DataSource) Tool {
	spec := Spec{
		Name: "list_services",
		Description: "列出当前纳管的全部服务，以及各自的重要级别、归属团队与健康状态。" +
			"回答\"有哪些服务\"\"哪些服务不健康\"时用它；" +
			"当不确定服务名怎么写时，也应当先用它确认，再去查具体指标或配置。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {}
}`),
		Tier:        TierHarmless,
		Timeout:     5 * time.Second,
		MaxRetry:    2,
		BackoffBase: 100 * time.Millisecond,
		Idempotent:  true,
	}

	return New(spec, func(ctx context.Context, _ listServicesArgs) (Result, error) {
		services, err := ds.ListServices(ctx)
		if err != nil {
			return Result{}, err
		}

		items := make([]map[string]any, 0, len(services))
		unhealthy := 0
		for _, s := range services {
			if s.Status != store.ServiceStatusHealthy {
				unhealthy++
			}
			items = append(items, map[string]any{
				"name":        s.Name,
				"criticality": s.Criticality,
				"owner":       s.Owner,
				"status":      s.Status,
				"description": s.Description,
			})
		}
		return Result{
			Data: map[string]any{
				"count":     len(items),
				"unhealthy": unhealthy,
				"services":  items,
			},
			Summary: fmt.Sprintf("共 %d 个服务，其中 %d 个非 healthy", len(items), unhealthy),
		}, nil
	})
}

// ---------------------------------------------------------------------------
// 组装
// ---------------------------------------------------------------------------

// NewDefaultRegistry 注册**只读**工具。
//
// 用在只需要查询能力的场景（单轮探针、评测的只读用例），也让既有测试不必实现写接口。
//
// 注意：ds 允许为 nil——`-tools` 只打印清单、不执行工具，不需要数据源。
func NewDefaultRegistry(ds DataSource) (*Registry, error) {
	r := NewRegistry()
	for _, t := range []Tool{
		newListServicesTool(ds),
		newQueryMetricsTool(ds),
		newQueryConfigTool(ds),
	} {
		if err := r.Register(t); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// NewFullRegistry 注册全部内置工具：只读 + 写（update_config）+ 高危（restart / scale）。
//
// 这是 `-ask` 实际使用的注册表。三个写/高危工具的权限声明会被策略引擎直接读到：
//   - update_config：TierWrite，需要审批，支持 dry-run + 二次确认；
//   - restart_service：TierDangerous，需要审批，**非幂等**（禁止自动重试）；
//   - scale_service：TierDangerous，需要审批，幂等（允许一次重试）。
func NewFullRegistry(ds DataSource, wds WriteDataSource) (*Registry, error) {
	r, err := NewDefaultRegistry(ds)
	if err != nil {
		return nil, err
	}
	for _, t := range []Tool{
		newUpdateConfigTool(wds),
		newRestartServiceTool(wds),
		newScaleServiceTool(wds),
	} {
		if err := r.Register(t); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// formatWindow 把 time.Duration 变成人话（5m 而不是 5m0s）。
func formatWindow(d time.Duration) string {
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d >= time.Minute && d%time.Minute == 0:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return d.String()
	}
}

// formatValue 让摘要里的数字不至于露出浮点尾巴（0.1+0.2 那类）。
func formatValue(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return fmt.Sprintf("%.0f", v)
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.3f", v), "0"), ".")
}

// round3 让回喂给模型的数值保持可读：模型不需要看到 0.30000000000000004。
func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
