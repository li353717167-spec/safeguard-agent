package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mechrevo/safeguard-agent/internal/contextplan"
	"github.com/mechrevo/safeguard-agent/internal/llm"
	"github.com/mechrevo/safeguard-agent/internal/store"
)

// ContextBudget 是"发给模型的上下文"的预算。
//
// 注意：具体怎么压由 Strategy 决定，这里只描述"多大算大"。
// 把"预算"与"策略"分开，是因为它们的变化频率完全不同：
// 预算跟着模型窗口走，策略跟着场景走（有人想保留最近几轮全量，有人想按信息密度挑）。
type ContextBudget struct {
	MaxTokens          int
	KeepRecentRounds   int
	MaxToolResultBytes int
}

// DefaultContextBudget 返回默认预算。
func DefaultContextBudget() ContextBudget {
	return ContextBudget{MaxTokens: 6000, KeepRecentRounds: 2, MaxToolResultBytes: 2000}
}

func (b ContextBudget) normalized() ContextBudget {
	if b.MaxTokens <= 0 {
		b.MaxTokens = 6000
	}
	if b.KeepRecentRounds < 0 {
		b.KeepRecentRounds = 0
	}
	if b.MaxToolResultBytes <= 0 {
		b.MaxToolResultBytes = 2000
	}
	return b
}

func (b ContextBudget) planBudget() contextplan.Budget {
	n := b.normalized()
	return contextplan.Budget{
		MaxTokens:          n.MaxTokens,
		KeepRecentRounds:   n.KeepRecentRounds,
		MaxToolResultBytes: n.MaxToolResultBytes,
	}
}

// ContextStats 是一次压缩的结果（给轨迹、日志与评测看）。
type ContextStats struct {
	Tokens       int
	Trimmed      int
	Deduped      int
	FoldedRounds int
	Budget       int
}

// ContextArchive 是编排层需要的归档能力（consumer-side 接口）。
type ContextArchive interface {
	Save(ctx context.Context, entries []store.ContextArchiveEntry) error
}

// compactContext 在**发送给模型之前**压缩上下文。
//
// 流程：策略算 → 归档原文 → 重建消息（摘要插在前缀之后）。
// 压缩直接改写 r.messages，所以后续每一轮和"撞上限后的总结调用"用的都是压缩后的形态。
func (r *run) compactContext(round int) {
	if r.o.strategy == nil {
		return
	}

	pinned := r.basePinned
	if r.summary != "" {
		pinned++
	}

	traces := make([]contextplan.Trace, 0, len(r.res.Traces))
	for _, t := range r.res.Traces {
		traces = append(traces, contextplan.Trace{
			Round: t.Round, ToolName: t.ToolName, Summary: t.Summary,
			Failed: t.Error != "" || t.Rejected,
		})
	}

	out, err := r.o.strategy.Compact(contextplan.Input{
		TaskID:   r.taskID,
		Messages: r.messages,
		Pinned:   pinned,
		Budget:   r.o.budget.planBudget(),
		Traces:   traces,
	})
	if err != nil {
		// 压缩失败不能拖垮任务：继续用未压缩的上下文，只是可能超预算。
		slog.Warn("上下文压缩失败，继续使用原上下文", "task_id", r.taskID, "strategy", r.o.strategy.Name(), "err", err)
		return
	}

	r.stats = ContextStats{
		Tokens:       out.Stats.Tokens,
		Trimmed:      out.Stats.Trimmed,
		Deduped:      out.Stats.Deduped,
		FoldedRounds: out.Stats.FoldedRounds,
		Budget:       out.Stats.Budget,
	}
	if out.Stats.Trimmed == 0 && out.Stats.Deduped == 0 && out.Stats.FoldedRounds == 0 {
		return
	}

	// 先把原文归档：摘要里的 ref 能不能取回，取决于这一步。
	// 归档失败只降级"可回查"，不影响压缩本身（宁可少一个能力，不要卡住任务）。
	if r.o.archive != nil && len(out.Archived) > 0 {
		entries := make([]store.ContextArchiveEntry, 0, len(out.Archived))
		for _, a := range out.Archived {
			entries = append(entries, store.ContextArchiveEntry{
				Ref: a.Ref, TaskID: r.taskID, Round: a.Round,
				Role: a.Role, ToolName: a.ToolName, Content: a.Content, Tokens: a.Tokens,
			})
		}
		if err := r.o.archive.Save(context.Background(), entries); err != nil {
			slog.Warn("上下文归档失败（回查能力降级）", "task_id", r.taskID, "entries", len(entries), "err", err)
		}
	}

	if out.Summary != "" {
		if r.summary == "" {
			r.summary = out.Summary
		} else {
			r.summary += "\n" + strings.TrimPrefix(out.Summary, "[已压缩的早期进展]\n")
		}
		rebuilt := make([]llm.Message, 0, len(out.Messages)+1)
		rebuilt = append(rebuilt, r.messages[:r.basePinned]...)
		rebuilt = append(rebuilt, llm.Message{Role: llm.RoleUser, Content: r.summary})
		rebuilt = append(rebuilt, out.Messages[pinned:]...)
		r.messages = rebuilt
	} else {
		r.messages = out.Messages
	}

	r.emit(Event{Round: round, Kind: "context", Text: fmt.Sprintf(
		"上下文估算 %d/%d tokens（裁剪 %d 条、去重 %d 条、折叠 %d 轮｜策略 %s）",
		r.stats.Tokens, r.stats.Budget, r.stats.Trimmed, r.stats.Deduped, r.stats.FoldedRounds, r.o.strategy.Name())})
}
