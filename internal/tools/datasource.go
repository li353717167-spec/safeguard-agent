package tools

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DataSource 是只读工具的数据来源。
//
// 接口定义在**消费方**（本包）而不是实现方，这是 Go 的惯例：
// 工具只声明自己需要什么能力，于是评测（D11）可以塞一个假数据源进来，
// 工具与注册中心的代码一行都不用改。
type DataSource interface {
	ListServices(ctx context.Context) ([]ServiceInfo, error)
	// QueryMetrics 在服务不存在、或该服务在该窗口内没有采样点时返回 *NotFoundError。
	QueryMetrics(ctx context.Context, service, metric string, window time.Duration) (MetricSummary, error)
	// QueryConfig 在服务不存在、或该服务没有登记配置项时返回 *NotFoundError。
	QueryConfig(ctx context.Context, service string) ([]ConfigItem, error)
}

// ServiceInfo 是服务目录的一行。
// Criticality 刻意用中文短语而不是 0/1/2：它会被回喂给模型，让模型去猜数字含义是自找麻烦。
type ServiceInfo struct {
	Name        string
	Criticality string
	Owner       string
	Status      string
	Description string
}

// MetricSummary 是一个服务的某个指标在时间窗内的汇总。
type MetricSummary struct {
	Service     string
	Metric      string
	Unit        string
	SampleCount int
	Latest      float64
	Average     float64
	Max         float64
	LatestAt    time.Time
	Window      time.Duration
}

// ConfigItem 是一条配置项。
type ConfigItem struct {
	Key       string
	Value     string
	UpdatedAt time.Time
}

// NotFoundError 表示"查得到服务，但查不到要的数据"或"服务根本不存在"。
//
// 它被单独成类型，是因为工具层要把它转成一次**成功的**工具调用：
// "这个服务不存在"是查询的正常结果，不是系统故障。若把它当错误抛出，
// 模型看到的就是一个失败信号，它更可能去"猜一个像样的数值"而不是如实回答查不到——
// 这正是 D11 幻觉陷阱题要考的点。
type NotFoundError struct {
	Kind string // service | metric | config
	Name string
	Hint string
}

func (e *NotFoundError) Error() string {
	if e.Hint != "" {
		return fmt.Sprintf("%s %q 不存在或没有数据（%s）", e.Kind, e.Name, e.Hint)
	}
	return fmt.Sprintf("%s %q 不存在或没有数据", e.Kind, e.Name)
}

// IsNotFound 判断错误是否是"查不到"。
func IsNotFound(err error) (nf *NotFoundError, ok bool) {
	ok = errors.As(err, &nf)
	return nf, ok
}
