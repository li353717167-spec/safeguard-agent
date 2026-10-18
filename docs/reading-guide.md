# 代码阅读指南

> 目的：把代码读成**一张图 + 三条主线**，而不是按文件顺序硬啃。
>
> 当前进度：**D1–D7 已完成**（骨架/迁移 → 工具层 → ReAct 主循环 → 执行器 → 幂等 → 审批 → 断点恢复），后续是评测与文档打磨。
>
> 📄 配套文档：[`code-walkthrough.md`](code-walkthrough.md) —— 跟着一次 `-ask` 把整条链路读完，
> 含每个包的函数级地图与 19 道自测题的**答题要点**。本文件负责"按什么顺序读"，它负责"这段代码在哪、为什么这么写"。

---

## 0. 先看清"有什么、没什么"

| 文件 | 行数 | 一句话职责 |
|---|---|---|
| `internal/store/migrations/0001_init.sql` | 112 | **全项目最重要的文件**：5 张表定义了两条主线（状态机 / 审计） |
| `internal/store/migrations/0002_catalog.sql` | 52 | 服务目录三张表（只读工具的数据源） |
| `internal/store/store.go` | 171 | 装配：GORM + 连接池、强制安全参数、掩码 DSN、把 ORM 的时钟钉在 UTC |
| `internal/store/models.go` | 169 | 8 张表的 GORM 模型 + 状态常量（**只做数据访问，schema 归 SQL 迁移**） |
| `internal/store/migrate.go` | 184 | 迁移器：embed + 版本序 + checksum 校验 + `GET_LOCK` 会话锁 |
| `internal/store/seed.go` | 200 | 示例服务目录：可重复灌入，指标时间窗相对当前时刻生成 |
| `internal/tools/spec.go` | 108 | 工具定义（含权限与执行策略）+ 强类型工具包装 + 参数错误类型 |
| `internal/tools/registry.go` | 138 | 注册中心 + **启动门禁** + `Schemas()`（只给模型看该看的） |
| `internal/tools/args.go` | 49 | 严格参数解码：拒绝未知字段、拒绝非对象、拒绝多余内容 |
| `internal/tools/datasource.go` | 63 | 数据源接口（定义在消费方）+ not_found 的语义 |
| `internal/tools/mysqlsource.go` | 130 | MySQL 实现：区分"服务不存在"与"没有数据" |
| `internal/tools/readonly.go` | 290 | 三个只读工具 + 参数校验 + 注册组装 |
| `internal/orchestrator/orchestrator.go` | 434 | **ReAct 主循环**：模型决策 → 工具执行 → 结果回喂 + 撞上限转总结 |
| `internal/orchestrator/guard.go` | 49 | 双上限（轮次 / 工具调用） |
| `internal/executor/errclass.go` | 126 | 异常四分类 `Classify` + 熔断错误类型 |
| `internal/executor/retry.go` | 76 | 退避策略（100→200→400ms + 抖动）+ 可取消等待 |
| `internal/executor/circuit.go` | 226 | 熔断器：60s/5 次 → 打开，30s → 半开放 1 个试探 |
| `internal/executor/execute.go` | 124 | 执行器：熔断判定 → 执行 → 分类 → 重试 |
| `internal/executor/canonical.go` | 117 | 参数规范化（键排序 / 数字归一 / 数组保序 / 深度限制） |
| `internal/executor/idem.go` | 40 | 幂等 key：确定性哈希，长度前缀防拼接歧义 |
| `internal/executor/persisted.go` | 157 | **三段式落库 + 回放**：写意图 → 调下游 → 写结果 |
| `internal/store/toolcallrepo.go` | 92 | `tool_calls` 仓储：1062 判重、写结果、按任务读轨迹 |
| `internal/store/approvalrepo.go` | 101 | 审批工单仓储：建单唯一键、只在 pending 上做决定、超时过期 |
| `internal/store/catalogrepo.go` | 155 | 服务目录**写**操作：配置变更台账（下游幂等）+ 状态更新 |
| `internal/policy/policy.go` | 81 | 策略引擎：纯规则判定是否需审批（只看工具属性） |
| `internal/approval/gate.go` | 168 | 审批门禁：**先落库再挂起**（意图 → 工单） |
| `internal/executor/approvalctx.go` | 26 | "已获批，这一次放行"的 context 信号 |
| `internal/orchestrator` 的 `Resume` | orchestrator.go:213 | **断点恢复**：从 task_steps 重建对话，在原 step 号上重放挂起调用 |
| `internal/tools/write.go` | 372 | 写/高危工具：dry-run + confirm 令牌、restart、scale |
| `internal/tools/context.go` | 26 | 幂等 key 的 context 传递（写工具强制要求） |
| `internal/store/migrations/0003_config_changes.sql` | 22 | 变更台账：下游幂等的落点 |
| `internal/store/taskrepo.go` | 73 | 任务状态机仓储（编排层不直接碰 GORM） |
| `internal/llm/schema.go` | 93 | 厂商无关的对话/工具/用量结构 |
| `internal/llm/client.go` | 86 | `Client` 接口（只有 `Chat`）+ 类型化错误 `APIError` |
| `internal/llm/openai_compat.go` | 405 | OpenAI 兼容适配：把厂商差异全部关在这一层 |
| `internal/config/*.go` | 221 | 配置装载：环境变量 + `.env`，加载与校验分离 |
| `cmd/agent/main.go` | 551 | 装配与七个动作（`-ask` / `-tool-probe` / `-tools` / `-seed` / `-migrate` / `-db-ping` / `-prompt`） |
| `*_test.go`（12 个） | 3046 | 错误分类、DSN 语义、迁移与模型契约、工具门禁、参数校验、not_found 语义、循环的每个停止条件、重试与熔断 |

