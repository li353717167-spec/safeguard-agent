package tools

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/mechrevo/safeguard-agent/internal/store"
)

// MySQLDataSource 从 MySQL 读服务目录、配置与指标，并承担写工具的下游。
//
// 这是唯一的数据源实现：工具不自己造数据，也不直接拼 SQL——
// 它们只声明"我要什么"，具体怎么取/怎么改由这里决定（换后端时工具代码零改动）。
type MySQLDataSource struct {
	db      *gorm.DB
	catalog *store.CatalogRepo
}

// NewMySQLDataSource 用 ORM 句柄构造数据源。
func NewMySQLDataSource(db *gorm.DB) *MySQLDataSource {
	return &MySQLDataSource{db: db, catalog: store.NewCatalogRepo(db)}
}

// 以下四个方法实现 WriteDataSource。只做一层转发：SQL 与事务都在 store.CatalogRepo 里，
// 工具层不该出现 SQL——那是"下游"的事。

func (s *MySQLDataSource) ServiceExists(ctx context.Context, service string) (bool, error) {
	var count int64
	if err := s.db.WithContext(ctx).Model(&store.Service{}).Where("name = ?", service).Count(&count).Error; err != nil {
		return false, fmt.Errorf("tools: 校验服务是否存在失败: %w", err)
	}
	return count > 0, nil
}

func (s *MySQLDataSource) ConfigValue(ctx context.Context, service, key string) (string, bool, error) {
	return s.catalog.ConfigValue(ctx, service, key)
}

func (s *MySQLDataSource) ApplyConfig(ctx context.Context, idemKey, service, key, newValue string) (string, bool, error) {
	return s.catalog.ApplyConfig(ctx, idemKey, service, key, newValue)
}

func (s *MySQLDataSource) SetServiceStatus(ctx context.Context, service, status string) error {
	return s.catalog.SetServiceStatus(ctx, service, status)
}

// criticalityText 把整数级别转成人话：这个字段会进 prompt，让模型做数字翻译没有意义。
var criticalityText = map[int8]string{
	store.ServiceCriticalityCore:      "核心",
	store.ServiceCriticalityImportant: "重要",
	store.ServiceCriticalityNormal:    "普通",
}

// metricUnits 是各指标的展示单位，属呈现层关注点，不进数据库。
var metricUnits = map[string]string{
	"error_rate":  "%",
	"qps":         "req/s",
	"p99_latency": "ms",
}

func metricUnit(metric string) string { return metricUnits[metric] }

func (s *MySQLDataSource) ListServices(ctx context.Context) ([]ServiceInfo, error) {
	var rows []store.Service
	// 固定排序：工具输出稳定，评测的期望值才可复现。
	if err := s.db.WithContext(ctx).Order("criticality asc, name asc").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("tools: 查询服务目录失败: %w", err)
	}
	out := make([]ServiceInfo, 0, len(rows))
	for _, r := range rows {
		info := ServiceInfo{
			Name:        r.Name,
			Criticality: criticalityText[r.Criticality],
			Owner:       r.Owner,
			Status:      r.Status,
		}
		if r.Description != nil {
			info.Description = *r.Description
		}
		out = append(out, info)
	}
	return out, nil
}

// ensureService 先确认服务存在。
//
// 这一步不能省：没有它，"服务不存在"和"服务存在但没有数据"会返回同一种空结果，
// 而这两种情况该给模型的回答完全不同。
func (s *MySQLDataSource) ensureService(ctx context.Context, name string) error {
	var count int64
	if err := s.db.WithContext(ctx).Model(&store.Service{}).Where("name = ?", name).Count(&count).Error; err != nil {
		return fmt.Errorf("tools: 校验服务是否存在失败: %w", err)
	}
	if count == 0 {
		return &NotFoundError{Kind: "service", Name: name, Hint: "可用 list_services 获取全部服务名"}
	}
	return nil
}

func (s *MySQLDataSource) QueryMetrics(ctx context.Context, service, metric string, window time.Duration) (MetricSummary, error) {
	if err := s.ensureService(ctx, service); err != nil {
		return MetricSummary{}, err
	}

	since := time.Now().UTC().Add(-window)
	var points []store.ServiceMetric
	if err := s.db.WithContext(ctx).
		Where("service = ? AND metric = ? AND window_start >= ?", service, metric, since).
		Order("window_start DESC").
		Find(&points).Error; err != nil {
		return MetricSummary{}, fmt.Errorf("tools: 查询指标失败: %w", err)
	}
	if len(points) == 0 {
		return MetricSummary{}, &NotFoundError{
			Kind: "metric",
			Name: service + "." + metric,
			Hint: fmt.Sprintf("该服务在最近 %s 内没有 %s 的采样点", window, metric),
		}
	}

	// points 已按时间倒序：第一个就是最新采样点。
	summary := MetricSummary{
		Service:     service,
		Metric:      metric,
		Unit:        metricUnit(metric),
		SampleCount: len(points),
		Latest:      points[0].Value,
		LatestAt:    points[0].WindowStart,
		Window:      window,
	}
	var total, maxValue float64
	for i, p := range points {
		total += p.Value
		if i == 0 || p.Value > maxValue {
			maxValue = p.Value
		}
	}
	summary.Average = total / float64(len(points))
	summary.Max = maxValue
	return summary, nil
}

func (s *MySQLDataSource) QueryConfig(ctx context.Context, service string) ([]ConfigItem, error) {
	if err := s.ensureService(ctx, service); err != nil {
		return nil, err
	}

	var rows []store.ServiceConfig
	if err := s.db.WithContext(ctx).
		Where("service = ?", service).
		Order("config_key asc").
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("tools: 查询服务配置失败: %w", err)
	}
	if len(rows) == 0 {
		return nil, &NotFoundError{Kind: "config", Name: service, Hint: "该服务没有登记配置项"}
	}

	out := make([]ConfigItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, ConfigItem{Key: r.ConfigKey, Value: r.ConfigValue, UpdatedAt: r.UpdatedAt})
	}
	return out, nil
}
