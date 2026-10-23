package orchestrator

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/mechrevo/safeguard-agent/internal/llm"
)

// ContextBudget 是"发给模型的上下文"的预算。
//
// 为什么需要它：轮次上限（8 轮）只约束**次数**，不约束**体积**。
// 一个 query_metrics 返回 500 个采样点，两轮就能把窗口撑满；
// 上下文一满，真正的失败不是报错，而是模型开始遗忘最早的要求、答非所问。
type ContextBudget struct {
	// MaxTokens 是估算上限。超了先裁剪工具结果、再折叠早期轮次。
	MaxTokens int
	// KeepRecentRounds 是**永不折叠**的最近轮数：最近几轮往往是当前推理的关键上下文。
	KeepRecentRounds int
	// MaxToolResultBytes 是单条工具结果保留的最大字节数，超过就只留预览 + 指向落库原文。
	MaxToolResultBytes int
}

const (
	defaultMaxTokens        = 6000
	defaultKeepRecentRounds = 2
	defaultMaxToolBytes     = 2000
)

// DefaultContextBudget 返回默认预算。
//
// 6000 tokens 的取值考虑：多数 OpenAI 兼容端点窗口 8k~32k，
// 留出约 2k 给系统提示与输出，6000 是"小模型也吃得下"的安全值。
func DefaultContextBudget() ContextBudget {
	return ContextBudget{
		MaxTokens:          defaultMaxTokens,
		KeepRecentRounds:   defaultKeepRecentRounds,
		MaxToolResultBytes: defaultMaxToolBytes,
	}
}

func (b ContextBudget) normalized() ContextBudget {
	if b.MaxTokens <= 0 {
		b.MaxTokens = defaultMaxTokens
	}
	if b.KeepRecentRounds < 0 {
		b.KeepRecentRounds = 0
	}
	if b.MaxToolResultBytes <= 0 {
		b.MaxToolResultBytes = defaultMaxToolBytes
	}
	return b
}

// ContextStats 是一次压缩的结果（给轨迹、日志与评测看）。
type ContextStats struct {
	Tokens       int // 压缩后对整段上下文的估算
	Trimmed      int // 被裁剪的工具结果条数
	FoldedRounds int // 被折叠进摘要的轮数
	Budget       int // 本次使用的预算上限
}

// EstimateTokens 估算一段文本的 token 数。
//
// 不引入分词器（那是几 MB 的依赖，且不同厂商的分词器还不一样），
// 用可比较的近似：ASCII 约 4 字节 1 token，非 ASCII（中文）约 1 字 1 token。
// 这个估算偏保守（宁多算不少算），配合 MaxTokens 的余量使用。
func EstimateTokens(s string) int {
	var ascii, other int
	for _, r := range s {
		if r < utf8.RuneSelf {
			ascii++
		} else {
			other++
		}
	}
	return ascii/4 + other + 1
}

// EstimateMessages 估算整段上下文的 token 数（含每条消息的固定开销）。
func EstimateMessages(msgs []llm.Message) int {
	total := 0
	for _, m := range msgs {
		total += estimateMessage(m)
	}
	return total
}

func estimateMessage(m llm.Message) int {
	total := EstimateTokens(m.Content) + 8
	for _, call := range m.ToolCalls {
		total += EstimateTokens(call.Function.Name) + EstimateTokens(call.Function.Arguments) + 8
	}
	return total
}

// toolPrefixEnd 返回工具结果里"不可信数据"标记行的结束位置（没有标记则返回 0）。
//
// 裁剪时必须保留这行标记：它是间接注入防御的一部分，不能因为省 token 就丢掉。
func toolPrefixEnd(content string) int {
	if !strings.HasPrefix(content, "[工具 ") {
		return 0
	}
	if idx := strings.IndexByte(content, '\n'); idx >= 0 {
		return idx + 1
	}
	return 0
}

