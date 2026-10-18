package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// WriteDataSource 是写工具需要的下游能力。
//
// 刻意与只读的 DataSource 分开：接口隔离——只读工具不该有机会碰写接口，
// 而 D11 的假数据源也不必为了一个测试实现全部写方法。
type WriteDataSource interface {
	// ServiceExists 判断服务是否存在。
	ServiceExists(ctx context.Context, service string) (bool, error)
	// ConfigValue 读当前配置值；不存在时 found=false。
	ConfigValue(ctx context.Context, service, key string) (value string, found bool, err error)
	// ApplyConfig 应用一次配置变更。**同一个 idemKey 只允许生效一次**（下游幂等）。
	ApplyConfig(ctx context.Context, idemKey, service, key, newValue string) (oldValue string, applied bool, err error)
	// SetServiceStatus 改服务健康状态。
	SetServiceStatus(ctx context.Context, service, status string) error
}

// ---------------------------------------------------------------------------
// update_config（写工具，支持 dry-run + 二次确认）
// ---------------------------------------------------------------------------

type updateConfigArgs struct {
	Service string `json:"service"`
	Key     string `json:"key"`
	Value   string `json:"value"`
	// DryRun 用指针：区分"没给"（默认预览）与"明确给了 false"（真执行）。
	DryRun *bool `json:"dry_run"`
	// Confirm 是二次确认令牌：必须等于上一次预览返回的 confirm。
	Confirm string `json:"confirm"`
}

func (a updateConfigArgs) Validate() error {
	if strings.TrimSpace(a.Service) == "" {
		return argErrf("service 不能为空")
	}
	if strings.TrimSpace(a.Key) == "" {
		return argErrf("key 不能为空")
	}
	if strings.TrimSpace(a.Value) == "" {
		return argErrf("value 不能为空")
	}
	return nil
}

// confirmToken 是"将要把哪个服务的哪个键改成什么"的指纹。
//
// 它的作用不是安全（不是密码），而是**防止模型口头确认了 A、实际写了 B**：
// 预览时给它 token，真执行时要求原样回传，不一致就拒绝。
func confirmToken(service, key, value string) string {
	sum := sha256.Sum256([]byte("v1|" + service + "|" + key + "|" + value))
	return hex.EncodeToString(sum[:])[:16]
}

