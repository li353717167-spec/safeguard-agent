package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeClient 是可编排的端点：按顺序返回预置的结果。
type fakeClient struct {
	name     string
	calls    int
	err      error
	response string
}

func (f *fakeClient) Chat(context.Context, ChatRequest) (ChatResponse, error) {
	f.calls++
	if f.err != nil {
		return ChatResponse{}, f.err
	}
	return ChatResponse{
		Message:  Message{Role: RoleAssistant, Content: f.response},
		Provider: f.name,
		Model:    f.name,
	}, nil
}

func (f *fakeClient) Provider() string { return f.name }
func (f *fakeClient) Model() string    { return f.name }

func retryableErr() error {
	return &APIError{Provider: "fake", Status: 503, Message: "upstream unavailable"}
}

func newTestRouter(t *testing.T, now *time.Time, threshold int, clients ...*fakeClient) *Router {
	t.Helper()
	eps := make([]Endpoint, 0, len(clients))
	for _, c := range clients {
		eps = append(eps, Endpoint{Name: c.name, Client: c})
	}
	r, err := NewRouter(RouterConfig{
		Endpoints:     eps,
		FailThreshold: threshold,
		Cooldown:      30 * time.Second,
		Window:        time.Minute,
		Now:           func() time.Time { return *now },
	})
	if err != nil {
		t.Fatalf("构造路由器失败: %v", err)
	}
	return r
}

// 首选端点挂了 → 同一次调用自动换下一个，上层看到的是"一次成功"。
func TestRouterFailoverToFallback(t *testing.T) {
	now := time.Now().UTC()
	primary := &fakeClient{name: "primary", err: retryableErr()}
	backup := &fakeClient{name: "backup", response: "来自备用端点"}

	var switchedFrom, switchedTo string
	r, err := NewRouter(RouterConfig{
		Endpoints:     []Endpoint{{Name: "primary", Client: primary}, {Name: "backup", Client: backup}},
		FailThreshold: 3,
		Cooldown:      30 * time.Second,
		Now:           func() time.Time { return now },
		OnFailover:    func(from, to, reason string) { switchedFrom, switchedTo = from, to },
	})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}

	resp, err := r.Chat(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatalf("应切换成功，实际: %v", err)
	}
	if resp.Provider != "backup" {
		t.Errorf("应答来自 %s，期望 backup", resp.Provider)
	}
	if primary.calls != 1 || backup.calls != 1 {
		t.Errorf("调用次数 primary=%d backup=%d", primary.calls, backup.calls)
	}
	if switchedFrom != "primary" || switchedTo != "backup" {
		t.Errorf("切换回调未触发: from=%q to=%q", switchedFrom, switchedTo)
	}

	// 粘性：下一次应直接用 backup，不再打 primary
	if _, err := r.Chat(context.Background(), ChatRequest{}); err != nil {
		t.Fatalf("第二次调用失败: %v", err)
	}
	if primary.calls != 1 {
		t.Errorf("粘性失效：primary 被再次调用（%d 次）", primary.calls)
	}
	if backup.calls != 2 {
		t.Errorf("backup 应被调用 2 次，实际 %d", backup.calls)
	}
}

// 全部端点都挂：返回最后一个错误，不能让调用方拿到"空响应 + nil 错误"。
func TestRouterAllEndpointsFail(t *testing.T) {
	now := time.Now().UTC()
	a := &fakeClient{name: "a", err: retryableErr()}
	b := &fakeClient{name: "b", err: &APIError{Status: 500, Message: "boom"}}
	r := newTestRouter(t, &now, 3, a, b)

	if _, err := r.Chat(context.Background(), ChatRequest{}); err == nil {
		t.Fatal("全部失败时应返回错误")
	}
	if a.calls != 1 || b.calls != 1 {
		t.Errorf("每个端点都应被尝试一次：a=%d b=%d", a.calls, b.calls)
	}
}

