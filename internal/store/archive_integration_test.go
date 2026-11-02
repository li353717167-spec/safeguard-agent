package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestRealDBContextArchive 验证归档在真实 MySQL 上的行为：
// 写入、按 ref 取回、按关键词搜索、跨任务隔离、任务删除后级联清理。
func TestRealDBContextArchive(t *testing.T) {
	dsn := os.Getenv("SAFEGUARD_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("未设置 SAFEGUARD_TEST_DB_DSN，跳过真实数据库集成测试")
	}

	ctx := context.Background()
	st, err := Open(ctx, Config{DSN: dsn})
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Now().UTC()
	taskID := fmt.Sprintf("task_arch_%d", time.Now().UnixNano())
	otherTask := taskID + "_other"
	taskRepo := NewTaskRepo(st.GORM())
	for _, id := range []string{taskID, otherTask} {
		if err := taskRepo.Create(ctx, &Task{
			TaskID: id, UserQuery: "归档测试", Status: TaskStatusRunning,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("创建任务失败: %v", err)
		}
	}
	t.Cleanup(func() {
		_ = st.GORM().Where("task_id IN ?", []string{taskID, otherTask}).Delete(&Task{}).Error
	})

	repo := NewContextArchiveRepo(st.GORM())
	if err := repo.Save(ctx, []ContextArchiveEntry{
		{Ref: "ctx_aaaa0001", TaskID: taskID, Round: 1, Role: "tool", ToolName: "query_metrics",
			Content: "payment-service.error_rate 最近 30m：最新 0.08%", Tokens: 20},
		{Ref: "ctx_aaaa0002", TaskID: taskID, Round: 2, Role: "tool", ToolName: "query_config",
			Content: `{"key":"timeout_ms","value":"1500"}`, Tokens: 12},
	}); err != nil {
		t.Fatalf("归档失败: %v", err)
	}

	// 重复归档是正常的（压缩会反复评估同一段历史），不能报错
	if err := repo.Save(ctx, []ContextArchiveEntry{
		{Ref: "ctx_aaaa0001", TaskID: taskID, Round: 1, Role: "tool", Content: "重复", Tokens: 1},
	}); err != nil {
		t.Fatalf("重复归档应被忽略而不是报错: %v", err)
	}

	entry, ok, err := repo.Get(ctx, taskID, "ctx_aaaa0001")
	if err != nil || !ok {
		t.Fatalf("应能按 ref 取回: ok=%v err=%v", ok, err)
	}
	if entry.ToolName != "query_metrics" || entry.Tokens != 20 {
		t.Errorf("取回内容不对: %+v", entry)
	}

	// 跨任务隔离：别的任务读不到
	if _, ok, err := repo.Get(ctx, otherTask, "ctx_aaaa0001"); err != nil || ok {
		t.Fatalf("跨任务不该读到归档: ok=%v err=%v", ok, err)
	}

	found, err := repo.Search(ctx, taskID, "timeout_ms", 0, 5)
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if len(found) != 1 || found[0].Ref != "ctx_aaaa0002" {
		t.Errorf("搜索应命中 timeout_ms 那条: %+v", found)
	}

	// 按轮次过滤
	found, err = repo.Search(ctx, taskID, "", 1, 5)
	if err != nil {
		t.Fatalf("按轮次搜索失败: %v", err)
	}
	if len(found) != 1 || found[0].Round != 1 {
		t.Errorf("按轮次过滤不对: %+v", found)
	}

	// 任务删除 → 归档级联清理（归档属于执行状态，不是审计）
	if err := st.GORM().Where("task_id = ?", taskID).Delete(&Task{}).Error; err != nil {
		t.Fatalf("删除任务失败: %v", err)
	}
	if _, ok, err := repo.Get(ctx, taskID, "ctx_aaaa0001"); err != nil || ok {
		t.Fatalf("任务删除后归档应被级联清理: ok=%v err=%v", ok, err)
	}
	t.Log("归档链路：写入 → 按 ref 取回 → 关键词搜 → 轮次过滤 → 跨任务隔离 → 级联清理")
}
