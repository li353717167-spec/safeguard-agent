package store

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 本文件提供**可重复执行**的示例服务目录，供只读工具与评测使用。
//
// 为什么数据不放在迁移里：
//   - 迁移只应改结构；把数据写进迁移等于让"有哪些服务"由版本号决定；
//   - service_metrics 是时间序列，演示/评测时"最近 5 分钟"必须永远有数据，
//     所以需要一个能随时重灌的入口（`-seed`）。重灌时时间窗按当前时刻重新生成，
//     而不是固定写死在 SQL 里。
//
// ⚠️ 两条硬规矩：
//  1. 全部数据自造，不含任何真实公司或业务信息；
//  2. **刻意不包含 order-service**：D11 的幻觉陷阱题问的就是一个不存在的服务，
//     如果这里塞了它，那条用例就失去判定依据。改列表前先看
//     TestDemoCatalogHasNoTrapService。
const demoWindowCount = 5

// CatalogMetricNames 是服务指标的全部取值，与 0002_catalog.sql 的
// chk_service_metrics_metric 约束、以及工具层 query_metrics 的枚举必须一致。
// 故意导出：让工具层的参数校验复用它，而不是各写一份然后慢慢漂移。
var CatalogMetricNames = []string{"error_rate", "qps", "p99_latency"}

type demoServiceSpec struct {
	Service Service
	// Metrics: 指标名 → 从旧到新的 demoWindowCount 个采样点。
	// 用固定值而不是随机数：评测的期望结果必须可复现。
	Metrics map[string][]float64
	Configs map[string]string
}

func strPtr(s string) *string { return &s }

var demoCatalog = []demoServiceSpec{
	{
		Service: Service{
			Name: "payment-service", Criticality: ServiceCriticalityCore, Owner: "支付组",
			Status: ServiceStatusHealthy, Description: strPtr("收单与退款主链路"),
		},
		Metrics: map[string][]float64{
			"error_rate":  {0.12, 0.15, 0.11, 0.13, 0.10},
			"qps":         {1820, 1885, 1902, 1870, 1910},
			"p99_latency": {168, 172, 165, 170, 163},
		},
		Configs: map[string]string{
			"timeout_ms": "800", "max_connections": "512", "retry_policy": "exponential_3", "log_level": "info",
		},
	},
	{
		// 刻意造成"错误率与延迟在升高"的一致故事：查指标能得到与 status=degraded 相符的证据
		Service: Service{
			Name: "checkout-service", Criticality: ServiceCriticalityCore, Owner: "交易组",
			Status: ServiceStatusDegraded, Description: strPtr("下单与结算编排"),
		},
		Metrics: map[string][]float64{
			"error_rate":  {0.8, 1.4, 2.1, 2.6, 3.2},
			"qps":         {960, 905, 870, 812, 764},
			"p99_latency": {420, 585, 730, 910, 1180},
		},
		Configs: map[string]string{
			"timeout_ms": "1500", "max_connections": "256", "retry_policy": "exponential_2", "log_level": "warn",
			"degrade_switch": "on",
		},
	},
	{
		Service: Service{
			Name: "gateway-service", Criticality: ServiceCriticalityCore, Owner: "基础架构组",
			Status: ServiceStatusHealthy, Description: strPtr("南北向流量入口"),
		},
		Metrics: map[string][]float64{
			"error_rate":  {0.05, 0.06, 0.04, 0.07, 0.05},
			"qps":         {12400, 12680, 12750, 12510, 12900},
			"p99_latency": {52, 48, 55, 50, 47},
		},
		Configs: map[string]string{
			"timeout_ms": "3000", "max_connections": "4096", "rate_limit_qps": "15000", "log_level": "info",
		},
	},
	{
		Service: Service{
			Name: "user-service", Criticality: ServiceCriticalityImportant, Owner: "用户组",
			Status: ServiceStatusHealthy, Description: strPtr("账号、登录与权限"),
		},
		Metrics: map[string][]float64{
			"error_rate":  {0.18, 0.21, 0.16, 0.19, 0.17},
			"qps":         {2400, 2450, 2380, 2510, 2470},
			"p99_latency": {96, 102, 94, 99, 91},
		},
		Configs: map[string]string{
			"timeout_ms": "600", "max_connections": "1024", "session_ttl_s": "7200", "log_level": "info",
		},
	},
	{
		Service: Service{
			Name: "inventory-service", Criticality: ServiceCriticalityImportant, Owner: "供应链组",
			Status: ServiceStatusHealthy, Description: strPtr("库存扣减与对账"),
		},
		Metrics: map[string][]float64{
			"error_rate":  {0.22, 0.19, 0.25, 0.2, 0.18},
			"qps":         {780, 810, 795, 830, 822},
			"p99_latency": {210, 205, 218, 199, 207},
		},
		Configs: map[string]string{
			"timeout_ms": "1000", "max_connections": "256", "stock_lock_mode": "optimistic", "log_level": "info",
		},
	},
	{
		Service: Service{
			Name: "search-service", Criticality: ServiceCriticalityImportant, Owner: "搜索组",
			Status: ServiceStatusDegraded, Description: strPtr("商品检索与排序"),
		},
		Metrics: map[string][]float64{
			"error_rate":  {0.4, 0.9, 1.6, 1.9, 2.2},
			"qps":         {3100, 3020, 2880, 2790, 2650},
			"p99_latency": {310, 380, 470, 540, 620},
		},
		Configs: map[string]string{
			"timeout_ms": "1200", "max_connections": "512", "index_shard": "8", "log_level": "warn",
		},
	},
	{
		Service: Service{
			Name: "notification-service", Criticality: ServiceCriticalityNormal, Owner: "消息组",
			Status: ServiceStatusHealthy, Description: strPtr("短信、推送与站内信"),
		},
		Metrics: map[string][]float64{
			"error_rate":  {0.3, 0.28, 0.35, 0.31, 0.29},
			"qps":         {520, 515, 540, 528, 533},
			"p99_latency": {142, 138, 150, 145, 140},
		},
		Configs: map[string]string{
			"timeout_ms": "2000", "max_connections": "128", "batch_size": "200", "log_level": "info",
		},
	},
}

