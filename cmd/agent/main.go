// Command agent 是 safeguard-agent 的入口。
//
// 入口只负责装配与接线，不放业务判断。当前支持的动作：
//
//	-ask          走完整的 ReAct 循环（多轮工具调用 + 双上限）
//	-tool-probe   单轮工具探针（验证工具选择）
//	-tools        打印工具清单与权限策略
//	-migrate      执行 schema 迁移
//	-seed         灌入/刷新示例服务目录
//	-db-ping      探测数据库与 ORM 模型映射
//	-prompt       调一次模型（不含工具）
//
// 策略引擎与幂等执行器在 D5~D6 接入。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/mechrevo/safeguard-agent/internal/approval"
	"github.com/mechrevo/safeguard-agent/internal/config"
	"github.com/mechrevo/safeguard-agent/internal/eval"
	"github.com/mechrevo/safeguard-agent/internal/executor"
	"github.com/mechrevo/safeguard-agent/internal/llm"
	"github.com/mechrevo/safeguard-agent/internal/orchestrator"
	"github.com/mechrevo/safeguard-agent/internal/policy"
	"github.com/mechrevo/safeguard-agent/internal/store"
	"github.com/mechrevo/safeguard-agent/internal/tools"
)

// version 会在打 tag 时同步更新；用于确认二进制是不是最新构建。
const version = "0.1.0-d3"

// 退出码约定：0 成功、1 运行失败、2 用法或配置错误。
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

func main() {
	os.Exit(realMain(os.Args[1:]))
}

func realMain(args []string) int {
	fs := flag.NewFlagSet("safeguard-agent", flag.ContinueOnError)
	prompt := fs.String("prompt", "", "一次性用户输入：调用一次模型并打印返回")
	ask := fs.String("ask", "", "走完整的 ReAct 循环（多轮工具调用 + 双上限）回答问题")
	toolProbe := fs.String("tool-probe", "", "单轮工具探针：让模型选一次工具并执行（验证工具选择）")
	maxRounds := fs.Int("max-rounds", 0, "覆盖 ReAct 轮次上限（默认 8）")
	maxToolCalls := fs.Int("max-tool-calls", 0, "覆盖工具调用次数上限（默认 20）")
	listTools := fs.Bool("tools", false, "打印已注册工具清单与权限策略后退出")
	listApprovals := fs.Bool("approvals", false, "列出待审批工单后退出")
	approve := fs.String("approve", "", "批准指定工单（tk_...）后退出")
	reject := fs.String("reject", "", "拒绝指定工单（tk_...）后退出")
	rejectReason := fs.String("reason", "", "配合 -reject：拒绝理由（会被回喂给模型）")
	resumeID := fs.String("resume", "", "从断点恢复指定任务（task_...）并继续执行")
	auditArg := fs.String("audit", "", "查看审计：传 task_... 看该任务，传 all 看最近 20 条")
	evalCases := fs.String("eval", "", "跑评测：指定评测集 JSON 路径（如 internal/eval/cases.json）")
	evalModel := fs.String("eval-model", "fake", "评测用模型：fake（理想模型，验证判分器）| broken（坏模型，验证判分能发现错误）| real（真实模型）")
	evalOut := fs.String("eval-out", "", "把评测报告写成 Markdown 到指定路径")
	evalJudge := fs.Bool("eval-judge", false, "额外用模型做语义判分（非确定性，仅作参考）")
	doMigrate := fs.Bool("migrate", false, "执行数据库迁移后退出")
	doSeed := fs.Bool("seed", false, "灌入/刷新示例服务目录后退出（可重复执行）")
	dbPing := fs.Bool("db-ping", false, "探测数据库连通性后退出")
	showRaw := fs.Bool("raw", false, "打印模型原始响应（排查用）")
	timeout := fs.Duration("timeout", 0, "覆盖单次模型调用超时，例如 30s")
	showVersion := fs.Bool("version", false, "打印版本号")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "safeguard-agent %s · 带权限管控与人工审批的运维 Agent\n\n", version)
		fmt.Fprintf(fs.Output(), "用法:\n"+
			"  safeguard-agent -ask \"checkout-service 为什么变慢了\"\n"+
			"  safeguard-agent -tool-probe \"payment-service 最近 5 分钟的错误率怎么样\"\n"+
			"  safeguard-agent -tools\n"+
			"  safeguard-agent -approvals\n"+
			"  safeguard-agent -approve <tk_...>\n"+
			"  safeguard-agent -reject <tk_...> -reason \"...\"\n"+
			"  safeguard-agent -resume <task_...>\n"+
			"  safeguard-agent -audit <task_...|all>\n"+
			"  safeguard-agent -eval internal/eval/cases.json -eval-model=fake\n"+
			"  safeguard-agent -migrate\n"+
			"  safeguard-agent -seed\n"+
			"  safeguard-agent -db-ping\n"+
			"  safeguard-agent -prompt \"你好\"\n\n参数:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *showVersion {
		fmt.Println(version)
		return exitOK
	}

	// 本地开发用 .env；线上直接注入环境变量（.env 不覆盖已存在的真实环境变量）。
	dotEnvKeys, err := config.LoadDotEnv(".env")
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取 .env 失败: %v\n", err)
		return exitUsage
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "配置错误: %v\n", err)
		return exitUsage
	}
	level, err := cfg.SlogLevel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "配置错误: %v\n", err)
		return exitUsage
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
	if dotEnvKeys > 0 {
		slog.Debug("已从 .env 装载配置", "keys", dotEnvKeys)
	}

	// 一次 Ctrl+C 就取消整个流程：长任务要能在收到信号后把状态留在库里（D6/D7 依赖这一点）。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch {
	case *listTools:
		return runListTools()
	case *listApprovals:
		return runApprovals(ctx, cfg)
	case strings.TrimSpace(*approve) != "":
		return runDecide(ctx, cfg, strings.TrimSpace(*approve), store.ApprovalStatusApproved, "cli-user", "")
	case strings.TrimSpace(*reject) != "":
		return runDecide(ctx, cfg, strings.TrimSpace(*reject), store.ApprovalStatusRejected, "cli-user", *rejectReason)
	case strings.TrimSpace(*resumeID) != "":
		return runResume(ctx, cfg, strings.TrimSpace(*resumeID), *timeout, *showRaw)
	case strings.TrimSpace(*auditArg) != "":
		return runAudit(ctx, cfg, strings.TrimSpace(*auditArg))
	case strings.TrimSpace(*evalCases) != "":
		return runEval(ctx, cfg, strings.TrimSpace(*evalCases), *evalModel, *evalOut, *evalJudge, *timeout)
	case *doMigrate:
		return runMigrate(ctx, cfg)
	case *doSeed:
		return runSeed(ctx, cfg)
	case *dbPing:
		return runDBPing(ctx, cfg)
	case strings.TrimSpace(*ask) != "":
		return runAsk(ctx, cfg, strings.TrimSpace(*ask), *timeout, *maxRounds, *maxToolCalls)
	case strings.TrimSpace(*toolProbe) != "":
		return runToolProbe(ctx, cfg, strings.TrimSpace(*toolProbe), *timeout, *showRaw)
	case strings.TrimSpace(*prompt) != "":
		return runPrompt(ctx, cfg, strings.TrimSpace(*prompt), *timeout, *showRaw)
	default:
		fs.Usage()
		return exitUsage
	}
}