> 实现代码约 4580 行，测试 3046 行——**测试占了近四成**，这不是凑数：
> 错误分类表、DSN 语义、迁移不变量、模型契约、注册门禁、参数拒绝规则、
> "撞上限时消息序列不能残缺"这类协议细节，以及**用真实驱动错误验证分类**，全靠它钉住。

**还没有的（不是遗漏，是 D2–D7 的范围）**：`orchestrator/`（ReAct 循环、双上限）、`tools/`（工具注册与实现）、`policy/`（审批规则）、`executor/`（异常分类、重试、熔断、幂等执行器）、`approval/`、`eval/`、HTTP server、任何工具实现。

---

## 1. 阅读顺序：9 遍，每遍只回答一个问题

> 不建议先读 `main.go`。它只是装配，读完不会产生"原来如此"的感觉。
> 如果习惯入口优先，可以最后再回来看它，那时你会认出每一行在接什么线。

### Pass 0 · 先跑，再读（5 分钟）

```bash
# 离线（不需要数据库与模型）
go test ./...

# 在线
go run ./cmd/agent -db-ping
go run ./cmd/agent -migrate                    # 连跑两次，第二次走 checksum 跳过
go run ./cmd/agent -seed                       # 灌/刷新示例数据（指标时间窗跟着当前时刻走）
go run ./cmd/agent -tools                      # 工具清单：tier / 审批 / 超时 / 重试
go run ./cmd/agent -tool-probe "payment-service 最近 5 分钟的错误率怎么样"
go run ./cmd/agent -tool-probe "查一下 order-service 的监控指标"   # 陷阱：该服务不存在
go run ./cmd/agent -prompt "用一句话说明什么是幂等" -raw
```

先建立"行为印象"：`-raw` 打印的原始响应里藏着厂商差异，而屏幕上半部分的输出是**归一化之后**的样子。
`-tool-probe` 则把"模型怎么选工具、工具返回什么"这两件事直接摊开给你看。

### Pass 1 · 数据模型：这两条主线是怎么落到表上的？（30 分钟）

读 `internal/store/migrations/0001_init.sql`，**按下面四步走**：

1. **看 5 张表的分工**：`tasks`（18 行，编排进度）→ `task_steps`（38 行，决策状态）→ `tool_calls`（58 行，执行状态）→ `approvals`（88 行，待办）→ `audit_logs`（115 行，审计）。
2. **盯住 73 行**：`PRIMARY KEY (idem_key)`。问自己：*如果主键是自增 id，"这个操作到底发出去过没有"要几次查询才能回答？* 再看 75 行 `UNIQUE KEY uk_tool_calls_target (task_id, step_no, call_index)`——它是"重放时跳过已成功步骤"的判定依据。
3. **对比 `tool_calls` 与 `audit_logs`**：前者在 79 行有 `FOREIGN KEY ... ON DELETE CASCADE`，后者**一个外键都没有**。为什么？因为审计要能记录"任务都不存在时的操作"，且任务被删不能抹掉痕迹。
4. **看 4 条 `CONSTRAINT chk_*_status CHECK (status IN (...))`**：非法状态要在写入时被数据库拒绝，而不是等恢复流程在凌晨三点读到它。

