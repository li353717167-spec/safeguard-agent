package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type fakeArchive struct {
	items map[string]ArchiveItem
	calls int
}

func (f *fakeArchive) Get(_ context.Context, taskID, ref string) (ArchiveItem, bool, error) {
	f.calls++
	item, ok := f.items[ref]
	if !ok || taskID != "task_1" {
		return ArchiveItem{}, false, nil
	}
	return item, true, nil
}

func (f *fakeArchive) Search(_ context.Context, taskID, query string, round, limit int) ([]ArchiveItem, error) {
	f.calls++
	if taskID != "task_1" {
		return nil, nil
	}
	var out []ArchiveItem
	for _, item := range f.items {
		if query != "" && !strings.Contains(item.Content, query) {
			continue
		}
		out = append(out, item)
	}
	if len(out) > limit && limit > 0 {
		out = out[:limit]
	}
	return out, nil
}

func TestRecallContextValidate(t *testing.T) {
	tool := NewRecallContext(&fakeArchive{})
	if err := tool.Validate(json.RawMessage(`{}`)); err == nil {
		t.Error("ref 与 query 都为空时应拒绝")
	}
	if err := tool.Validate(json.RawMessage(`{"ref":"ctx_1"}`)); err != nil {
		t.Errorf("只给 ref 应合法: %v", err)
	}
}

func TestRecallContextNeedsTask(t *testing.T) {
	tool := NewRecallContext(&fakeArchive{})
	_, err := tool.Invoke(context.Background(), json.RawMessage(`{"ref":"ctx_1"}`))
	if err == nil {
		t.Fatal("没有任务上下文时应报错（归档有权限边界）")
	}
}

func TestRecallContextByRef(t *testing.T) {
	fa := &fakeArchive{items: map[string]ArchiveItem{
		"ctx_1": {Ref: "ctx_1", Round: 2, Role: "tool", ToolName: "query_metrics",
			Content: "error_rate 0.08%", Tokens: 10},
	}}
	tool := NewRecallContext(fa)
	ctx := WithTaskID(context.Background(), "task_1")

	res, err := tool.Invoke(ctx, json.RawMessage(`{"ref":"ctx_1"}`))
	if err != nil {
		t.Fatalf("取回失败: %v", err)
	}
	data := res.Data.(map[string]any)
	if data["found"] != true || data["count"] != 1 {
		t.Errorf("应取回 1 条: %v", data)
	}
	if !strings.Contains(res.Summary, "ctx_1") {
		t.Errorf("摘要应含 ref: %s", res.Summary)
	}
}

func TestRecallContextNotFound(t *testing.T) {
	tool := NewRecallContext(&fakeArchive{items: map[string]ArchiveItem{}})
	ctx := WithTaskID(context.Background(), "task_1")
	res, err := tool.Invoke(ctx, json.RawMessage(`{"ref":"ctx_nope"}`))
	if err != nil {
		t.Fatalf("未找到不该报错: %v", err)
	}
	if res.Data.(map[string]any)["found"] != false {
		t.Error("应返回 found=false")
	}
}

// 回查必须有长度上限：否则会把刚省下的上下文一次性撑满。
func TestRecallContextTruncatesLongContent(t *testing.T) {
	fa := &fakeArchive{items: map[string]ArchiveItem{
		"ctx_big": {Ref: "ctx_big", Round: 1, Role: "tool", Content: strings.Repeat("x", maxRecallBytes+500)},
	}}
	tool := NewRecallContext(fa)
	ctx := WithTaskID(context.Background(), "task_1")

	res, err := tool.Invoke(ctx, json.RawMessage(`{"ref":"ctx_big"}`))
	if err != nil {
		t.Fatalf("取回失败: %v", err)
	}
	items := res.Data.(map[string]any)["items"].([]any)
	first := items[0].(map[string]any)
	content := first["content"].(string)
	if len(content) > maxRecallBytes {
		t.Errorf("单条内容应被截断到 %d，实际 %d", maxRecallBytes, len(content))
	}
	if first["truncated"] != true {
		t.Error("应标记 truncated")
	}
}

func TestRecallContextSearch(t *testing.T) {
	fa := &fakeArchive{items: map[string]ArchiveItem{
		"ctx_1": {Ref: "ctx_1", Round: 1, Role: "tool", Content: "timeout_ms=1500"},
		"ctx_2": {Ref: "ctx_2", Round: 2, Role: "tool", Content: "error_rate 0.08%"},
	}}
	tool := NewRecallContext(fa)
	ctx := WithTaskID(context.Background(), "task_1")

	res, err := tool.Invoke(ctx, json.RawMessage(`{"query":"timeout"}`))
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	data := res.Data.(map[string]any)
	if data["count"] != 1 {
		t.Errorf("应命中 1 条: %v", data)
	}
}
