// Package contextplan 负责"发送给模型之前，上下文怎么压"。
//
// 它被抽成独立包，是因为压缩策略天然是**可替换**的：
// 有人想保留最近几轮全量、有人想按信息密度挑、有人想直接丢最老的。
// 编排层只依赖 Strategy 接口，具体策略通过名字（配置）或代码注册（插件）注入。
//
// 三种内置策略：
//
//	name=trim-fold  裁剪超长工具结果 + 折叠最老的整轮（最早的行为，保持兼容）
//	name=tiered     裁剪 + 去重 + 分层（最近全量 / 中间只留摘要 / 更早折叠成总结）
//	name=none       不压缩（只保留预算检查，用于对照实验）
//
// 扩展方式见 Register；配置式扩展见 Config 与 LoadConfig（JSON 策略文件）。
package contextplan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/mechrevo/safeguard-agent/internal/llm"
)

// Budget 是一次压缩的预算。
type Budget struct {
	MaxTokens          int
	KeepRecentRounds   int
	MaxToolResultBytes int
}

// DefaultBudget 返回默认预算。
func DefaultBudget() Budget {
	return Budget{MaxTokens: 6000, KeepRecentRounds: 2, MaxToolResultBytes: 2000}
}

// Normalize 补齐零值。
func (b Budget) Normalize() Budget {
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

// Trace 是压缩时需要参考的一次工具调用摘要（来自编排层的轨迹）。
type Trace struct {
	Round    int
	ToolName string
	Summary  string
	Failed   bool
}

// Input 是一次压缩的输入。
type Input struct {
	TaskID string
	// Messages 是完整历史（含 system/user 前缀）。
	Messages []llm.Message
	// Pinned 是永不折叠的前缀长度（system + 用户原始问题）。
	Pinned int
	Budget Budget
	Traces []Trace
}

// Archived 是一条被压缩掉的原始内容（供上层归档、之后回查）。
type Archived struct {
	Ref      string
	Round    int
	Role     string
	ToolName string
	Content  string
	Tokens   int
}

// Stats 是压缩统计。
type Stats struct {
	Tokens       int
	Trimmed      int
	Deduped      int
	FoldedRounds int
	Budget       int
}

// Output 是一次压缩的结果。
type Output struct {
	Messages []llm.Message
	// Summary 需要插在 Pinned 之后（为空表示没有折叠）。
	Summary string
	// Archived 是被移除/降级/裁剪的原始内容；上层负责落库。
	Archived []Archived
	Stats    Stats
}

// Strategy 是可替换的压缩策略。
//
// 实现约定：
//   - 不得丢弃 pinned 前缀；
//   - 不得破坏 "assistant.tool_calls ↔ tool 结果" 的配对（协议约束）；
//   - 返回的 Messages 必须是新切片，不改入参。
type Strategy interface {
	Name() string
	Compact(in Input) (Output, error)
}

// Factory 构造策略（插件注册点）。
type Factory func(cfg Config) (Strategy, error)

var registry = map[string]Factory{}

// Register 注册一个策略，供配置按名字选择。
//
// 这就是"插件入口"：使用方（或二次开发者）可以在自己的 main 里注册自定义策略：
//
//	contextplan.Register("mine", func(cfg contextplan.Config) (contextplan.Strategy, error) {
//	    return myStrategy{}, nil
//	})
//
// 为什么不支持 Go 的 plugin 动态库：plugin 要求编译环境完全一致、平台受限，
// 对"配置一个压缩策略"这种小事来说代价过大。编译期注册 + 配置选名字，已经够用。
func Register(name string, f Factory) {
	registry[name] = f
}

// Names 返回已注册的策略名（排好序，便于 CLI 展示）。
func Names() []string {
	out := make([]string, 0, len(registry))
	for name := range registry {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// New 按名字构造策略。
func New(name string, cfg Config) (Strategy, error) {
	if name == "" {
		name = "tiered"
	}
	f, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("未知的压缩策略 %q（可用：%s）", name, strings.Join(Names(), ", "))
	}
	return f(cfg)
}

func init() {
	Register("trim-fold", func(cfg Config) (Strategy, error) {
		off := false
		cfg.Dedupe = &off
		cfg.DetailRounds = 0
		cfg.KeepFailures = &off
		return &planner{mode: "trim-fold", cfg: cfg}, nil
	})
	Register("tiered", func(cfg Config) (Strategy, error) {
		return &planner{mode: "tiered", cfg: cfg}, nil
	})
	Register("none", func(cfg Config) (Strategy, error) {
		return &planner{mode: "none", cfg: cfg}, nil
	})
}

// Config 是策略的可配置参数（对应 JSON 策略文件）。
//
// 一个最小策略文件：
//
//	{
//	  "strategy": "tiered",
//	  "max_tokens": 6000,
//	  "keep_recent_rounds": 2,
//	  "detail_rounds": 2,
//	  "max_tool_bytes": 2000,
//	  "dedupe": true,
//	  "keep_failures": true
//	}
type Config struct {
	Strategy         string `json:"strategy"`
	MaxTokens        int    `json:"max_tokens"`
	KeepRecentRounds int    `json:"keep_recent_rounds"`
	DetailRounds     int    `json:"detail_rounds"`
	MaxToolBytes     int    `json:"max_tool_bytes"`
	// 指针类型：用来区分"配置里没写"与"显式写了 false"。
	Dedupe       *bool `json:"dedupe"`
	KeepFailures *bool `json:"keep_failures"`
}

// DedupeOn 返回是否开启去重（默认开）。
func (c Config) DedupeOn() bool { return c.Dedupe == nil || *c.Dedupe }

// KeepFailuresOn 返回是否"失败调用不丢"（默认开）。
func (c Config) KeepFailuresOn() bool { return c.KeepFailures == nil || *c.KeepFailures }

// WithDefaults 补齐缺省值。
func (c Config) WithDefaults() Config {
	if c.Strategy == "" {
		c.Strategy = "tiered"
	}
	if c.MaxTokens <= 0 {
		c.MaxTokens = 6000
	}
	if c.KeepRecentRounds <= 0 {
		c.KeepRecentRounds = 2
	}
	if c.DetailRounds < 0 {
		c.DetailRounds = 0
	}
	if c.MaxToolBytes <= 0 {
		c.MaxToolBytes = 2000
	}
	// 去重与"失败不丢"的默认值由 DedupeOn / KeepFailuresOn 决定（指针为 nil 即默认开）。
	return c
}

// LoadConfig 从 JSON 文件读策略配置。
func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("读取压缩策略失败: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("解析压缩策略失败（%s）: %w", path, err)
	}
	return cfg, nil
}

// EstimateTokens 估算一段文本的 token 数（ASCII 约 4 字节 1 token，中文约 1 字 1 token）。
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

// Estimate 估算整段上下文的 token 数。
func Estimate(msgs []llm.Message) int {
	total := 0
	for _, m := range msgs {
		total += EstimateTokens(m.Content) + 8
		for _, call := range m.ToolCalls {
			total += EstimateTokens(call.Function.Name) + EstimateTokens(call.Function.Arguments) + 8
		}
	}
	return total
}

// Ref 生成一条归档记录的短 id（确定性：同样的任务/轮次/内容得到同样的 ref）。
func Ref(taskID string, round int, idx int, content string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d|%s", taskID, round, idx, content)))
	return "ctx_" + hex.EncodeToString(sum[:4])
}

// TrimNote 是裁剪后的替换载荷说明。
const TrimNote = "工具结果过长，已裁剪；原文已归档，可用 recall_context 按 ref 取回"

// trimToolContent 把过长的工具结果换成"预览 + 指向归档"。
func trimToolContent(content string, limit int) (string, bool) {
	if len(content) <= limit {
		return content, false
	}
	prefixEnd := 0
	if strings.HasPrefix(content, "[工具 ") {
		if idx := strings.IndexByte(content, '\n'); idx >= 0 {
			prefixEnd = idx + 1
		}
	}
	prefix := content[:prefixEnd]
	body := content[prefixEnd:]

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
	payload := map[string]any{
		"truncated":      true,
		"original_bytes": len(body),
		"preview":        preview,
		"note":           TrimNote,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return prefix + string([]rune(body)[:min(len([]rune(body)), limit)]), true
	}
	return prefix + string(raw), true
}

// planner 是内置策略的统一实现：mode 决定启用哪些步骤。
type planner struct {
	mode string
	cfg  Config
}

func (p *planner) Name() string { return p.mode }

func (p *planner) Compact(in Input) (Output, error) {
	budget := in.Budget.Normalize()
	cfg := p.cfg.WithDefaults()
	if cfg.MaxToolBytes > 0 {
		budget.MaxToolResultBytes = cfg.MaxToolBytes
	}

	out := make([]llm.Message, len(in.Messages))
	copy(out, in.Messages)

	stats := Stats{Budget: budget.MaxTokens}
	var archived []Archived

	if p.mode == "none" {
		stats.Tokens = Estimate(out)
		return Output{Messages: out, Stats: stats}, nil
	}

	// callID → 调用（去重要用参数）
	calls := map[string]llm.ToolCall{}
	for _, m := range out {
		if m.Role != llm.RoleAssistant {
			continue
		}
		for _, c := range m.ToolCalls {
			calls[c.ID] = c
		}
	}

	// ① 裁剪超长工具结果
	for i := in.Pinned; i < len(out); i++ {
		if out[i].Role != llm.RoleTool {
			continue
		}
		original := out[i].Content
		trimmed, changed := trimToolContent(original, budget.MaxToolResultBytes)
		if !changed {
			continue
		}
		out[i].Content = trimmed
		stats.Trimmed++
		archived = append(archived, Archived{
			Ref:   Ref(in.TaskID, roundOf(in, i), i, original),
			Round: roundOf(in, i), Role: string(out[i].Role),
			ToolName: out[i].Name, Content: original,
			Tokens: EstimateTokens(original),
		})
	}

	// ② 去重：同一工具 + 同一参数只保留最后一次
	if cfg.DedupeOn() {
		lastIndex := map[string]int{}
		for i := in.Pinned; i < len(out); i++ {
			if out[i].Role != llm.RoleTool {
				continue
			}
			if key := dedupeKey(out[i], calls); key != "" {
				lastIndex[key] = i
			}
		}
		for i := in.Pinned; i < len(out); i++ {
			if out[i].Role != llm.RoleTool {
				continue
			}
			key := dedupeKey(out[i], calls)
			if key == "" || lastIndex[key] == i {
				continue
			}
			original := out[i].Content
			ref := Ref(in.TaskID, roundOf(in, i), i, original)
			archived = append(archived, Archived{
				Ref: ref, Round: roundOf(in, i), Role: string(out[i].Role),
				ToolName: out[i].Name, Content: original, Tokens: EstimateTokens(original),
			})
			payload, _ := json.Marshal(map[string]any{
				"deduped": true,
				"ref":     ref,
				"note":    "同一查询的重复结果，已省略；需要细节可用 recall_context 按 ref 取回",
			})
			out[i].Content = string(payload)
			stats.Deduped++
		}
	}

	if Estimate(out) <= budget.MaxTokens {
		stats.Tokens = Estimate(out)
		return Output{Messages: out, Stats: stats, Archived: archived}, nil
	}

	// ③ 分层 / 折叠
	gs := groupMessages(out, in.Pinned, in.Traces)

	recent := min(budget.KeepRecentRounds, len(gs))
	keep := map[int]bool{} // 强制保留的组（失败调用）
	if cfg.KeepFailuresOn() {
		for _, g := range gs {
			if g.failures > 0 {
				keep[g.index] = true
			}
		}
	}

	foldFrom := 0
	detailFrom := len(gs) - recent
	if p.mode == "tiered" {
		detailFrom = max(0, len(gs)-recent-cfg.DetailRounds)
	}

	// 中间档：只保留"工具名 + 一行摘要"
	for i := detailFrom; i < len(gs)-recent; i++ {
		if keep[i] {
			continue
		}
		for mi := gs[i].start; mi < gs[i].end; mi++ {
			if out[mi].Role != llm.RoleTool {
				continue
			}
			original := out[mi].Content
			ref := Ref(in.TaskID, gs[i].round, mi, original)
			archived = append(archived, Archived{
				Ref: ref, Round: gs[i].round, Role: string(out[mi].Role),
				ToolName: out[mi].Name, Content: original, Tokens: EstimateTokens(original),
			})
			out[mi].Content = condensed(out[mi], gs[i].lineFor(out[mi].ToolCallID), ref)
		}
	}

	// 折叠是**按集合**而不是按前缀：失败轮要保住，所以允许"跳过它、折掉后面的别的轮"。
	// 代价是要反复估算（轮数上限只有个位数，O(n²) 无所谓），收益是保留策略不会被排队顺序绑架。
	folded := map[int]bool{}
	build := func() []llm.Message {
		kept := make([]llm.Message, 0, len(out))
		kept = append(kept, out[:in.Pinned]...)
		for i := range gs {
			if folded[i] {
				continue
			}
			kept = append(kept, out[gs[i].start:gs[i].end]...)
		}
		return kept
	}

	for i := 0; i < detailFrom; i++ {
		if keep[i] {
			continue // 失败的调用信息量最大，默认不折
		}
		folded[i] = true
		if estimateWithSummary(build(), len(folded)) <= budget.MaxTokens {
			break
		}
	}
	kept := build()
	foldFrom = len(folded)

	var lines []string
	for i := range gs {
		if !folded[i] {
			continue
		}
		var refs []string
		for mi := gs[i].start; mi < gs[i].end; mi++ {
			original := out[mi].Content
			ref := Ref(in.TaskID, gs[i].round, mi, original)
			if out[mi].Role == llm.RoleTool {
				archived = append(archived, Archived{
					Ref: ref, Round: gs[i].round, Role: string(out[mi].Role),
					ToolName: out[mi].Name, Content: original, Tokens: EstimateTokens(original),
				})
			}
			refs = append(refs, ref)
		}
		// 摘要行要带上 ref，模型才有办法用 recall_context 取回细节。
		gs[i].refs = refs
		lines = append(lines, gs[i].summaryLine())
	}
	stats.FoldedRounds = foldFrom

	summary := ""
	if len(lines) > 0 {
		summary = "[已压缩的早期进展]\n" + strings.Join(lines, "\n") +
			"\n（原文已归档：需要细节时调用 recall_context，用行尾的 ref）"
	}
	stats.Tokens = Estimate(kept)
	return Output{Messages: kept, Summary: summary, Archived: archived, Stats: stats}, nil
}

// condensed 把工具结果换成一行的摘要载荷。
func condensed(m llm.Message, line, ref string) string {
	payload, _ := json.Marshal(map[string]any{
		"condensed": true,
		"ref":       ref,
		"summary":   line,
		"note":      "早期结果已压缩为摘要；需要细节可用 recall_context 按 ref 取回",
	})
	return string(payload)
}

func dedupeKey(m llm.Message, calls map[string]llm.ToolCall) string {
	call, ok := calls[m.ToolCallID]
	if !ok {
		return ""
	}
	return call.Function.Name + "|" + call.Function.Arguments
}

// roundOf 用"它是第几个工具结果"来推断轮次（轨迹与 tool 消息一一对应）。
func roundOf(in Input, msgIndex int) int {
	n := 0
	for i := in.Pinned; i < msgIndex && i < len(in.Messages); i++ {
		if in.Messages[i].Role == llm.RoleTool {
			n++
		}
	}
	if n < len(in.Traces) {
		return in.Traces[n].Round
	}
	return n + 1
}

// group 是一整轮：一条 assistant + 紧随其后的 tool 消息。
type group struct {
	index    int
	start    int
	end      int
	round    int
	failures int
	lines    []string
	refs     []string
}

func (g group) summaryLine() string {
	if len(g.lines) == 0 {
		return fmt.Sprintf("- 第%d轮（无工具摘要）", g.round)
	}
	line := "- " + strings.Join(g.lines, "；")
	if len(g.refs) > 0 {
		line += " [" + g.refs[0] + "]"
	}
	return line
}

func (g group) lineFor(callID string) string {
	if len(g.lines) == 0 {
		return ""
	}
	return g.lines[0]
}

// groupMessages 把 pinned 之后的消息按"整轮"切分，并挂上轨迹摘要。
func groupMessages(msgs []llm.Message, pinned int, traces []Trace) []group {
	var gs []group
	traceIdx := 0
	for i := pinned; i < len(msgs); i++ {
		if msgs[i].Role != llm.RoleAssistant {
			continue
		}
		end := i + 1
		for end < len(msgs) && msgs[end].Role == llm.RoleTool {
			end++
		}
		g := group{index: len(gs), start: i, end: end, round: traceIdx + 1}
		if traceIdx < len(traces) {
			g.round = traces[traceIdx].Round
		}
		for mi := i + 1; mi < end; mi++ {
			if msgs[mi].Role != llm.RoleTool {
				continue
			}
			if traceIdx < len(traces) {
				t := traces[traceIdx]
				g.lines = append(g.lines, fmt.Sprintf("第%d轮 %s：%s", t.Round, t.ToolName, t.Summary))
				if t.Failed {
					g.failures++
				}
			}
			traceIdx++
		}
		gs = append(gs, g)
		i = end - 1
	}
	return gs
}

// Errors 供调用方判断策略是否可用。
var ErrUnavailable = errors.New("contextplan: 策略不可用")

// estimateWithSummary 估算"折叠 fold 轮并加一条摘要"后的 token 数。
func estimateWithSummary(msgs []llm.Message, fold int) int {
	if fold == 0 {
		return Estimate(msgs)
	}
	return Estimate(msgs) + 40 + fold*30
}
