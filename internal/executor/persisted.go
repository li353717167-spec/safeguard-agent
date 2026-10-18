package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mechrevo/safeguard-agent/internal/store"
	"github.com/mechrevo/safeguard-agent/internal/tools"
)

// CallRef 标识一次工具调用在任务里的位置。幂等 key 由它 + 工具名 + 参数决定。
type CallRef struct {
	TaskID    string
	StepNo    int
	CallIndex int
}

// ToolCallStore 是幂等执行器需要的持久化能力。
// 接口定义在消费方：测试塞一个内存实现即可，不需要数据库。
type ToolCallStore interface {
	BeginIntent(ctx context.Context, tc *store.ToolCall) (*store.ToolCall, bool, error)
	RecordResult(ctx context.Context, idemKey string, res store.ToolCallResult) error
}

// PersistedExecutor = Executor + 三段式落库 + 回放。
//
// 它要回答的是**中间状态**：「执行成功了，但结果没回写怎么办」。
//
//	① 写意图（status=started）—— 此刻崩溃，恢复时能看到"发过、但不知道结果"
//	② 调下游（同一个 key 再次到达是安全的）
//	③ 写结果（success / failed）
//
// 三段落库都完成，"这个操作到底做到哪了"才是一个能用查询回答的问题，而不是靠日志反推。
type PersistedExecutor struct {
	exec  *Executor
	store ToolCallStore
	now   func() time.Time
}

// NewPersisted 构造带落库的执行器。
func NewPersisted(exec *Executor, st ToolCallStore) *PersistedExecutor {
	if exec == nil {
		exec = New(DefaultConfig())
	}
	return &PersistedExecutor{exec: exec, store: st, now: func() time.Time { return time.Now().UTC() }}
}

// Executor 暴露内部执行器（熔断状态查询、测试注入）。
func (p *PersistedExecutor) Executor() *Executor { return p.exec }

// Run 执行一次工具调用，并把三段式落库串起来。
func (p *PersistedExecutor) Run(ctx context.Context, tool tools.Tool, rawArgs json.RawMessage, ref CallRef) Outcome {
	spec := tool.Spec()

	key, err := BuildIdempotencyKey(ref.TaskID, ref.StepNo, ref.CallIndex, spec.Name, rawArgs)
	if err != nil {
		return Outcome{Error: err, Class: ClassBadInput}
	}
	canonical, err := CanonicalJSON(rawArgs)
	if err != nil {
		return Outcome{Error: err, Class: ClassBadInput, IdemKey: key}
	}

	// ① 写意图
	existing, existed, err := p.store.BeginIntent(ctx, &store.ToolCall{
		IdemKey:   key,
		TaskID:    ref.TaskID,
		StepNo:    ref.StepNo,
		CallIndex: ref.CallIndex,
		ToolName:  spec.Name,
		Args:      canonical,
		Tier:      int8(spec.Tier),
		Status:    store.ToolCallStatusStarted,
		StartedAt: p.now(),
	})
	if err != nil {
		return Outcome{Error: err, Class: ClassFatal, IdemKey: key}
	}

	if existed {
		switch existing.Status {
		case store.ToolCallStatusSuccess:
			// 已经成功过：直接回放上次结果，不再打下游。
			// 这就是"同一个逻辑操作重试两次，下游只被真实执行一次"。
			return Outcome{
				Result: tools.Result{
					Data:    decodeStoredResult(existing.Result),
					Summary: fmt.Sprintf("%s（回放：该操作此前已成功执行）", spec.Name),
				},
				Class: ClassOK, IdemKey: key, Replayed: true,
			}

		case store.ToolCallStatusWaitingApproval:
			// 这次调用此前被挂起等审批。**只有拿到"已获批"的信号才允许执行**——
			// 否则一个还没批的高危操作会被"重试"意外地执行掉。
			if !ApprovalGranted(ctx) {
				return Outcome{
					Error:   fmt.Errorf("工具 %s 的这次调用正在等待人工审批，不能直接执行", spec.Name),
					Class:   ClassFatal,
					IdemKey: key,
				}
			}
			// 已获批：继续往下执行（下面 ②③ 会把 waiting_approval 改写成最终结果）。

		case store.ToolCallStatusStarted:
			// 悬挂意图：进程死在 ① 与 ③ 之间（"执行成功但结果回写失败"就是这种）。
			// 用**同一个 key** 再执行一次是安全的——这正是"本地状态表 + 下游幂等"的双保险。
			// 只读工具天然幂等；写工具要靠把 key 透传给下游。
		}
	}

	// ② 调下游。
	//
	// 把幂等 key 注入 ctx 再交给工具：这是"双保险"的第二半——
	// 本地状态表挡住"这一次不许再发"，下游凭同一个 key 挡住"真被重复发了"。
	// 写工具（update_config / scale_service）会要求 ctx 里必须有这个 key，
	// 所以忘了注入的后果不是"少一层保护"，而是写操作直接拒绝执行（fail closed）。
	execCtx := tools.WithIdempotencyKey(ctx, key)
	out := p.exec.Execute(execCtx, tool, rawArgs)
	out.IdemKey = key

	// ③ 写结果
	res := store.ToolCallResult{
		Status:     store.ToolCallStatusSuccess,
		RetryCount: retryCount(out),
		FinishedAt: p.now(),
	}
	if out.Error == nil {
		if payload, marshalErr := json.Marshal(out.Result.Data); marshalErr == nil {
			encoded := string(payload)
			res.Result = &encoded
		}
		// 序列化失败时状态仍算成功（下游确实执行了），只是不留 result。
	} else {
		res.Status = store.ToolCallStatusFailed
		class := toStoreErrClass(out.Class)
		res.ErrClass = &class
		message := out.Error.Error()
		res.ErrMessage = &message
	}

	if err := p.store.RecordResult(ctx, key, res); err != nil {
		// 回写失败不能吞掉：调用方必须知道"执行了但没记下来"。
		if out.Error == nil {
			out.Error = fmt.Errorf("工具已执行成功，但结果回写失败（下次用同一个 key 重试是安全的）: %w", err)
			out.Class = ClassFatal
		} else {
			out.Error = fmt.Errorf("%w；另外结果回写也失败了: %v", out.Error, err)
		}
	}
	return out
}

// retryCount 返回"重试了几次"（不含首次）。
func retryCount(out Outcome) int {
	if n := len(out.Attempts) - 1; n > 0 {
		return n
	}
	return 0
}

// decodeStoredResult 把库里存的 result JSON 解回 any；解不出来就退回原始字符串。
func decodeStoredResult(raw *string) any {
	if raw == nil || *raw == "" {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(*raw), &v); err != nil {
		return *raw
	}
	return v
}

// toStoreErrClass 把 Go 侧 ErrClass 映射成 tool_calls.err_class 的取值。
//
// 两套编码刻意不同：
//   - Go 侧 ClassOK=0（零值即"成功"；忘赋值时不会静默变成"可重试"）；
//   - 数据库侧沿用 0001_init.sql 的注释：0=可重试 1=致命 2=参数错 3=熔断打开。
//
// 因为顺序不同，必须显式映射，不能直接 int8(class)。
func toStoreErrClass(c ErrClass) int8 {
	switch c {
	case ClassRetryable:
		return 0
	case ClassFatal:
		return 1
	case ClassBadInput:
		return 2
	case ClassCircuitOpen:
		return 3
	default:
		return 1 // 未知按致命处理（fail closed）
	}
}