// 连续失败到阈值 → 冷却；冷却期内不再被优先选择。
func TestRouterCooldownAfterThreshold(t *testing.T) {
	now := time.Now().UTC()
	primary := &fakeClient{name: "primary", err: retryableErr()}
	backup := &fakeClient{name: "backup", response: "ok"}
	r := newTestRouter(t, &now, 2, primary, backup)

	// 第一次：primary 失败 1 次（未到阈值），切 backup 成功
	if _, err := r.Chat(context.Background(), ChatRequest{}); err != nil {
		t.Fatalf("第一次调用失败: %v", err)
	}
	// 第二次：粘在 backup 上（primary 没被调用）
	if _, err := r.Chat(context.Background(), ChatRequest{}); err != nil {
		t.Fatalf("第二次调用失败: %v", err)
	}
	if primary.calls != 1 {
		t.Errorf("粘性失效：primary 被调用 %d 次", primary.calls)
	}

	// 把 backup 弄挂，逼它回到 primary；再失败一次就达到阈值 → 冷却
	backup.err = retryableErr()
	if _, err := r.Chat(context.Background(), ChatRequest{}); err == nil {
		t.Fatal("两个端点都挂时应报错")
	}
	if primary.calls != 2 {
		t.Fatalf("primary 应被再试一次，实际 %d", primary.calls)
	}

	// 现在 primary 连续失败 2 次 = 阈值 → 冷却中
	statuses := r.Statuses()
	var primaryStatus *EndpointStatus
	for i := range statuses {
		if statuses[i].Name == "primary" {
			primaryStatus = &statuses[i]
		}
	}
	if primaryStatus == nil {
		t.Fatal("找不到 primary 状态")
	}
	if primaryStatus.Healthy {
		t.Errorf("primary 连续失败 %d 次应判定不健康: %+v", primaryStatus.ConsecutiveFailures, primaryStatus)
	}
	if primaryStatus.CooldownRemaining <= 0 {
		t.Errorf("应处于冷却中: %+v", primaryStatus)
	}

	// 冷却结束后恢复健康
	now = now.Add(31 * time.Second)
	for _, st := range r.Statuses() {
		if st.Name == "primary" && !st.Healthy {
			t.Errorf("冷却结束后应恢复健康: %+v", st)
		}
	}
}

// 调用方主动取消：不切换、不计端点健康度。
func TestRouterCanceledDoesNotFailover(t *testing.T) {
	now := time.Now().UTC()
	primary := &fakeClient{name: "primary", err: context.Canceled}
	backup := &fakeClient{name: "backup", response: "不该被调用"}
	r := newTestRouter(t, &now, 3, primary, backup)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := r.Chat(ctx, ChatRequest{}); err == nil {
		t.Fatal("应返回取消错误")
	}
	if backup.calls != 0 {
		t.Errorf("取消不该触发切换，backup 被调用了 %d 次", backup.calls)
	}
	if st := r.Statuses()[0]; st.ConsecutiveFailures != 0 {
		t.Errorf("取消不该计入端点失败，实际连续失败 %d", st.ConsecutiveFailures)
	}
}

// 全挂时 fail-open：仍然按"最早恢复"的顺序去试，而不是直接失败。
func TestRouterFailOpenWhenAllCooling(t *testing.T) {
	now := time.Now().UTC()
	a := &fakeClient{name: "a", err: retryableErr()}
	b := &fakeClient{name: "b", response: "b 还活着"}
	r := newTestRouter(t, &now, 1, a, b)

	// 让 a 进入冷却（阈值 1）
	if _, err := r.Chat(context.Background(), ChatRequest{}); err != nil {
		t.Fatalf("应切到 b: %v", err)
	}
	// 让 b 也失败 → 两个都冷却
	b.err = retryableErr()
	if _, err := r.Chat(context.Background(), ChatRequest{}); err == nil {
		t.Fatal("两个都挂应报错")
	}
	// 现在两个都不健康，但 b 恢复后应能被选中（fail-open + 冷却到期）
	b.err = nil
	now = now.Add(31 * time.Second)
	if _, err := r.Chat(context.Background(), ChatRequest{}); err != nil {
		t.Fatalf("冷却到期后应恢复: %v", err)
	}
}

// 健康时就用首选端点，不做多余的切换。
func TestRouterPrefersPrimaryWhenHealthy(t *testing.T) {
	now := time.Now().UTC()
	primary := &fakeClient{name: "primary", response: "ok"}
	backup := &fakeClient{name: "backup", response: "ok"}
	r := newTestRouter(t, &now, 3, primary, backup)

	for i := 0; i < 3; i++ {
		if _, err := r.Chat(context.Background(), ChatRequest{}); err != nil {
			t.Fatalf("调用失败: %v", err)
		}
	}
	if primary.calls != 3 {
		t.Errorf("primary 应被调用 3 次，实际 %d", primary.calls)
	}
	if backup.calls != 0 {
		t.Errorf("healthy 时不该用备份端点，实际 %d", backup.calls)
	}
}

func TestDescribeStatuses(t *testing.T) {
	now := time.Now().UTC()
	primary := &fakeClient{name: "primary", err: retryableErr()}
	backup := &fakeClient{name: "backup", response: "ok"}
	r := newTestRouter(t, &now, 3, primary, backup)
	if _, err := r.Chat(context.Background(), ChatRequest{}); err != nil {
		t.Fatalf("调用失败: %v", err)
	}

	out := DescribeStatuses(r.Statuses())
	if !strings.Contains(out, "primary") || !strings.Contains(out, "backup") {
		t.Errorf("应列出所有端点: %s", out)
	}
	if !strings.Contains(out, "*") {
		t.Errorf("应标记当前粘住的端点: %s", out)
	}
	if !strings.Contains(out, "最后错误") {
		t.Errorf("应展示最后错误: %s", out)
	}
}

var _ = errors.Is
