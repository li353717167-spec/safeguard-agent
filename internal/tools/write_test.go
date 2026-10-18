package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// fakeWriteSource 是内存版写数据源。
type fakeWriteSource struct {
	services map[string]bool
	configs  map[string]string
	statuses map[string]string

	appliedKeys []string
	// nextApplied 为 false 时，下一次 ApplyConfig 返回 applied=false（模拟下游幂等命中）。
	nextApplied bool
}

func newFakeWriteSource() *fakeWriteSource {
	return &fakeWriteSource{
		services:    map[string]bool{"payment-service": true},
		configs:     map[string]string{"payment-service|timeout_ms": "800"},
		statuses:    map[string]string{"payment-service": "degraded"},
		nextApplied: true,
	}
}

func (f *fakeWriteSource) ServiceExists(_ context.Context, service string) (bool, error) {
	return f.services[service], nil
}

func (f *fakeWriteSource) ConfigValue(_ context.Context, service, key string) (string, bool, error) {
	v, ok := f.configs[service+"|"+key]
	return v, ok, nil
}

func (f *fakeWriteSource) ApplyConfig(_ context.Context, idemKey, service, key, newValue string) (string, bool, error) {
	f.appliedKeys = append(f.appliedKeys, idemKey)
	old := f.configs[service+"|"+key]
	if !f.nextApplied {
		return old, false, nil
	}
	f.configs[service+"|"+key] = newValue
	return old, true, nil
}

func (f *fakeWriteSource) SetServiceStatus(_ context.Context, service, status string) error {
	f.statuses[service] = status
	return nil
}

func invokeWrite(t *testing.T, tool Tool, args string) (Result, error) {
	t.Helper()
	return tool.Invoke(context.Background(), json.RawMessage(args))
}

func dataMap(t *testing.T, res Result) map[string]any {
	t.Helper()
	m, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("Data 期望 map, 实际 %T", res.Data)
	}
	return m
}

// update_config 默认只预览：绝不能碰下游。
func TestUpdateConfigDefaultsToDryRun(t *testing.T) {
	ds := newFakeWriteSource()
	tool := newUpdateConfigTool(ds)

	res, err := invokeWrite(t, tool, `{"service":"payment-service","key":"timeout_ms","value":"1500"}`)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	data := dataMap(t, res)
	if data["dry_run"] != true {
		t.Errorf("默认应当是预览，实际 dry_run=%v", data["dry_run"])
	}
	if data["current_value"] != "800" {
		t.Errorf("预览应带当前值，实际 %v", data["current_value"])
	}
	if data["changed"] != true {
		t.Errorf("值变了 should changed=true, 实际 %v", data["changed"])
	}
	if data["confirm"] == "" || data["confirm"] == nil {
		t.Error("预览必须返回 confirm 令牌")
	}
	if len(ds.appliedKeys) != 0 {
		t.Errorf("预览阶段不该写下游，实际调用了 %d 次", len(ds.appliedKeys))
	}
}

// 真执行必须带对令牌——这是"二次确认"的落地方式（不是靠提示词约束模型）。
func TestUpdateConfigRequiresConfirmToken(t *testing.T) {
	ds := newFakeWriteSource()
	tool := newUpdateConfigTool(ds)

	_, err := invokeWrite(t, tool, `{"service":"payment-service","key":"timeout_ms","value":"1500","dry_run":false}`)
	if err == nil {
		t.Fatal("没有 confirm 令牌时必须拒绝执行")
	}
	if !IsArgError(err) {
		t.Errorf("应是参数错误（可回喂模型自我修正），实际 %T", err)
	}
	if !strings.Contains(err.Error(), "confirm") {
		t.Errorf("错误信息要提到 confirm: %v", err)
	}
	if len(ds.appliedKeys) != 0 {
		t.Error("校验失败时绝不能写下游")
	}
}

// 写操作必须经由幂等执行器：ctx 里没有幂等 key 就拒绝。
func TestWriteToolsRequireIdempotencyKey(t *testing.T) {
	ds := newFakeWriteSource()
	tool := newUpdateConfigTool(ds)

	token := confirmToken("payment-service", "timeout_ms", "1500")
	args := `{"service":"payment-service","key":"timeout_ms","value":"1500","dry_run":false,"confirm":"` + token + `"}`

	_, err := invokeWrite(t, tool, args)
	if err == nil {
		t.Fatal("缺少幂等 key 时必须拒绝写操作")
	}
	if !strings.Contains(err.Error(), "幂等") {
		t.Errorf("错误信息要说明缺少幂等 key: %v", err)
	}
}