// openStore 是迁移与探活共用的装配入口：把应用配置翻译成 store.Config。
// Debug 跟随日志级别——只有真的要看 SQL 时才打开 ORM 的语句日志。
func openStore(ctx context.Context, cfg config.Config) (*store.Store, error) {
	return store.Open(ctx, store.Config{
		DSN:             cfg.DB.DSN,
		MaxOpenConns:    cfg.DB.MaxOpenConns,
		MaxIdleConns:    cfg.DB.MaxIdleConns,
		ConnMaxLifetime: cfg.DB.ConnMaxLifetime,
		Debug:           strings.EqualFold(cfg.LogLevel, "debug"),
	})
}

// runMigrate 把数据库 schema 对齐到当前二进制内置版本。
func runMigrate(ctx context.Context, cfg config.Config) int {
	if err := cfg.DB.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "配置错误: %v\n", err)
		return exitUsage
	}

	st, err := openStore(ctx, cfg)
	if err != nil {
		slog.Error("连接数据库失败", "err", err)
		return exitFailure
	}
	defer st.Close()

	slog.Info("开始迁移", "dsn", st.SafeDSN())
	res, err := st.Migrate(ctx)
	if err != nil {
		slog.Error("迁移失败", "err", err)
		return exitFailure
	}

	if len(res.Applied) == 0 {
		slog.Info("schema 已是最新，无需迁移", "已应用", len(res.Skipped))
	} else {
		slog.Info("迁移完成", "本次执行", res.Applied, "此前已应用", len(res.Skipped))
	}
	if len(res.Missing) > 0 {
		// 不直接失败：可能是本地清理过历史迁移文件。但必须让人看见，这是 schema 漂移信号。
		slog.Warn("数据库中存在仓库里找不到的迁移版本（schema 漂移信号）", "versions", res.Missing)
	}
	return exitOK
}