**读完应能说出**：状态和审计是两件事；主键即幂等 key 的价值；为什么所有 DDL 都写成 `IF NOT EXISTS`。

### Pass 2 · 持久化与迁移：工程约束是怎么落地的？（45 分钟）

读 `internal/store/store.go` → `models.go` → `migrate.go`。

- `Open`（store.go:64）：为什么连接参数（UTC、`parseTime`、TLS、关多语句）要在代码里强制，而不是靠每个环境把 DSN 写对。
- `gormOptions`：**四个被钉死的 ORM 行为**——`NowFunc` 必须 UTC、不用软删除、关键更新要显式指定列、日志接 slog 且参数脱敏。每一条都对应一类"默认行为悄悄违背设计"的事故。
- `normalizeDSN`：**这段值得逐行读**。它解释了为什么不做 `ParseDSN → FormatDSN` 往返（会把含 `@` 的密码转义坏），以及为什么默认 `tls=preferred`。
- `models.go`：先读文件顶部的边界说明——**模型只做数据访问，schema 的唯一事实源是 `migrations/*.sql`**。再注意两个细节：JSON 列为什么用 `string` 而不是 `[]byte`；可空列为什么用指针（写 `nil` 得 SQL NULL，而空字符串会被 JSON 列拒绝）。
- `Migrate`（migrate.go:89）：`GET_LOCK` 会话锁（117 行，多实例同时启动）→ 逐版本比对 checksum → 执行 → 记录。
- `loadApplied`（migrate.go:182）：已应用记录与仓库文件对不上时报告漂移。

**关键认知**：MySQL 的 DDL 会隐式提交，**事务包不住 DDL**，所以迁移不可能有原子性——只能靠"幂等 + 可重入 + checksum 拦住偷偷改内容"。这就是 112 行 SQL 里 `IF NOT EXISTS` 的真正原因。
**另一个关键认知**：ORM 解决的是映射与开发效率，不解决 schema 的版本化与可审计——所以 `AutoMigrate` 一次都不能用。

### Pass 3 · 工具层：怎么把"不可信输入"和"权限"同时管住？（40 分钟）

读 `internal/tools/spec.go` → `registry.go` → `args.go` → `datasource.go` → `mysqlsource.go` → `readonly.go`。

- `Tool` / `typedTool[A]`（spec.go）：接口与泛型怎么配合，做到"解码与校验只写一次，每个工具只管自己的业务"。
- `Registry.Register`（registry.go）：**启动门禁清单**——名字规范、描述长度下限、高危必须审批、需要审批必须有 TTL、非幂等不许自动重试。注意它是"宁可起不来"的取舍。
- `Registry.Schemas`（registry.go）：**为什么只导出 name/description/parameters**。这是"权限是工具的属性，不是模型的判断"的落地处。
- `decodeArgs`（args.go）：三条规矩对应三类真实故障（非对象 / 幻觉字段 / 多余内容）。
- `NotFoundError`（datasource.go）：**"查不到"为什么是一次成功的工具调用**，而不是错误。
- `ensureService`（mysqlsource.go）：为什么必须先确认服务存在——否则"服务不存在"和"服务没有数据"会返回同一种空结果，而这两种情况该给模型的回答完全不同。
- `readonly.go`：三个工具的 Description 不是注释，**是 prompt**。对照 `-tool-probe` 的实测结果看它们写得好不好。

**读完应能说出**：模型看不到审批标记；幻觉参数在进入业务逻辑前就被拒绝；查不到不是失败。

### Pass 4 · 编排层：这个循环为什么不会失控？（45 分钟）

**这是全项目最核心的一段代码**：模型决策 → 工具执行 → 结果回喂，就是 Agent 与"会聊天的模型"的唯一区别。

读 `internal/orchestrator/orchestrator.go`（先看 `Run` 的主循环），再回头看 `guard.go`。