func TestUpdateConfigAppliesWithIdempotencyKey(t *testing.T) {
	ds := newFakeWriteSource()
	tool := newUpdateConfigTool(ds)

	token := confirmToken("payment-service", "timeout_ms", "1500")
	args := `{"service":"payment-service","key":"timeout_ms","value":"1500","dry_run":false,"confirm":"` + token + `"}`

	ctx := WithIdempotencyKey(context.Background(), "0123456789abcdef0123456789abcdef")
	res, err := tool.Invoke(ctx, json.RawMessage(args))
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}

	data := dataMap(t, res)
	if data["dry_run"] != false {
		t.Errorf("dry_run 应为 false，实际 %v", data["dry_run"])
	}
	if data["applied"] != true {
		t.Errorf("首次应用应当 applied=true，实际 %v", data["applied"])
	}
	if data["old_value"] != "800" || data["new_value"] != "1500" {
		t.Errorf("变更前后值不正确: %v -> %v", data["old_value"], data["new_value"])
	}
	if len(ds.appliedKeys) != 1 || ds.appliedKeys[0] != "0123456789abcdef0123456789abcdef" {
		t.Errorf("幂等 key 未透传给下游: %v", ds.appliedKeys)
	}
	if ds.configs["payment-service|timeout_ms"] != "1500" {
		t.Errorf("下游配置未更新: %v", ds.configs)
	}
}

// 下游按幂等 key 判定"已经生效过"时，必须如实告诉模型（而不是再改一遍）。
func TestUpdateConfigReportsIdempotentHit(t *testing.T) {
	ds := newFakeWriteSource()
	ds.nextApplied = false
	tool := newUpdateConfigTool(ds)

	token := confirmToken("payment-service", "timeout_ms", "1500")
	args := `{"service":"payment-service","key":"timeout_ms","value":"1500","dry_run":false,"confirm":"` + token + `"}`
	ctx := WithIdempotencyKey(context.Background(), "k1")

	res, err := tool.Invoke(ctx, json.RawMessage(args))
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	data := dataMap(t, res)
	if data["applied"] != false || data["idempotent_hit"] != true {
		t.Errorf("应报告幂等命中，实际 applied=%v hit=%v", data["applied"], data["idempotent_hit"])
	}
}

func TestUpdateConfigUnknownServiceIsNotFound(t *testing.T) {
	ds := newFakeWriteSource()
	tool := newUpdateConfigTool(ds)

	res, err := invokeWrite(t, tool, `{"service":"order-service","key":"timeout_ms","value":"1500"}`)
	if err != nil {
		t.Fatalf("服务不存在应是成功的工具结果，而不是错误: %v", err)
	}
	data := dataMap(t, res)
	if data["found"] != false {
		t.Errorf("应返回 found=false，实际 %v", data["found"])
	}
}

func TestWriteToolSpecsAreCorrect(t *testing.T) {
	ds := newFakeWriteSource()

	update := newUpdateConfigTool(ds).Spec()
	if update.Tier != TierWrite || !update.RequireApproval || !update.DryRun {
		t.Errorf("update_config 的等级/审批/dry-run 声明不对: %+v", update)
	}
	if update.Preview == nil {
		t.Error("update_config 必须提供 Preview：审批人要看懂将要改什么")
	}

	restart := newRestartServiceTool(ds).Spec()
	if restart.Tier != TierDangerous || !restart.RequireApproval {
		t.Errorf("restart_service 必须是高危且需审批: %+v", restart)
	}
	if restart.Idempotent || restart.MaxRetry != 0 {
		t.Error("重启不是幂等操作，必须禁止自动重试")
	}

	scale := newScaleServiceTool(ds).Spec()
	if scale.Tier != TierDangerous || !scale.RequireApproval {
		t.Errorf("scale_service 必须是高危且需审批: %+v", scale)
	}
	if !scale.Idempotent {
		t.Error("把副本数设成同一个值是幂等的，应允许一次重试")
	}
}

func TestRestartServiceSetsHealthy(t *testing.T) {
	ds := newFakeWriteSource()
	tool := newRestartServiceTool(ds)

	res, err := invokeWrite(t, tool, `{"service":"payment-service","reason":"延迟升高"}`)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if dataMap(t, res)["status"] != "healthy" {
		t.Errorf("重启后应把状态置为 healthy")
	}
	if ds.statuses["payment-service"] != "healthy" {
		t.Errorf("下游状态未更新: %v", ds.statuses)
	}
}

func TestScaleServiceValidatesReplicas(t *testing.T) {
	ds := newFakeWriteSource()
	tool := newScaleServiceTool(ds)

	for _, bad := range []string{`{"service":"payment-service","replicas":0}`, `{"service":"payment-service","replicas":101}`} {
		if _, err := invokeWrite(t, tool, bad); err == nil || !IsArgError(err) {
			t.Errorf("replicas 越界应当报参数错误: %s -> %v", bad, err)
		}
	}

	ctx := WithIdempotencyKey(context.Background(), "k1")
	res, err := tool.Invoke(ctx, json.RawMessage(`{"service":"payment-service","replicas":6}`))
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if dataMap(t, res)["replicas"] != 6 {
		t.Errorf("副本数未生效: %v", res.Data)
	}
	if ds.configs["payment-service|replicas"] != "6" {
		t.Errorf("下游 replicas 配置未更新: %v", ds.configs)
	}
}