// runDBPing 验证连通性、连接池参数，以及 ORM 模型与实际表的映射。
func runDBPing(ctx context.Context, cfg config.Config) int {
	if err := cfg.DB.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "配置错误: %v\n", err)
		return exitUsage
	}

	st, err := openStore(ctx, cfg)
	if err != nil {
		slog.Error("连接数据库失败", "err", err)
		return exitFailure
	}
	defer st.Close()

	if err := st.Ping(ctx); err != nil {
		slog.Error("数据库探测失败", "err", err)
		return exitFailure
	}

	// 走 GORM 读服务器信息：这条路径顺带验证了 ORM 装配（DSN 参数、TLS、UTC）。
	var info struct {
		Version   string
		Now       time.Time
		Isolation string
	}
	if err := st.GORM().
		Raw("SELECT VERSION() AS version, UTC_TIMESTAMP(3) AS now, @@transaction_isolation AS isolation").
		Scan(&info).Error; err != nil {
		slog.Error("读取数据库信息失败", "err", err)
		return exitFailure
	}

	fmt.Println("--- 数据库连通 ---")
	fmt.Printf("dsn=%s\n", st.SafeDSN())
	fmt.Printf("server_version=%s\n", info.Version)
	fmt.Printf("server_utc_time=%s\n", info.Now.UTC().Format(time.RFC3339Nano))
	fmt.Printf("tx_isolation=%s\n", info.Isolation)
	fmt.Printf("pool: open=%d idle=%d max_open=%d\n",
		st.Stats().OpenConnections, st.Stats().Idle, cfg.DB.MaxOpenConns)

	// 模型 ↔ 实际表 的自检：对 5 张表各做一次 Count。
	// 模型标签/表名/列名与迁移不一致时这里会立刻失败——比等到 D5 写数据时才发现便宜得多。
	fmt.Println("--- ORM 模型映射自检 ---")
	for _, m := range []any{
		&store.Task{}, &store.TaskStep{}, &store.ToolCall{}, &store.Approval{}, &store.AuditLog{},
		&store.Service{}, &store.ServiceConfig{}, &store.ServiceMetric{}, &store.ConfigChange{},
	} {
		namer, ok := m.(interface{ TableName() string })
		if !ok {
			slog.Error("模型未实现 TableName()：不允许依赖 GORM 的复数化推断")
			return exitFailure
		}
		var rows int64
		if err := st.GORM().Model(m).Count(&rows).Error; err != nil {
			slog.Error("模型与表不匹配（比对结构与 migrations/*.sql）", "table", namer.TableName(), "err", err)
			return exitFailure
		}
		fmt.Printf("table %-12s rows=%d\n", namer.TableName(), rows)
	}
	return exitOK
}

// runPrompt 走一次真实模型调用，验证抽象层与凭据配置。
func runPrompt(ctx context.Context, cfg config.Config, prompt string, timeoutOverride time.Duration, showRaw bool) int {
	if err := cfg.LLM.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "配置错误: %v\n", err)
		return exitUsage
	}
	if timeoutOverride > 0 {
		cfg.LLM.Timeout = timeoutOverride
	}

	client, err := newLLMClient(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "配置错误: %v\n", err)
		return exitUsage
	}
	// 记录时只说模型与端点，绝不打印 key。
	slog.Debug("调用模型", "provider", client.Provider(), "model", client.Model(), "base_url", cfg.LLM.BaseURL, "timeout", cfg.LLM.Timeout)

	callCtx, cancel := context.WithTimeout(ctx, cfg.LLM.Timeout)
	defer cancel()

	resp, err := client.Chat(callCtx, llm.ChatRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: prompt}},
	})
	if err != nil {
		var apiErr *llm.APIError
		if errors.As(err, &apiErr) {
			slog.Error("模型调用失败",
				"status", apiErr.Status,
				"code", apiErr.Code,
				"retryable", apiErr.Retryable(),
				"retry_after", apiErr.RetryAfter,
				"request_id", apiErr.RequestID,
				"err", apiErr.Message,
			)
		} else {
			slog.Error("模型调用失败", "err", err)
		}
		return exitFailure
	}

	printResponse(resp, showRaw)
	return exitOK
}

// newLLMClient 按配置构造模型客户端。
// 换模型只改 .env 的 BaseURL / Model / APIKey，这里与业务代码都不用动。
func newLLMClient(cfg config.Config) (llm.Client, error) {
	if err := cfg.LLM.Validate(); err != nil {
		return nil, err
	}
	return llm.NewOpenAICompat(llm.OpenAICompatConfig{
		BaseURL: cfg.LLM.BaseURL,
		APIKey:  cfg.LLM.APIKey,
		Model:   cfg.LLM.Model,
	})
}

// runApprovals 列出待审批工单。
//
// 顺手把超时未处理的工单标记为过期：没人批的挂起不该无限期占着——
// 它会让任务永远停在 waiting_approval，而恢复流程也无从判断"这张单还算不算数"。
func runApprovals(ctx context.Context, cfg config.Config) int {
	gate, st, code := openGate(ctx, cfg)
	if gate == nil {
		return code
	}
	defer st.Close()

	expired, err := gate.ExpireOverdue(ctx)
	if err != nil {
		slog.Error("处理过期工单失败", "err", err)
		return exitFailure
	}
	if expired > 0 {
		slog.Info("已把超时未处理的工单标记为过期", "count", expired)
	}

	pending, err := gate.ListPending(ctx)
	if err != nil {
		slog.Error("读取待审批工单失败", "err", err)
		return exitFailure
	}
	if len(pending) == 0 {
		fmt.Println("--- 没有待审批工单 ---")
		return exitOK
	}

	fmt.Printf("--- 待审批工单（%d）---\n", len(pending))
	for _, t := range pending {
		preview := ""
		if t.Preview != nil {
			preview = *t.Preview
		}
		fmt.Printf("* %s  tool=%s  task=%s  过期=%s\n",
			t.TicketID, t.ToolName, t.TaskID, t.ExpiresAt.Format(time.RFC3339))
		fmt.Printf("  预览：%s\n", preview)
		fmt.Printf("  批准：-approve %s    拒绝：-reject %s -reason \"...\"\n", t.TicketID, t.TicketID)
	}
	return exitOK
}