- `Run` 的主循环（约 160 行）：注意它每一轮的固定动作——调模型（带 schema）→ 把 assistant 消息压进历史 → 有工具调用就执行并回填 → 回到轮次开头。
- `handleToolCall` 分了四层判定，**顺序不能换**：① 预算内？② 工具存在？③ 权限允许？④ 执行（带自己的超时）。
- **残废消息序列那个坑**：预算耗尽时仍然要给每个 `tool_calls` 补一条 tool 结果。缺了它，服务端会直接拒绝整个请求——这是最容易漏的协议细节，有专门的测试 `TestMaxToolCallsNeverLeavesDanglingToolCall`。
- 两类错误的分流：`ArgError` → 回喂"请修正参数"；系统错误 → 回喂"这不是你参数的问题"。**标注错类别，模型会去反复改它改不了的参数。**
- `finishWithSummary`：撞上限时**不报错**，而是再调一次模型（**不带 tools**）生成"已知 / 未确认 / 下一步"；这次调用也失败就退化成本地摘要。
- `recordStep`：落库失败直接让本次运行失败。**宁可失败，也不要带着不完整的记录继续跑**——状态机是恢复的唯一依据。

**读完应能说出**：双上限为什么必须是两个；撞上限为什么不报错；为什么被拒绝的调用仍要有 tool 结果；为什么落库失败要 fail loud。

### Pass 5 · 执行器：重试、熔断与幂等（50 分钟）

读 `internal/executor/errclass.go` → `retry.go` → `circuit.go` → `execute.go` → `canonical.go` → `idem.go` → `persisted.go`。

- `Classify`（errclass.go）：**判据全是错误类型**（`errors.As`/`errors.Is`），没有一个字符串匹配。注意 `context.Canceled → fatal` 而 `context.DeadlineExceeded → retryable`——长得很像，含义完全不同。
- `RetryPolicy.delay`（retry.go）：100→200→400ms，抖动上限是基础延迟的一半（让抖动修饰退避，而不是主导退避）。
- `Breaker.Allow` / `OnSuccess` / `OnRetryableFailure` / `OnNeutral`（circuit.go）：为什么有第四个方法？因为半开时若试探返回"参数错误"，必须把试探名额还回去。
- `Execute`（execute.go）：顺序是 **先问熔断器再打下游**；`maxAttempts = 1 + spec.MaxRetry`，非幂等工具强制 1 次。

- `CanonicalJSON`（canonical.go）：三条规则（键排序 / 数字归一 / 数组保序），以及为什么数字必须用 `big.Rat` 而不是 `float64`。
- `BuildIdempotencyKey`（idem.go）：五个组成项、为什么不能含时间戳、长度前缀解决什么。
- `PersistedExecutor.Run`（persisted.go）：**本项目最重要的一段**——① 写意图 → ② 调下游 → ③ 写结果；命中 `success` 就回放、命中 `started` 就重跑。读的时候问自己："如果进程在 ① 和 ③ 之间被杀，会发生什么？"

**读完应能说出**：为什么只有可重试失败计入熔断；为什么兜底是 fatal 而不是 retryable；熔断状态为什么放内存；三段式里哪一段崩溃最危险、为什么"重跑"是安全的。

### Pass 6 · 策略引擎与审批挂起：权限为什么不能交给模型？（30 分钟）

读 `internal/policy/policy.go` → `internal/approval/gate.go` → `internal/tools/write.go`。

- `NeedsApproval`（policy.go）：三条规则全部来自工具静态属性。注意它的签名**没有 args**——参数解释是工具的职责，策略层不该理解业务参数。
- `Gate.Park`（gate.go）：**先写执行意图、再建工单**，顺序不能反。返回值里的 `AlreadyDone` / `Created` 分别对应"此前已成功"与"重入时复用”。
- `update_config`（write.go）：默认 dry-run；真执行要 `dry_run=false` + `confirm` 令牌；缺幂等 key 直接拒绝。
- 下游幂等（catalogrepo.go）：先按幂等 key 插台账，再改配置——顺序反了就会"改了但没记账"。

**读完应能说出**：为什么审批不能由模型决定；为什么先落库再挂起；confirm 令牌防的是什么。

### Pass 7 · 断点恢复：进程被杀之后怎么接着做？（30 分钟）

