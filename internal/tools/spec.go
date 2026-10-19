// Package tools 是工具层：工具的定义、注册、参数校验与具体实现。
//
// 三条贯穿全项目的认知，都落在这个包里：
//
//  1. **工具描述本身就是 prompt**。描述写得含糊，模型必然选错工具，
//     而这类问题在代码 review 里几乎看不出来——所以注册时有质量门禁（见 Registry.Register），
//     而不是靠人盯着看。
//  2. **权限是工具的静态属性，不是模型的判断**。Tier / RequireApproval 由工具声明，
//     并且**永远不会出现在给模型看的 schema 里**（见 Registry.Schemas）：
//     模型知道"能做什么"，但不知道也不需要知道"这事要不要审批"。
//  3. **模型给的参数是不可信输入**。强类型解码 + 拒绝未知字段 + 逐工具显式校验，
//     校验发生在即将产生副作用的那一层。
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Tier 是工具的风险等级。它是权限判定的输入，不是给模型看的提示。
type Tier int

const (
	// TierHarmless：只读、无副作用。
	TierHarmless Tier = iota
	// TierWrite：有副作用但可逆（改配置这类）。
	TierWrite
	// TierDangerous：不可逆或影响面大（重启、扩缩容）。
	TierDangerous
)

func (t Tier) String() string {
	switch t {
	case TierHarmless:
		return "harmless"
	case TierWrite:
		return "write"
	case TierDangerous:
		return "dangerous"
	default:
		return "unknown"
	}
}

// Spec 是工具定义。它同时承担三个角色：
// 给模型看的契约（Name/Description/Parameters）、给策略引擎看的权限声明（Tier/RequireApproval*）、
// 给执行器看的执行策略（MaxRetry/Timeout/Idempotent）。
type Spec struct {
	Name        string
	Description string
	// Parameters 是给模型看的 JSON Schema。
	// ⚠️ 它只是"提示"，不是可信输入契约——真正的校验是 Args.Validate。
	Parameters json.RawMessage

	// 权限：由工具配置决定，模型无权干预（D6 的策略引擎只读这里，不看模型输出）。
	Tier            Tier
	RequireApproval bool
	ApprovalRole    string
	ApprovalTTL     time.Duration

	// 执行策略。
	MaxRetry    int
	BackoffBase time.Duration
	Timeout     time.Duration
	Idempotent  bool
	DryRun      bool

	// Preview 是可选钩子：给审批人看"将要发生什么"（人话，不是原始 JSON）。
	// 审批人看到的应当是"timeout_ms: 800 → 1000"，而不是一串参数。
	// 由工具自己实现——它最清楚怎么描述自己的意图，编排层不该去猜参数语义。
	Preview func(ctx context.Context, rawArgs json.RawMessage) (string, error)

	// ApprovalCheck 是可选钩子：按**本次调用的参数**细化审批判定。
	//
	// 典型用法：写工具的 dry-run 预览不需要审批，只有真执行才需要——
	// 否则一次变更会被批两次（预览一次、执行一次）。
	//
	// 它仍然把"参数语义"留在工具里：策略引擎只调用这个钩子，自己不解析参数。
	// ⚠️ 高危工具不接受钩子放宽（见 policy.Engine），避免某个工具不小心把审批关掉。
	ApprovalCheck func(rawArgs json.RawMessage) (require bool, reason string)

	// Audit 是可选钩子：告诉编排层"这次调用审计时怎么写"。
	//
	// 返回两个值：
	//   - target：被操作对象（审计的 target 列），例如 payment-service；
	//   - mutating：这次调用**是否会产生变更**。
	//
	// 为什么要区分 mutating：update_config 的 dry-run 只返回预览，
	// 它不应该被记成一次"变更"。而"会不会真改"只有工具自己知道
	// （取决于 dry_run 参数），所以由工具声明——和 Preview / ApprovalCheck 同一个模式。
	Audit func(rawArgs json.RawMessage) (target string, mutating bool, err error)
}

// Result 是工具执行的规范化输出。
type Result struct {
	// Data 会被序列化成 JSON 回喂给模型。
	Data any
	// Summary 是一行摘要：给人看的，进日志与审计，不进 prompt。
	Summary string
}

// Tool 是注册中心持有的统一形态：定义 + 强类型调用。
type Tool interface {
	Spec() Spec
	// Invoke 接收模型给的**原始**参数 JSON，负责不信任地解码、校验并执行。
	Invoke(ctx context.Context, rawArgs json.RawMessage) (Result, error)
	// Validate 只做解码与参数校验，**不执行任何副作用**。
	//
	// 审批流程会先调用它：参数本身就不合法的调用，不应该生成一张"批了也执行不了"的工单。
	// （否则审批人会看到一张永远不可能成功的单，而模型只能干等——本该立刻被告知参数错误）
	Validate(rawArgs json.RawMessage) error
}

// Args 是工具参数结构体要满足的约束：能自我校验。
//
// 为什么不用 JSON Schema 库做校验：schema 是写给模型看的说明，用它当校验器意味着
// "模型看什么、就信什么"。而 Go 的强类型解码天然拒绝未知字段与类型不符，
// 剩下的业务语义（枚举、范围、存在性）用逐工具显式代码表达，最直白也最好测。
type Args interface {
	Validate() error
}

// ArgError 表示"模型给的参数不合法"。
//
// 它单独成类型是因为处理方式与其他错误完全不同：
// D3 的主循环会把工具返回的错误**回喂给模型**，让它自己修正参数重试；
// 而系统错误（数据库连不上）不该回喂，那只会让模型去"修"一个它修不了的东西。
type ArgError struct{ Msg string }

func (e *ArgError) Error() string { return "参数不合法: " + e.Msg }

func argErrf(format string, args ...any) *ArgError {
	return &ArgError{Msg: fmt.Sprintf(format, args...)}
}

// IsArgError 判断错误是否属于"参数问题"（可回喂模型）。
func IsArgError(err error) bool {
	var target *ArgError
	return errors.As(err, &target)
}

// typedTool 把 Tool 接口与具体参数类型 A 绑在一起：
// 解码与校验只写一次，每个工具只关心自己的业务逻辑。
type typedTool[A Args] struct {
	spec Spec
	run  func(ctx context.Context, args A) (Result, error)
}

// New 构造一个强类型工具。
func New[A Args](spec Spec, run func(ctx context.Context, args A) (Result, error)) Tool {
	return &typedTool[A]{spec: spec, run: run}
}

func (t *typedTool[A]) Spec() Spec { return t.spec }

// Validate 只解码 + 校验，不执行。
func (t *typedTool[A]) Validate(rawArgs json.RawMessage) error {
	args, err := decodeArgs[A](rawArgs)
	if err != nil {
		return err
	}
	return args.Validate()
}

func (t *typedTool[A]) Invoke(ctx context.Context, rawArgs json.RawMessage) (Result, error) {
	args, err := decodeArgs[A](rawArgs)
	if err != nil {
		return Result{}, err
	}
	if err := args.Validate(); err != nil {
		return Result{}, err
	}
	return t.run(ctx, args)
}