// runAudit 打印审计时间线。
//
// 审计只增不删，所以它比任务状态"活得久"：任务被删掉之后，
// 仍然能查到"谁在什么时候对哪个服务做了什么"。
func runAudit(ctx context.Context, cfg config.Config, arg string) int {
	if err := cfg.DB.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "配置错误: %v\n", err)
		return exitUsage
	}
	st, err := openStore(ctx, cfg)
	if err != nil {
		slog.Error("连接数据库失败", "err", err)
		return exitFailure
	}
	defer st.Close()

	repo := store.NewAuditRepo(st.GORM())
	var rows []store.AuditLog
	if arg == "all" {
		rows, err = repo.ListRecent(ctx, 20)
	} else {
		rows, err = repo.ListByTask(ctx, arg)
	}
	if err != nil {
		slog.Error("读取审计失败", "err", err)
		return exitFailure
	}
	if len(rows) == 0 {
		fmt.Println("--- 没有审计记录 ---")
		return exitOK
	}

	fmt.Printf("--- 审计（%d 条）---\n", len(rows))
	for _, r := range rows {
		target := "-"
		if r.Target != nil {
			target = *r.Target
		}
		detail := "-"
		if r.Detail != nil {
			detail = truncateRunes(*r.Detail, 70)
		}
		fmt.Printf("%s  %-20s %-22s %-16s %s\n",
			r.CreatedAt.Format("01-02 15:04:05"), r.Action, target, r.Actor, detail)
	}
	return exitOK
}

// runDecide 批准或拒绝一张工单。
func runDecide(ctx context.Context, cfg config.Config, ticketID, status, approver, reason string) int {
	gate, st, code := openGate(ctx, cfg)
	if gate == nil {
		return code
	}
	defer st.Close()

	ticket, err := gate.Decide(ctx, ticketID, status, approver, reason)
	if err != nil {
		slog.Error("处理审批失败", "ticket", ticketID, "err", err)
		return exitFailure
	}
	// 审批是"人做的决定"，必须留痕：actor 记录到具体审批人。
	if auditErr := store.NewAuditRepo(st.GORM()).Record(ctx, store.AuditEvent{
		TaskID: ticket.TaskID,
		Actor:  store.ActorApprover + approver,
		Action: store.AuditApprovalDecided,
		Target: ticket.ToolName,
		Detail: map[string]any{"ticket_id": ticket.TicketID, "status": ticket.Status, "reason": reason},
	}); auditErr != nil {
		slog.Error("写审批审计失败", "ticket", ticketID, "err", auditErr)
	}

	action := "批准"
	if status == store.ApprovalStatusRejected {
		action = "拒绝"
	}
	fmt.Printf("--- 工单 %s 已%s ---\n", ticket.TicketID, action)
	fmt.Printf("tool=%s task=%s status=%s approver=%s\n", ticket.ToolName, ticket.TaskID, ticket.Status, approver)
	if ticket.Preview != nil {
		fmt.Printf("预览：%s\n", *ticket.Preview)
	}
	if reason != "" {
		fmt.Printf("理由：%s（拒绝理由会被回喂给模型，让它给出替代方案）\n", reason)
	}
	if status == store.ApprovalStatusApproved {
		fmt.Println("说明：本次只记录审批结论，工单与执行意图都已在库里；从断点继续执行由 D7 的恢复流程接手。")
	}
	return exitOK
}

// openGate 是审批相关命令共用的装配入口。
func openGate(ctx context.Context, cfg config.Config) (*approval.Gate, *store.Store, int) {
	if err := cfg.DB.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "配置错误: %v\n", err)
		return nil, nil, exitUsage
	}
	st, err := openStore(ctx, cfg)
	if err != nil {
		slog.Error("连接数据库失败", "err", err)
		return nil, nil, exitFailure
	}
	gate := approval.NewGate(store.NewToolCallRepo(st.GORM()), store.NewApprovalRepo(st.GORM()))
	return gate, st, exitOK
}

// runListTools 打印工具清单与权限策略。
// 它不需要数据库与模型，所以数据源传 nil——注册门禁校验的是定义，不是执行。
func runListTools() int {
	// nil 数据源：这里只取定义，不执行任何工具。
	reg, err := tools.NewFullRegistry(nil, nil)
	if err != nil {
		slog.Error("注册内置工具失败（启动门禁）", "err", err)
		return exitFailure
	}

	fmt.Printf("--- 已注册工具（%d 个）---\n", reg.Len())
	for _, e := range reg.Inventory() {
		approval := "否"
		if e.RequireApproval {
			approval = fmt.Sprintf("是（角色=%s TTL=%s）", e.ApprovalRole, e.ApprovalTTL)
		}
		fmt.Printf("* %s\n", e.Name)
		fmt.Printf("  tier=%s 审批=%s 超时=%s 重试=%d 幂等=%v 干跑=%v\n",
			e.Tier, approval, e.Timeout, e.MaxRetry, e.Idempotent, e.DryRun)
		fmt.Printf("  描述=%s\n", e.Description)
	}
	fmt.Println()
	fmt.Println("注意：tier / 审批 / 重试与超时策略只存在于工具定义里，永远不会出现在给模型看的 schema 中。")
	return exitOK
}