读 `orchestrator.Resume` → `rebuild` → `executor/persisted.go` 的 `waiting_approval` 分支。

- `rebuild`（orchestrator.go:246）：按 `round_no` 分组，把每个 step 还原成 assistant/tool 消息。
  注意它读的是 `plan.tool_content`——**回喂给模型的原文必须落库**，否则拼不回历史。
- `Resume`：恢复预算计数（`guard.restore`），然后用**原 step_no** 处理挂起的调用。
- 为什么必须是原 step_no：它参与幂等 key。换成新步骤号 = 变成另一个操作，非幂等工具会被执行两次。
- `ApprovalGranted`（approvalctx.go）：一次性放行信号；没有它执行器会继续 fail closed。

**读完应能说出**：为什么"不存对话"也能恢复；恢复时哪一步最容易做错、后果是什么。

### Pass 8 · 模型抽象层：厂商差异是怎么被关起来的？（40 分钟）

读 `internal/llm/schema.go` → `client.go` → `openai_compat.go`。

- `Message`（schema.go:33）：一个结构承载三种形态（文本 / 工具调用 / 工具结果），因为协议就是这么定义的。
- `FunctionCall.Arguments`（schema.go:55）：**为什么是 string 而不是解析好的对象**？因为模型返回的参数可能是非法 JSON、带幻觉字段、甚至注入构造的——校验必须发生在"即将产生副作用的那一层"（D2 的工具层）。
- `HasToolCalls()`（schema.go:127）：为什么判断依据是 `tool_calls` 而不是 `finish_reason`？（有厂商在多轮工具调用时仍返回 `stop`。）
- `Client` 接口（client.go:16）：为什么只有一个 `Chat`。
- `APIError`（client.go:30）+ `Unwrap`（69）+ `Retryable()`（76）：类型化错误；注意 `Retryable()` **只判传输层**（429/5xx/网络），不判业务语义——业务语义属于 D4 的执行器。
- `Chat`（openai_compat.go:92）：主线。注意它对非 2xx、对"200 里包着业务错误"、对空 choices 分别怎么处理。
- 四个归一化函数（328–393 行）：`normalizeToolCalls` / `normalizeArguments` / `normalizeUsage` / `normalizeFinishReason`——**整个适配层的存在意义就是这 60 行**。

**对照着读**：`openai_compat_test.go:19` 的 `toolCallBody` 是本地模型的**真实响应样本**（含 `reasoning_content`、字符串形态的参数、OpenAI 风格的缓存字段）。

### Pass 9 · 装配与配置：谁负责接线？（20 分钟）

读 `cmd/agent/main.go` → `internal/config/config.go`。

- `realMain`（main.go:39）：只做装配 + 分发，任何业务判断都不在这里。
- 三个动作 `runMigrate` / `runDBPing` / `runPrompt`（100/138/182）。
- `config.Load`（config.go:45）与 `LLM.Validate` / `DB.Validate`（84/95）**刻意分开**：`-migrate` 不需要模型 key，`-prompt` 不需要数据库。
- `LoadDotEnv`（dotenv.go:22）：为什么真实环境变量优先于 `.env`。

---

## 2. 三条主线（读的时候随时把代码挂上去）

| 主线 | 现在落在哪 | 后面会长成什么 |
|---|---|---|
| **幂等与状态** | `tool_calls` 主键 = 幂等 key；`(task_id, step_no, call_index)` 唯一 | D5 三段式落库（写意图→调下游→写结果）→ D7 断点恢复 |
| **审计与状态分离** | 两张表、一套级联、一套不级联 | D6 审批工单写审计、D7 恢复时读状态 |
| **厂商差异收敛** | 4 个 `normalize*` + `APIError` | D4 的 `Classify()` 直接 `errors.As` 到 `APIError`，不匹配字符串 |

---

## 3. 动手实验（把静态代码变成可观察行为）