// SeedResult 汇报本次灌入的规模，供启动日志与评测记录使用。
type SeedResult struct {
	Services int
	Configs  int
	Metrics  int
	// WindowStart/WindowEnd 是本次生成的采样时间范围，便于判断数据是否"新鲜"。
	WindowStart time.Time
	WindowEnd   time.Time
}

// SeedDemoCatalog 可重复执行地灌入示例服务目录。
//
// 幂等策略：服务与配置用 upsert；指标先按服务删干净再插入。
// 指标必须删除重插——因为时间窗是相对当前时刻生成的，upsert 会把历史采样点越堆越多，
// 而"最近 5 分钟"的语义会被旧数据稀释。
func SeedDemoCatalog(ctx context.Context, gdb *gorm.DB) (SeedResult, error) {
	now := time.Now().UTC().Truncate(time.Minute)
	windows := make([]time.Time, 0, demoWindowCount)
	for i := demoWindowCount - 1; i >= 0; i-- {
		windows = append(windows, now.Add(-time.Duration(i)*time.Minute))
	}

	var res SeedResult
	res.WindowStart, res.WindowEnd = windows[0], windows[len(windows)-1]

	err := gdb.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, spec := range demoCatalog {
			// created_at 不参与更新：首次灌入的时间应当保留。
			if err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "name"}},
				DoUpdates: clause.AssignmentColumns([]string{"criticality", "owner", "status", "description", "updated_at"}),
			}).Create(&spec.Service).Error; err != nil {
				return fmt.Errorf("store: 写入服务 %s 失败: %w", spec.Service.Name, err)
			}
			res.Services++

			for key, value := range spec.Configs {
				cfg := ServiceConfig{Service: spec.Service.Name, ConfigKey: key, ConfigValue: value}
				if err := tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&cfg).Error; err != nil {
					return fmt.Errorf("store: 写入配置 %s.%s 失败: %w", spec.Service.Name, key, err)
				}
				res.Configs++
			}

			if err := tx.Where("service = ?", spec.Service.Name).Delete(&ServiceMetric{}).Error; err != nil {
				return fmt.Errorf("store: 清理 %s 的旧指标失败: %w", spec.Service.Name, err)
			}
			points := make([]ServiceMetric, 0, len(CatalogMetricNames)*demoWindowCount)
			for _, metric := range CatalogMetricNames {
				values, ok := spec.Metrics[metric]
				if !ok || len(values) != demoWindowCount {
					return fmt.Errorf("store: 服务 %s 的指标 %s 采样点数量应为 %d", spec.Service.Name, metric, demoWindowCount)
				}
				for i, v := range values {
					points = append(points, ServiceMetric{
						Service:     spec.Service.Name,
						Metric:      metric,
						WindowStart: windows[i],
						Value:       v,
					})
				}
			}
			if err := tx.Create(&points).Error; err != nil {
				return fmt.Errorf("store: 写入 %s 的指标失败: %w", spec.Service.Name, err)
			}
			res.Metrics += len(points)
		}
		return nil
	})
	if err != nil {
		return SeedResult{}, err
	}
	return res, nil
}