// runSeed 灌入示例服务目录。
// 可重复执行：指标的时间窗按当前时刻重新生成，所以演示与评测前重跑一次就能保证"最近 5 分钟"有数据。
func runSeed(ctx context.Context, cfg config.Config) int {
	if err := cfg.DB.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "配置错误: %v\n", err)
		return exitUsage
	}

	st, err := openStore(ctx, cfg)
	if err != nil {
		slog.Error("连接数据库失败", "err", err)
		return exitFailure
	}
	defer st.Close()

	res, err := store.SeedDemoCatalog(ctx, st.GORM())
	if err != nil {
		slog.Error("灌入示例数据失败", "err", err)
		return exitFailure
	}

	fmt.Println("--- 示例服务目录已灌入 ---")
	fmt.Printf("services=%d configs=%d metrics=%d\n", res.Services, res.Configs, res.Metrics)
	fmt.Printf("采样窗口: %s ~ %s (UTC)\n",
		res.WindowStart.Format(time.RFC3339), res.WindowEnd.Format(time.RFC3339))
	return exitOK
}

// runAsk 走完整的 ReAct 循环（D3）：多轮工具调用 + 双上限 + 每步落库。
//
// 它是"Agent 真正跑起来"的入口：模型自己决定调几次工具、调哪个，
// 循环负责执行、回喂、限流，并把轨迹写进状态机。
// runEval 跑评测集并输出报告（D8–D12）。
//
// 三种模型模式对应三种用途，别混：
//   - fake：理想模型，用来验证**判分器**是对的（CI 用，不需要真模型，结果确定性）；
//   - broken：故意犯错的模型，用来验证判分器**会**判失败；
//   - real：真实模型，用来产出一份真实的能力报告（结果非确定性，需注明模型与时间）。
func runEval(ctx context.Context, cfg config.Config, casesPath, modelMode, outPath string, modelJudge bool, timeoutOverride time.Duration) int {
	cases, err := eval.LoadCases(casesPath)
	if err != nil {
		slog.Error("加载评测集失败", "path", casesPath, "err", err)
		return exitFailure
	}

	var model llm.Client
	provider, modelName := "oracle", "perfect-agent"
	switch strings.ToLower(modelMode) {
	case "", "fake":
		// nil 表示用理想模型
	case "broken":
		model = eval.NewBroken()
		provider, modelName = model.Provider(), model.Model()
	case "real":
		if timeoutOverride > 0 {
			cfg.LLM.Timeout = timeoutOverride
		}
		c, err := newLLMClient(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "配置错误: %v\n", err)
			return exitUsage
		}
		model = c
		provider, modelName = c.Provider(), c.Model()
	default:
		fmt.Fprintf(os.Stderr, "未知的 -eval-model: %s（可选 fake/broken/real）\n", modelMode)
		return exitUsage
	}

	fmt.Printf("--- 评测开始：%d 个用例，模型 %s/%s ---\n", len(cases), provider, modelName)
	results := eval.Evaluate(ctx, cases, eval.NewRunner(model, eval.Options{}))

	// 语义判分（可选）：只作参考列，不改变 Pass 判定。
	if modelJudge && model != nil {
		for i := range results {
			pass, note := eval.JudgeModel(ctx, model, results[i].Case, results[i].Outcome.Answer)
			results[i].Verdict.SemanticPass = &pass
			results[i].Verdict.SemanticNote = note
		}
	}

	metrics := eval.Aggregate(results)
	fmt.Printf("总通过率 %.0f%%（%d/%d）｜正常题 %.0f%%｜陷阱题拒绝率 %.0f%%｜上限触发 %d｜冗余调用 %d\n",
		metrics.PassRate(), metrics.Passed, metrics.Total,
		metrics.GoodRate(), metrics.TrapRate(), metrics.LimitHits, metrics.Redundant)
	for _, r := range results {
		mark := "✅"
		if !r.Verdict.Pass {
			mark = "❌"
		}
		fmt.Printf("%s %-28s stop=%-16s tools=%s\n", mark, r.Case.ID, r.Outcome.Stop, toolSeq(r.Outcome.Traces))
		if !r.Verdict.Pass {
			fmt.Printf("     原因：%s\n", strings.Join(r.Verdict.Failures, "；"))
		}
	}

	if outPath != "" {
		report := eval.RenderMarkdown("Agent 评测报告（safeguard-agent）", provider, modelName, results, metrics)
		if err := os.WriteFile(outPath, []byte(report), 0o600); err != nil {
			slog.Error("写评测报告失败", "path", outPath, "err", err)
			return exitFailure
		}
		fmt.Printf("报告已写入 %s\n", outPath)
	}
	return exitOK
}

// toolSeq 把轨迹压成"工具名(状态)"的序列，方便一眼看出它做了什么。
func toolSeq(traces []orchestrator.ToolTrace) string {
	if len(traces) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(traces))
	for _, t := range traces {
		mark := t.ToolName
		if t.Rejected || t.Error != "" {
			mark += "!"
		}
		parts = append(parts, mark)
	}
	return strings.Join(parts, "→")
}