func newUpdateConfigTool(ds WriteDataSource) Tool {
	spec := Spec{
		Name: "update_config",
		Description: "修改某个服务的一项配置。属于**写操作**：默认只返回预览（当前值 → 新值），" +
			"要真正执行必须再调一次并带上 dry_run=false 与预览返回的 confirm 令牌；" +
			"此外这次写操作还会经过人工审批。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "service": {"type": "string", "description": "服务名，例如 payment-service"},
    "key": {"type": "string", "description": "配置项名，例如 timeout_ms"},
    "value": {"type": "string", "description": "新的配置值"},
    "dry_run": {"type": "boolean", "description": "true 或省略 = 只预览；false = 真执行"},
    "confirm": {"type": "string", "description": "真执行时必须原样回传预览里的 confirm 令牌"}
  },
  "required": ["service", "key", "value"]
}`),
		Tier:            TierWrite,
		RequireApproval: true,
		ApprovalRole:    "sre",
		ApprovalTTL:     15 * time.Minute,
		Timeout:         5 * time.Second,
		MaxRetry:        1,
		Idempotent:      true,
		DryRun:          true,
		// 预览不需要审批，只有真执行才需要——否则一次变更会被批两次。
		ApprovalCheck: func(rawArgs json.RawMessage) (bool, string) {
			args, err := decodeArgs[updateConfigArgs](rawArgs)
			if err != nil {
				// 参数都解不出来时保守一点：要求审批（fail closed）。
				return true, "参数无法解析，保守起见要求人工审批"
			}
			if args.DryRun == nil || *args.DryRun {
				return false, ""
			}
			return true, "真正修改配置需要人工审批"
		},
		Preview: func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
			args, err := decodeArgs[updateConfigArgs](rawArgs)
			if err != nil {
				return "", err
			}
			current, found, err := ds.ConfigValue(ctx, args.Service, args.Key)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("修改 %s.%s：%s → %s",
				args.Service, args.Key, displayValue(current, found), args.Value), nil
		},
	}

	return New(spec, func(ctx context.Context, args updateConfigArgs) (Result, error) {
		exists, err := ds.ServiceExists(ctx, args.Service)
		if err != nil {
			return Result{}, err
		}
		if !exists {
			return notFoundResult("service", args.Service, "可用 list_services 获取全部服务名"), nil
		}

		current, found, err := ds.ConfigValue(ctx, args.Service, args.Key)
		if err != nil {
			return Result{}, err
		}
		token := confirmToken(args.Service, args.Key, args.Value)
		dryRun := args.DryRun == nil || *args.DryRun

		if dryRun {
			// 预览：不改任何东西，把"将要发生什么"讲清楚。
			return Result{
				Data: map[string]any{
					"dry_run":          true,
					"service":          args.Service,
					"key":              args.Key,
					"current_value":    current,
					"key_exists":       found,
					"new_value":        args.Value,
					"changed":          !found || current != args.Value,
					"confirm_required": true,
					"confirm":          token,
					"next":             "确认无误后，用同样的参数再调一次并带上 dry_run=false 与 confirm 令牌（该操作还会经过人工审批）",
				},
				Summary: fmt.Sprintf("预览 %s.%s：%s → %s", args.Service, args.Key, displayValue(current, found), args.Value),
			}, nil
		}

		// 真执行：必须先预览过（令牌一致）。
		if args.Confirm != token {
			return Result{}, argErrf("confirm 与当前参数不匹配：请先不带 dry_run=false 预览一次，并把返回的 confirm 原样回传")
		}

		idemKey, ok := IdempotencyKeyFromContext(ctx)
		if !ok {
			return Result{}, fmt.Errorf("tools: 写操作缺少幂等 key（必须经由幂等执行器执行）")
		}

		oldValue, applied, err := ds.ApplyConfig(ctx, idemKey, args.Service, args.Key, args.Value)
		if err != nil {
			return Result{}, err
		}
		return Result{
			Data: map[string]any{
				"dry_run":   false,
				"service":   args.Service,
				"key":       args.Key,
				"old_value": oldValue,
				"new_value": args.Value,
				// applied=false 说明下游按幂等 key 判定"这次变更已经生效过"，没有重复修改。
				"applied":        applied,
				"idempotent_hit": !applied,
			},
			Summary: fmt.Sprintf("已修改 %s.%s：%s → %s", args.Service, args.Key, oldValue, args.Value),
		}, nil
	})
}

// ---------------------------------------------------------------------------
// restart_service（高危）
// ---------------------------------------------------------------------------

type restartServiceArgs struct {
	Service string `json:"service"`
	Reason  string `json:"reason"`
}

func (a restartServiceArgs) Validate() error {
	if strings.TrimSpace(a.Service) == "" {
		return argErrf("service 不能为空")
	}
	return nil
}

func newRestartServiceTool(ds WriteDataSource) Tool {
	spec := Spec{
		Name: "restart_service",
		Description: "重启指定服务。属于**高危操作**：会影响线上流量，必须经过人工审批后才会执行，" +
			"且同一时刻不应重复发起。执行后服务的健康状态会被重置为 healthy。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "service": {"type": "string", "description": "要重启的服务名"},
    "reason": {"type": "string", "description": "为什么需要重启（会写进审批工单，帮助审批人判断）"}
  },
  "required": ["service"]
}`),
		Tier:            TierDangerous,
		RequireApproval: true,
		ApprovalRole:    "sre",
		ApprovalTTL:     15 * time.Minute,
		Timeout:         10 * time.Second,
		// 重启不是幂等操作：重复执行会真的再重启一次。所以 MaxRetry 必须为 0。
		MaxRetry:   0,
		Idempotent: false,
		Preview: func(_ context.Context, rawArgs json.RawMessage) (string, error) {
			args, err := decodeArgs[restartServiceArgs](rawArgs)
			if err != nil {
				return "", err
			}
			if strings.TrimSpace(args.Reason) == "" {
				return fmt.Sprintf("重启 %s（影响线上流量）", args.Service), nil
			}
			return fmt.Sprintf("重启 %s（影响线上流量）。原因：%s", args.Service, args.Reason), nil
		},
	}

	return New(spec, func(ctx context.Context, args restartServiceArgs) (Result, error) {
		exists, err := ds.ServiceExists(ctx, args.Service)
		if err != nil {
			return Result{}, err
		}
		if !exists {
			return notFoundResult("service", args.Service, "可用 list_services 获取全部服务名"), nil
		}
		if err := ds.SetServiceStatus(ctx, args.Service, "healthy"); err != nil {
			return Result{}, err
		}
		return Result{
			Data: map[string]any{
				"service": args.Service,
				"status":  "healthy",
				"reason":  args.Reason,
			},
			Summary: fmt.Sprintf("已重启 %s，健康状态重置为 healthy", args.Service),
		}, nil
	})
}