```bash
# 看错误分类表（429/401/400/200-包业务错误/非 JSON 网关页）
go test ./internal/llm -run TestChatErrorClassification -v

# 看"设计契约测试"：谁改掉幂等主键或 IF NOT EXISTS，测试立刻红
go test ./internal/store -run TestMigrationKeepsIdempotencyInvariants -v

# 看工具注册门禁：11 类错误定义会在启动期被拒
go test ./internal/tools -run TestRegisterGate -v

# 看参数拒绝规则：幻觉字段 / 非对象 / 多余内容 / 枚举非法
go test ./internal/tools -run TestInvokeRejectsBadArgs -v

# 看"权限不进 prompt"这条边界
go test ./internal/tools -run TestSchemasDoNotLeakPolicy -v

# 看厂商差异：对比原始响应与归一化结果
go run ./cmd/agent -prompt "用一句话说明什么是幂等" -raw

# 看类型化错误：把端点指向死端口
$env:SAFEGUARD_LLM_BASE_URL="http://127.0.0.1:9/v1"; go run ./cmd/agent -timeout 5s -prompt "hi"

# 看迁移的 checksum 拦截：给已应用的迁移加一行注释再 -migrate
```

---

## 4. 合上代码自测

1. `tool_calls` 的主键为什么是 `idem_key` 而不是自增 id？
2. `audit_logs` 为什么不建外键？级联删除为什么只作用于执行状态？
3. 幂等 key 应该由哪些字段构成？为什么参数必须先规范化？
4. 状态列为什么用 `VARCHAR + CHECK`，而不是整数枚举？
5. 迁移为什么不用 goose / golang-migrate？checksum 校验拦住的到底是什么？
6. `APIError.Retryable()` 为什么只判 429/5xx/网络，不判业务语义？
7. 换一个模型需要改哪些代码？
8. `reasoning_content` 为什么不能并入下一轮对话历史？
9. 既然用了 GORM，为什么还要自己写 SQL 迁移？`AutoMigrate` 差在哪？
10. GORM 的 `Updates` 有个什么默认行为会坑到状态机？怎么规避？
11. 模型能不能从工具 schema 里看出"这个工具要不要审批"？为什么这样设计？
12. 模型给了一个不存在的参数名，工具会怎么处理？为什么不能静默忽略？
13. 为什么"服务不存在"要让工具返回一次成功结果，而不是抛错误？
14. 为什么上限要有两个（轮次 + 工具调用）？只设一个会漏掉什么？
15. 撞上限时，为什么"被拒绝执行"的调用仍然必须回一条 tool 结果？
16. 参数错误和系统错误都回喂给模型，但为什么必须标注不同的 `kind`？
17. 为什么只有"可重试失败"才计入熔断？把参数错误算进去会怎样？
18. `context.Canceled` 和 `context.DeadlineExceeded` 为什么一个不可重试、一个可重试？
19. 熔断状态为什么放内存而不是 MySQL？代价是什么？

20. 幂等 key 里为什么绝对不能放时间戳或 UUID？
21. 参数规范化要解决哪种"看起来不同、其实是同一个操作"的情况？
22. 库里有一条 `status=started` 但没有结果的记录，说明发生了什么？该怎么处理？

23. 为什么审批判定不能接收参数（args）？那"改动幅度超阈值也审批"该怎么实现？
24. 挂起时为什么要"先落库再挂起"？顺序反了会发生什么？
25. `update_config` 的 `confirm` 令牌防的是什么问题？它算安全机制吗？

26. 断点恢复为什么不需要单独存一份对话？靠的是什么？
27. 恢复时把挂起的调用当成"新的一步"会发生什么？为什么？
28. `-approve` 之后为什么还需要 `-resume`？两者各自负责什么？

能不看代码答出这 28 条，D1–D7 就算读透了，Week 1 也就走完了。

---

## 5. 几个别误会的地方

- `.env` 里有真实数据库凭据 —— 它已在 `.gitignore` 中；提交前确认一下。
- `.gocache/`、`.gomodcache/`、`.gotmp/` 是本机沙箱环境的权宜（默认缓存目录在工作区外被拒），可随时删除。
- `go.mod` 写 `go 1.24.0`，本机工具链是 1.27.2 —— 正常，新工具链可以构建声明较低版本的模块。
- 有些看起来"过度讲究"的代码（TLS 默认、拒绝 Parse→Format 往返）都是被真实环境打出来的，注释里写了原因，不是凭空防御。
- **`cmd/agent/main.go` 已经 450 行，且大部分是 CLI 打印逻辑**。D6/D7 会再加审批与恢复相关命令，那时应当把它拆成 `internal/cli`（每个动作一个文件），入口只留装配。现在拆还太早——动作只有六个。