// runResume 从断点恢复一个任务（D7）。
//
// 装配与 -ask 完全一致：同一个数据源、同一套工具、同一个落库执行器、同一套策略与门禁。
// 这是刻意的——"恢复"必须是"同一段代码换个入口"，否则恢复路径本身就成了没人验证的分支。
func runResume(ctx context.Context, cfg config.Config, taskID string, timeoutOverride time.Duration, showRaw bool) int {
	if err := cfg.DB.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "配置错误: %v\n", err)
		return exitUsage
	}
	if timeoutOverride > 0 {
		cfg.LLM.Timeout = timeoutOverride
	}
	client, err := newLLMClient(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "配置错误: %v\n", err)
		return exitUsage
	}

	st, err := openStore(ctx, cfg)
	if err != nil {
		slog.Error("连接数据库失败", "err", err)
		return exitFailure
	}
	defer st.Close()

	ds := tools.NewMySQLDataSource(st.GORM())
	reg, err := tools.NewFullRegistry(ds, ds)
	if err != nil {
		slog.Error("注册内置工具失败（启动门禁）", "err", err)
		return exitFailure
	}

	taskRepo := store.NewTaskRepo(st.GORM())
	toolCalls := store.NewToolCallRepo(st.GORM())
	gate := approval.NewGate(toolCalls, store.NewApprovalRepo(st.GORM()))

	// 单独持有执行器，跑完才能打印熔断状态（它是运行期信号，只在内存里）。
	exec := executor.New(executor.DefaultConfig())
	defer printBreakers(exec)

	o := orchestrator.New(client, reg, taskRepo, orchestrator.Options{
		TaskID:      taskID,
		Limits:      orchestrator.DefaultLimits(),
		ChatTimeout: cfg.LLM.Timeout,
		Runner:      executor.NewPersisted(exec, toolCalls),
		Policy:      policy.New(),
		Gate:        gate,
		Auditor:     store.NewAuditRepo(st.GORM()),
		Budget: orchestrator.ContextBudget{
			MaxTokens:          cfg.Context.MaxTokens,
			KeepRecentRounds:   cfg.Context.KeepRecentRounds,
			MaxToolResultBytes: cfg.Context.MaxToolResultBytes,
		},
		OnEvent: printOrchestratorEvent,
	})

	fmt.Printf("--- 从断点恢复 %s ---\n", taskID)
	res, err := o.Resume(ctx, taskID)
	if err != nil {
		slog.Error("恢复失败", "task_id", taskID, "err", err)
		return exitFailure
	}
	// 谁在什么时候恢复了哪个任务——审计要能回答这个问题。
	if auditErr := store.NewAuditRepo(st.GORM()).Record(ctx, store.AuditEvent{
		TaskID: taskID,
		Actor:  store.ActorCLI,
		Action: store.AuditTaskResumed,
		Detail: map[string]any{"stop": string(res.Stop), "tool_calls": res.ToolCalls, "rounds": res.Rounds},
	}); auditErr != nil {
		slog.Error("写恢复审计失败", "task_id", taskID, "err", auditErr)
	}

	fmt.Println("--- 最终回答 ---")
	fmt.Println(res.Answer)
	if res.WaitingApproval() && res.Ticket != nil {
		fmt.Printf("--- 仍然需要审批 ---\nticket=%s\n", res.Ticket.TicketID)
	}
	fmt.Println("--- 运行信息 ---")
	fmt.Printf("task_id=%s 停止原因=%s 轮次=%d 工具调用=%d 上限触发=%v\n",
		res.TaskID, res.Stop, res.Rounds, res.ToolCalls, res.LimitTriggered())
	return exitOK
}

