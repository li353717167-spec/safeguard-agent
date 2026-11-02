package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ArchiveItem 是一条归档内容（工具只依赖这个形状，不依赖具体存储）。
type ArchiveItem struct {
	Ref      string
	Round    int
	Role     string
	ToolName string
	Content  string
	Tokens   int
}

// ContextArchive 是 recall_context 需要的查询能力。
type ContextArchive interface {
	Get(ctx context.Context, taskID, ref string) (ArchiveItem, bool, error)
	Search(ctx context.Context, taskID, query string, round, limit int) ([]ArchiveItem, error)
}

type recallArgs struct {
	Ref   string `json:"ref"`
	Query string `json:"query"`
	Round int    `json:"round"`
	Limit int    `json:"limit"`
}

// maxRecallBytes 是单条回查结果的截断上限。
//
// 必须设上限：否则"取回原文"会把刚省下来的上下文一次性又撑满——压缩就白做了。
const maxRecallBytes = 4000

// recallTool 让压缩变成"可逆的"：
// 压缩时丢掉的内容都带一个 ref（ctx_xxxxxxxx），模型需要细节时按 ref 取回，
// 或用关键词搜索本任务的归档。
type recallTool struct {
	archive ContextArchive
}

// NewRecallContext 构造 recall_context 工具。
func NewRecallContext(archive ContextArchive) Tool {
	return &recallTool{archive: archive}
}

func (t *recallTool) Spec() Spec {
	return Spec{
		Name: "recall_context",
		Description: "取回被压缩掉的早期上下文。压缩摘要里每行末尾的 [ctx:xxxxxxxx] 就是 ref；" +
			"也可以给 query 做关键词搜索。只会查当前任务的归档，单条返回有长度上限。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "ref":   {"type": "string",  "description": "精确取某一条归档，形如 ctx_ab12cd34（摘要行尾方括号里）"},
    "query": {"type": "string",  "description": "关键词搜索（对归档原文做包含匹配），例如 错误率 / timeout_ms"},
    "round": {"type": "integer", "description": "可选：只看某一轮的归档"},
    "limit": {"type": "integer", "description": "可选：最多返回几条（默认 5，上限 20）"}
  },
  "required": []
}`),
		Tier:       TierHarmless,
		Idempotent: true,
	}
}

// Validate 在**执行前**校验参数：坏参数不该走到下游，也不该被记成一次执行。
func (t *recallTool) Validate(rawArgs json.RawMessage) error {
	var args recallArgs
	if len(rawArgs) > 0 {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return fmt.Errorf("参数不是合法 JSON: %w", err)
		}
	}
	if strings.TrimSpace(args.Ref) == "" && strings.TrimSpace(args.Query) == "" {
		return fmt.Errorf("ref 与 query 至少要给一个")
	}
	return nil
}

func (t *recallTool) Invoke(ctx context.Context, rawArgs json.RawMessage) (Result, error) {
	var args recallArgs
	if len(rawArgs) > 0 {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return Result{}, fmt.Errorf("参数不是合法 JSON: %w", err)
		}
	}
	args.Ref = strings.TrimSpace(args.Ref)
	args.Query = strings.TrimSpace(args.Query)
	if args.Ref == "" && args.Query == "" {
		return Result{}, fmt.Errorf("ref 与 query 至少要给一个")
	}

	taskID := TaskIDFromContext(ctx)
	if taskID == "" {
		return Result{}, fmt.Errorf("recall_context 必须在任务上下文里调用（缺少 task_id）")
	}
	if t.archive == nil {
		return Result{Data: map[string]any{"found": false, "note": "当前没有开启上下文归档"}, Summary: "归档未启用"}, nil
	}

	if args.Ref != "" {
		item, ok, err := t.archive.Get(ctx, taskID, args.Ref)
		if err != nil {
			return Result{}, err
		}
		if !ok {
			return Result{Data: map[string]any{
				"found": false,
				"note":  "没有找到这个 ref：可能已被清理，或不属于当前任务",
			}, Summary: fmt.Sprintf("没有 ref=%s 的归档", args.Ref)}, nil
		}
		return Result{
			Data:    map[string]any{"found": true, "count": 1, "items": []any{renderItem(item)}},
			Summary: fmt.Sprintf("取回 ref=%s（第%d轮 %s）", item.Ref, item.Round, roleLabel(item)),
		}, nil
	}

	limit := args.Limit
	if limit <= 0 {
		limit = 5
	}
	if limit > 20 {
		limit = 20
	}
	items, err := t.archive.Search(ctx, taskID, args.Query, args.Round, limit)
	if err != nil {
		return Result{}, err
	}
	if len(items) == 0 {
		return Result{Data: map[string]any{
			"found": false,
			"note":  "没有匹配的归档；换个关键词，或直接看当前上下文",
		}, Summary: fmt.Sprintf("没有匹配「%s」的归档", args.Query)}, nil
	}
	rendered := make([]any, 0, len(items))
	for _, item := range items {
		rendered = append(rendered, renderItem(item))
	}
	return Result{
		Data:    map[string]any{"found": true, "count": len(rendered), "items": rendered},
		Summary: fmt.Sprintf("按「%s」找到 %d 条归档", args.Query, len(rendered)),
	}, nil
}

func renderItem(item ArchiveItem) map[string]any {
	content := item.Content
	truncated := false
	if len(content) > maxRecallBytes {
		content = content[:maxRecallBytes]
		for len(content) > 0 && !utf8.ValidString(content) {
			content = content[:len(content)-1]
		}
		truncated = true
	}
	out := map[string]any{
		"ref":     item.Ref,
		"round":   item.Round,
		"role":    item.Role,
		"content": content,
		"tokens":  item.Tokens,
	}
	if item.ToolName != "" {
		out["tool"] = item.ToolName
	}
	if truncated {
		out["truncated"] = true
		out["note"] = fmt.Sprintf("原文 %d 字节，超过单条回查上限 %d；如需更多请缩小查询范围", len(item.Content), maxRecallBytes)
	}
	return out
}

func roleLabel(item ArchiveItem) string {
	if item.ToolName != "" {
		return item.ToolName
	}
	return item.Role
}

// —— 任务上下文 ——
//
// 工具本身拿不到编排层状态，任务 id 通过 ctx 传递（和幂等 key 是同一个模式）。

type taskIDKey struct{}

// WithTaskID 把任务 id 放进 ctx（编排层在调用工具前注入）。
func WithTaskID(ctx context.Context, taskID string) context.Context {
	return context.WithValue(ctx, taskIDKey{}, taskID)
}

// TaskIDFromContext 读取当前任务 id。
func TaskIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(taskIDKey{}).(string); ok {
		return v
	}
	return ""
}