// trimToolContent 把过长的工具结果换成"预览 + 指向落库原文"。
//
// 为什么不直接截断字符串：截断后的 JSON 是坏的，模型会试图"修复"它而浪费轮次。
// 所以换成一个**合法 JSON**：明确告诉模型"结果被裁剪了、原文在哪"。
func trimToolContent(content string, limit int) (string, bool) {
	if len(content) <= limit {
		return content, false
	}

	prefixEnd := toolPrefixEnd(content)
	prefix := content[:prefixEnd]
	body := content[prefixEnd:]

	// 预览取约 1/3（至少 200 字节），按 rune 边界切，避免切坏中文。
	previewBytes := limit / 3
	if previewBytes < 200 {
		previewBytes = 200
	}
	if previewBytes > len(body) {
		previewBytes = len(body)
	}
	preview := body[:previewBytes]
	for len(preview) > 0 && !utf8.ValidString(preview) {
		preview = preview[:len(preview)-1]
	}

	replacement := map[string]any{
		"truncated":      true,
		"original_bytes": len(body),
		"preview":        preview,
		"note":           "工具结果过长，已裁剪；完整结果已落库（tool_calls.result）",
	}
	raw, err := json.Marshal(replacement)
	if err != nil {
		return prefix + string([]rune(body)[:min(len([]rune(body)), limit)]), true
	}
	return prefix + string(raw), true
}

// compactMessages 在**发送给模型之前**压缩上下文。
//
// 三个动作，代价从低到高：
//  1. 裁剪过长的工具结果（确定、零成本）；
//  2. 折叠最早的**整轮**（确定、零成本）；
//  3. 还不够就照发并告警——不做"再调一次模型写摘要"：
//     那会多一次调用、结果不确定，而且我们已经用轮次上限兜住了最坏情况。
//
// pinned 是"永不折叠"的前缀长度（system + 用户原始问题）。
// 返回的 out 已去掉被折叠的整轮；summary 需要调用方插在 pinned 之后。
func compactMessages(msgs []llm.Message, pinned int, budget ContextBudget, traces []ToolTrace, foldedBefore int) (out []llm.Message, stats ContextStats, summary string, foldedCalls int) {
	budget = budget.normalized()
	stats.Budget = budget.MaxTokens

	out = make([]llm.Message, len(msgs))
	copy(out, msgs)

	// ① 裁剪过长的工具结果
	for i := pinned; i < len(out); i++ {
		if out[i].Role != llm.RoleTool {
			continue
		}
		if trimmed, changed := trimToolContent(out[i].Content, budget.MaxToolResultBytes); changed {
			out[i].Content = trimmed
			stats.Trimmed++
		}
	}

	if EstimateMessages(out) <= budget.MaxTokens {
		stats.Tokens = EstimateMessages(out)
		return out, stats, "", 0
	}

	// ② 按"整轮"分组：一轮 = 一条 assistant + 其后所有 tool 消息。
	//    必须整轮删除：只删 tool 消息会留下"有 tool_calls 却没有结果"的残缺，
	//    OpenAI 兼容端点会直接拒绝整个请求（协议约束，不是优化）。
	type group struct{ start, end int } // [start, end)
	var groups []group
	for i := pinned; i < len(out); i++ {
		if out[i].Role != llm.RoleAssistant {
			continue
		}
		end := i + 1
		for end < len(out) && out[end].Role == llm.RoleTool {
			end++
		}
		groups = append(groups, group{start: i, end: end})
		i = end - 1
	}

	maxFold := len(groups) - budget.KeepRecentRounds
	if maxFold < 0 {
		maxFold = 0
	}

	// 逐轮试折叠，直到估算落进预算（或已经保留到只剩最近 N 轮）。
	//
	// 注意：候选**每次都要从原始 out 重新构造**。如果边折边覆盖 out，
	// 下一轮再用 groups 的旧下标去切就是越界（group 下标是相对原始切片的）。
	keepFrom := 0
	var folded []llm.Message
	for k := 1; k <= maxFold; k++ {
		candidate := make([]llm.Message, 0, len(out))
		candidate = append(candidate, out[:pinned]...)
		for g := k; g < len(groups); g++ {
			candidate = append(candidate, out[groups[g].start:groups[g].end]...)
		}
		keepFrom = k
		folded = candidate
		if estimateWithSummary(candidate, k) <= budget.MaxTokens {
			break
		}
	}
	if keepFrom > 0 {
		out = folded
	}

	if keepFrom > 0 {
		for g := 0; g < keepFrom; g++ {
			for i := groups[g].start; i < groups[g].end; i++ {
				if msgs[i].Role == llm.RoleTool {
					foldedCalls++
				}
			}
		}
		summary = buildFoldSummary(traces, foldedBefore, foldedCalls)
		stats.FoldedRounds = keepFrom
	}

	stats.Tokens = estimateWithSummary(out, keepFrom)
	return out, stats, summary, foldedCalls
}