func runAsk(ctx context.Context, cfg config.Config, question string, timeoutOverride time.Duration, maxRounds, maxToolCalls int) int {
	if err := cfg.DB.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "配置错误: %v\n", err)
		return exitUsage
	}
	if timeoutOverride > 0 {
		cfg.LLM.Timeout = timeoutOverride
	}
	client, err := newLLMClient(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "配置错误: %v\n", err)
		return exitUsage
	}

	st, err := openStore(ctx, cfg)
	if err != nil {
		slog.Error("连接数据库失败", "err", err)
		return exitFailure
	}
	defer st.Close()

	ds := tools.NewMySQLDataSource(st.GORM())
	// D6：注册全部工具（只读 + 写 + 高危）。三个写/高危工具的权限声明会被策略引擎读到。
	reg, err := tools.NewFullRegistry(ds, ds)
	if err != nil {
		slog.Error("注册内置工具失败（启动门禁）", "err", err)
		return exitFailure
	}

	limits := orchestrator.DefaultLimits()
	if maxRounds > 0 {
		limits.MaxRounds = maxRounds
	}
	if maxToolCalls > 0 {
		limits.MaxToolCalls = maxToolCalls
	}

	// 单独持有执行器与仓储句柄：跑完要打印熔断状态（运行期信号，只在内存里）。
	exec := executor.New(executor.DefaultConfig())
	toolCalls := store.NewToolCallRepo(st.GORM())
	// 退出时一定打印熔断状态：失败时更需要知道"是不是工具被熔断了"。
	defer printBreakers(exec)

	o := orchestrator.New(client, reg, store.NewTaskRepo(st.GORM()), orchestrator.Options{
		Limits:      limits,
		ChatTimeout: cfg.LLM.Timeout,
		// D4 + D5：执行器负责分类/重试/熔断，并用 tool_calls 做三段式落库与回放。
		Runner: executor.NewPersisted(exec, toolCalls),
		// D6：策略引擎（纯规则）+ 审批门禁（先落库再挂起）。
		Policy:  policy.New(),
		Gate:    approval.NewGate(toolCalls, store.NewApprovalRepo(st.GORM())),
		Auditor: store.NewAuditRepo(st.GORM()),
		// 上下文预算：超了先裁剪工具结果，再折叠早期整轮。
		Budget: orchestrator.ContextBudget{
			MaxTokens:          cfg.Context.MaxTokens,
			KeepRecentRounds:   cfg.Context.KeepRecentRounds,
			MaxToolResultBytes: cfg.Context.MaxToolResultBytes,
		},
		OnEvent: printOrchestratorEvent,
	})

	fmt.Printf("--- ReAct 循环开始（上限：轮次 %d / 工具调用 %d）---\n", limits.MaxRounds, limits.MaxToolCalls)
	res, err := o.Run(ctx, question)
	if err != nil {
		slog.Error("执行失败", "task_id", res.TaskID, "err", err)
		return exitFailure
	}

	fmt.Println("--- 最终回答 ---")
	fmt.Println(res.Answer)
	if res.WaitingApproval() && res.Ticket != nil {
		fmt.Println("--- 需要人工审批 ---")
		fmt.Printf("ticket=%s tool=%s task=%s\n", res.Ticket.TicketID, res.Ticket.ToolName, res.Ticket.TaskID)
		if res.Ticket.Preview != nil {
			fmt.Printf("预览：%s\n", *res.Ticket.Preview)
		}
		fmt.Printf("批准：go run ./cmd/agent -approve %s\n", res.Ticket.TicketID)
		fmt.Printf("拒绝：go run ./cmd/agent -reject %s -reason \"...\"\n", res.Ticket.TicketID)
		fmt.Println("说明：工单与执行意图已经落库，进程现在退出也不会丢；从断点续跑属于 D7。")
	}
	fmt.Println("--- 运行信息 ---")
	fmt.Printf("task_id=%s 停止原因=%s 轮次=%d 工具调用=%d 上限触发=%v\n",
		res.TaskID, res.Stop, res.Rounds, res.ToolCalls, res.LimitTriggered())
	if res.Context.Budget > 0 {
		fmt.Printf("上下文: 估算 %d/%d tokens（裁剪 %d 条工具结果、折叠 %d 轮）\n",
			res.Context.Tokens, res.Context.Budget, res.Context.Trimmed, res.Context.FoldedRounds)
	}
	fmt.Printf("tokens: prompt=%d completion=%d total=%d\n",
		res.Usage.PromptTokens, res.Usage.CompletionTokens, res.Usage.TotalTokens)
	return exitOK
}

// printBreakers 打印各工具的熔断状态。
//
// 为什么值得打出来：熔断状态只活在内存里（它是"这个进程看到的对端健康度"），
// 出了问题时如果连它是不是打开的都看不见，就只能靠猜。
// 正常运行时全是 closed，一行为零成本；异常时会明确告诉你哪个工具被熔断了。
func printBreakers(exec *executor.Executor) {
	if exec == nil {
		return
	}
	states := exec.Breakers().States()
	if len(states) == 0 {
		return
	}
	names := make([]string, 0, len(states))
	for name := range states {
		names = append(names, name)
	}
	sort.Strings(names)

	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+"="+states[name])
	}
	fmt.Printf("熔断器: %s\n", strings.Join(parts, "  "))
}

// printOrchestratorEvent 把循环时间线打到终端。
// 这是"Agent 到底做了什么"最直观的呈现，也是 D13 演示 GIF 的素材来源。
func printOrchestratorEvent(ev orchestrator.Event) {
	switch ev.Kind {
	case "assistant":
		if strings.TrimSpace(ev.Text) != "" {
			fmt.Printf("[轮次 %d] 模型：%s\n", ev.Round, truncateRunes(ev.Text, 200))
		}
	case "tool":
		if ev.Trace == nil {
			return
		}
		status := "成功"
		switch {
		case ev.Trace.Rejected:
			status = "未执行"
		case ev.Trace.Error != "":
			status = "失败"
		}
		fmt.Printf("[轮次 %d] 工具 %s %s：%s（耗时 %s）\n",
			ev.Round, ev.Trace.ToolName, status, ev.Trace.Summary, ev.Trace.Duration.Round(time.Millisecond))
		if ev.Trace.Error != "" {
			fmt.Printf("          错误：%s\n", truncateRunes(ev.Trace.Error, 160))
		}
	case "context":
		fmt.Printf("[轮次 %d] %s\n", ev.Round, ev.Text)
	case "limit":
		fmt.Printf("[轮次 %d] 已达上限：%s\n", ev.Round, ev.Text)
	}
}