// ---------------------------------------------------------------------------
// scale_service（高危，但幂等）
// ---------------------------------------------------------------------------

type scaleServiceArgs struct {
	Service  string `json:"service"`
	Replicas int    `json:"replicas"`
}

func (a scaleServiceArgs) Validate() error {
	if strings.TrimSpace(a.Service) == "" {
		return argErrf("service 不能为空")
	}
	if a.Replicas < 1 || a.Replicas > 100 {
		return argErrf("replicas 必须在 [1,100] 之间，收到 %d", a.Replicas)
	}
	return nil
}

func newScaleServiceTool(ds WriteDataSource) Tool {
	spec := Spec{
		Name: "scale_service",
		Description: "把指定服务的副本数调整到 N。属于**高危操作**（涉及成本与容量），必须经过人工审批。" +
			"该操作是幂等的：重复把副本数设成同一个值不会产生额外影响。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "service": {"type": "string", "description": "服务名"},
    "replicas": {"type": "integer", "description": "目标副本数，1-100"}
  },
  "required": ["service", "replicas"]
}`),
		Tier:            TierDangerous,
		RequireApproval: true,
		ApprovalRole:    "sre",
		ApprovalTTL:     15 * time.Minute,
		Timeout:         10 * time.Second,
		MaxRetry:        1,
		// 把副本数设成同一个值是幂等的（不是"加 N 个副本"），所以允许一次重试。
		Idempotent: true,
		Preview: func(_ context.Context, rawArgs json.RawMessage) (string, error) {
			args, err := decodeArgs[scaleServiceArgs](rawArgs)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("把 %s 的副本数调整为 %d（涉及成本与容量）", args.Service, args.Replicas), nil
		},
	}

	return New(spec, func(ctx context.Context, args scaleServiceArgs) (Result, error) {
		exists, err := ds.ServiceExists(ctx, args.Service)
		if err != nil {
			return Result{}, err
		}
		if !exists {
			return notFoundResult("service", args.Service, "可用 list_services 获取全部服务名"), nil
		}

		idemKey, ok := IdempotencyKeyFromContext(ctx)
		if !ok {
			return Result{}, fmt.Errorf("tools: 写操作缺少幂等 key（必须经由幂等执行器执行）")
		}

		oldValue, applied, err := ds.ApplyConfig(ctx, idemKey, args.Service, "replicas", fmt.Sprintf("%d", args.Replicas))
		if err != nil {
			return Result{}, err
		}
		return Result{
			Data: map[string]any{
				"service":        args.Service,
				"replicas":       args.Replicas,
				"previous":       oldValue,
				"applied":        applied,
				"idempotent_hit": !applied,
			},
			Summary: fmt.Sprintf("已把 %s 的副本数从 %s 调整为 %d",
				args.Service, displayValue(oldValue, oldValue != ""), args.Replicas),
		}, nil
	})
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// notFoundResult 把"服务不存在"变成一次成功的工具结果（与只读工具同一套语义）。
func notFoundResult(kind, name, hint string) Result {
	return Result{
		Data: map[string]any{
			"found":  false,
			"kind":   kind,
			"target": name,
			"hint":   hint,
		},
		Summary: fmt.Sprintf("未找到 %s（%s）", name, kind),
	}
}

func displayValue(value string, found bool) string {
	if !found {
		return "(未设置)"
	}
	return value
}