// estimateWithSummary 估算"folded 轮被折叠成一条摘要"之后的 token 数。
func estimateWithSummary(msgs []llm.Message, folded int) int {
	if folded == 0 {
		return EstimateMessages(msgs)
	}
	// 摘要消息本身：标题 + 每条调用一行，给个保守的固定开销。
	return EstimateMessages(msgs) + 40 + folded*30
}

// buildFoldSummary 用轨迹里的 Summary 拼出被折叠轮次的一句话进展。
//
// 为什么用轨迹而不是重新解析工具结果：轨迹里的 Summary 是工具**自己**生成的人话摘要，
// 比从 JSON 里再抽一次准确得多，也不必让编排层理解每种结果的形状。
func buildFoldSummary(traces []ToolTrace, from, count int) string {
	if count <= 0 || from >= len(traces) {
		return ""
	}
	end := from + count
	if end > len(traces) {
		end = len(traces)
	}
	lines := make([]string, 0, end-from)
	for _, t := range traces[from:end] {
		lines = append(lines, fmt.Sprintf("- 第%d轮 %s：%s", t.Round, t.ToolName, t.Summary))
	}
	if len(lines) == 0 {
		return ""
	}
	return "[已压缩的早期进展]\n" + strings.Join(lines, "\n")
}

// compactContext 在**发送给模型之前**压缩上下文。
//
// 它直接改写 r.messages（历史本身就该是压缩后的形态）：
// 这样后续每一轮、以及撞上限后的总结调用，用的都是压缩后的上下文。
func (r *run) compactContext(round int) {
	pinned := r.basePinned
	if r.summary != "" {
		pinned++
	}

	msgs, stats, summary, folded := compactMessages(r.messages, pinned, r.o.budget, r.res.Traces, r.foldedCalls)
	r.stats = stats
	if stats.Trimmed == 0 && stats.FoldedRounds == 0 {
		return
	}

	if summary != "" {
		if r.summary == "" {
			r.summary = summary
		} else {
			r.summary += "\n" + strings.TrimPrefix(summary, "[已压缩的早期进展]\n")
		}
		r.foldedCalls += folded

		rebuilt := make([]llm.Message, 0, len(msgs)+1)
		rebuilt = append(rebuilt, r.messages[:r.basePinned]...)
		rebuilt = append(rebuilt, llm.Message{Role: llm.RoleUser, Content: r.summary})
		rebuilt = append(rebuilt, msgs[pinned:]...)
		msgs = rebuilt
	}
	r.messages = msgs

	r.emit(Event{Round: round, Kind: "context", Text: fmt.Sprintf(
		"上下文估算 %d/%d tokens（裁剪 %d 条工具结果、折叠 %d 轮）",
		stats.Tokens, stats.Budget, stats.Trimmed, stats.FoldedRounds)})
}