// runToolProbe 是 D2 的验收入口：让模型在**真实工具清单**下选一次工具，并真的执行它。
//
// 它是单轮的——D3 会把它扩成多轮 ReAct 循环（含轮次与工具调用双上限）。
// 这里刻意只执行 TierHarmless 工具：需要审批的操作必须走 D6 的策略引擎与工单，
// 绝不能在探针里被"顺手执行"。
func runToolProbe(ctx context.Context, cfg config.Config, question string, timeoutOverride time.Duration, showRaw bool) int {
	if err := cfg.DB.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "配置错误: %v\n", err)
		return exitUsage
	}
	if timeoutOverride > 0 {
		cfg.LLM.Timeout = timeoutOverride
	}
	client, err := newLLMClient(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "配置错误: %v\n", err)
		return exitUsage
	}

	st, err := openStore(ctx, cfg)
	if err != nil {
		slog.Error("连接数据库失败", "err", err)
		return exitFailure
	}
	defer st.Close()

	reg, err := tools.NewDefaultRegistry(tools.NewMySQLDataSource(st.GORM()))
	if err != nil {
		slog.Error("注册内置工具失败（启动门禁）", "err", err)
		return exitFailure
	}
	slog.Debug("调用模型（带工具）", "tools", reg.Names(), "model", client.Model())

	callCtx, cancel := context.WithTimeout(ctx, cfg.LLM.Timeout)
	defer cancel()

	resp, err := client.Chat(callCtx, llm.ChatRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: question}},
		Tools:    reg.Schemas(),
	})
	if err != nil {
		var apiErr *llm.APIError
		if errors.As(err, &apiErr) {
			slog.Error("模型调用失败", "status", apiErr.Status, "code", apiErr.Code, "retryable", apiErr.Retryable(), "err", apiErr.Message)
		} else {
			slog.Error("模型调用失败", "err", err)
		}
		return exitFailure
	}

	if !resp.HasToolCalls() {
		fmt.Println("--- 模型没有选择工具 ---")
		if strings.TrimSpace(resp.Message.Content) != "" {
			fmt.Println(resp.Message.Content)
		}
		fmt.Printf("(finish_reason=%s；如果这里本该调用工具，先检查工具描述是否写清楚了)\n", resp.FinishReason)
		printMeta(resp)
		return exitOK
	}

	for i, call := range resp.Message.ToolCalls {
		fmt.Printf("--- 第 %d 个工具调用 ---\n", i+1)
		fmt.Printf("tool=%s\nargs=%s\n", call.Function.Name, call.Function.Arguments)

		tool, ok := reg.Get(call.Function.Name)
		if !ok {
			// 模型编了一个不存在的工具名：这是幻觉，必须显式记录下来，而不是静默跳过。
			fmt.Printf("结果: 模型请求了未注册的工具 %q（幻觉），已拒绝执行\n", call.Function.Name)
			continue
		}
		spec := tool.Spec()
		if spec.Tier != tools.TierHarmless {
			fmt.Printf("结果: 工具 %s 的等级是 %s，单轮探针只执行无害工具；需要审批的操作应由策略引擎拦截（D6）\n",
				spec.Name, spec.Tier)
			continue
		}

		toolCtx, toolCancel := context.WithTimeout(ctx, spec.Timeout)
		res, err := tool.Invoke(toolCtx, json.RawMessage(call.Function.Arguments))
		toolCancel()
		if err != nil {
			if tools.IsArgError(err) {
				fmt.Printf("结果: 参数不合法（D3 会把这条回喂模型让它自我修正）: %v\n", err)
			} else {
				fmt.Printf("结果: 执行失败: %v\n", err)
			}
			continue
		}
		payload, _ := json.MarshalIndent(res.Data, "", "  ")
		fmt.Printf("摘要: %s\n", res.Summary)
		fmt.Printf("回喂给模型的内容:\n%s\n", payload)
	}

	printMeta(resp)
	if showRaw && len(resp.Raw) > 0 {
		fmt.Println("--- 原始响应 ---")
		fmt.Println(string(resp.Raw))
	}
	return exitOK
}

func printMeta(resp llm.ChatResponse) {
	fmt.Println("--- 元信息 ---")
	fmt.Printf("provider=%s model=%s finish=%s latency=%s\n",
		resp.Provider, resp.Model, resp.FinishReason, resp.Latency.Round(time.Millisecond))
	fmt.Printf("tokens: prompt=%d completion=%d total=%d cache_hit=%d reasoning=%d\n",
		resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens,
		resp.Usage.CacheHitTokens, resp.Usage.ReasoningTokens)
	if resp.RequestID != "" {
		fmt.Printf("request_id=%s\n", resp.RequestID)
	}
}

func printResponse(resp llm.ChatResponse, showRaw bool) {
	fmt.Println("--- 模型返回 ---")
	switch {
	case strings.TrimSpace(resp.Message.Content) != "":
		fmt.Println(resp.Message.Content)
	case len(resp.Message.ToolCalls) == 0:
		fmt.Println("(空响应：既没有文本也没有工具调用)")
	}

	if len(resp.Message.ToolCalls) > 0 {
		fmt.Println("--- 工具调用 ---")
		for i, tc := range resp.Message.ToolCalls {
			fmt.Printf("%d. %s(%s)  call_id=%s\n", i+1, tc.Function.Name, tc.Function.Arguments, tc.ID)
		}
	}

	if resp.Reasoning != "" {
		// 思维链不进入对话历史（避免污染上下文与重复计费），这里只是让人看见它存在。
		fmt.Printf("--- 思维链（%d 字，未并入上下文）---\n%s\n", len([]rune(resp.Reasoning)), truncateRunes(resp.Reasoning, 600))
	}

	printMeta(resp)

	if showRaw && len(resp.Raw) > 0 {
		fmt.Println("--- 原始响应 ---")
		fmt.Println(string(resp.Raw))
	}
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…(已截断)"
}
