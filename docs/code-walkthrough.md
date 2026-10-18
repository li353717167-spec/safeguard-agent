# 代码走读：跟着一次 `-ask` 把代码读完

> 这份文档和 [`reading-guide.md`](reading-guide.md) 是互补的：
> **reading-guide** 回答"按什么顺序读、读完要能说出什么"；**本文件**回答"这段代码在整条链路的哪个位置、为什么这么写"。
>
> 覆盖范围：D1（骨架 · 模型抽象层 · 建表迁移）+ D2（工具层）+ D3（ReAct 主循环）。
> 行号基于当前代码；**函数名是稳定锚点**，行号漂了就按函数名搜。

---

## 0. 先看全景

### 0.1 依赖方向（谁 import 谁）

```
                cmd/agent  (装配 + CLI)
                     │
      ┌──────────────┼───────────────┬──────────────┐
      ▼              ▼               ▼              ▼
 orchestrator ──▶ policy / approval          executor ──▶ tools          llm          store
      │              │             │                          ▲
      └──────────────┴─────────────┴──────────────────────────┘
         （都依赖 store 的模型与仓储接口）
```

只有 7 个包，依赖是单向的：**没有人 import `cmd/agent`，`llm` 谁都不 import。**

### 0.2 "谁不知道谁"才是设计重点

| 包 | 它**不知道**什么 | 为什么这件事重要 |
|---|---|---|
| `llm` | 不知道有"工具执行""审批""任务"这些概念 | 所以换模型不影响业务；模型侧的错误只有"传输层"一种含义 |
| `tools` | 不知道数据来自 MySQL（只认 `DataSource` 接口） | 所以评测（D11）能塞假数据源，工具代码一行不改 |
| `tools` | 不知道"自己要不要审批"这件事会被谁用 | 审批标记只在 `Spec` 里，**永远不进给模型的 schema** |
| `orchestrator` | 不知道 GORM、不知道 SQL | 它只认 `StateStore` 接口（`Create` / `UpdateProgress` / `AppendStep`） |
| `executor` | 不知道有"任务""轮次""模型" | 它只认 `tools.Tool` + `ErrClass`，可以脱离 Agent 单独使用 |
| `store` | 不知道"任务"在业务上意味着什么 | 它只负责把状态老实存下来、把 schema 对齐 |
| `cmd/agent` | 什么都不实现 | 只做装配与打印——**入口里没有一行业务判断** |

一句话记法：**越靠里的包，知道的越少；知道的越少，越不容易被改坏。**

---

## 1. 一次 `-ask` 的完整生命周期

命令：`go run ./cmd/agent -ask "checkout-service 最近是不是变慢了？"`

| # | 位置 | 做什么 | 注意点 |
|---|---|---|---|
| 1 | `main.go:46` `main` → `main.go:50` `realMain` | 解析参数，得到要执行哪个动作 | 退出码约定：0 成功 / 1 运行失败 / 2 用法或配置错误 |
| 2 | `config/dotenv.go:22` `LoadDotEnv` | 读 `.env` 注入进程环境 | **真实环境变量优先**，`.env` 不覆盖 |
| 3 | `config/config.go:45` `Load` | 只解析"形状"（时长、整数、字符串） | 必需项交给使用方 `Validate`：`-migrate` 不需要模型 key |
| 4 | `main.go:285` `newLLMClient` | 构造模型客户端 | 这里只认 `llm.Client` 接口 |
| 5 | `main.go:133` `openStore` → `store/store.go:75` `Open` | 归一化 DSN → `gorm.Open` → 连接池 → Ping | DSN 的 TLS/UTC/parseTime 在这里被**强制**，不靠环境写对 |
| 6 | `main.go:353` `runAsk` → `tools/readonly.go:271` `NewDefaultRegistry` | 注册三个只读工具 | 注册即门禁（`registry.go:44`），写错直接起不来 |
| 7 | `orchestrator/orchestrator.go:127` `New` → `:151` `Run` | 进入 ReAct 循环 | 上限默认 8 轮 / 20 次调用 |
| 8 | `orchestrator.go:246` `chat` → `:251` `chatWith` → `llm/openai_compat.go:92` `Chat` | 调一次模型，带上三个工具的 schema | 每轮都注入 schema；单次调用有超时 |
| 9 | `openai_compat.go:328-394` `normalize*` | 把厂商的响应归一化成 `llm` 的类型 | **厂商差异全部死在这里**，业务层看不到 |
| 10 | 回到 `Run` 主循环 | `resp.HasToolCalls()` 判断要不要执行工具 | 判据是 `tool_calls` 而不是 `finish_reason` |
| 11 | `orchestrator.go:270` `handleToolCall` | 四层判定：预算 → 存在 → 权限 → 执行 | 顺序不能换 |
| 12 | `orchestrator.go:301` → `executor/execute.go:78` `Execute` | 执行器：熔断判定 → 带超时执行 → 分类 → 退避重试 | 只有可重试失败才重试，且非幂等工具永不重试 |
| 12 | `tools/spec.go:129` `typedTool.Invoke` → `tools/args.go:16` `decodeArgs` → `Validate` | 严格解码 + 逐工具校验 | 未知字段直接拒绝，不静默忽略 |
| 13 | `tools/mysqlsource.go:77` `QueryMetrics` | GORM 查 MySQL，聚合成 `MetricSummary` | 「服务不存在」返回 `NotFoundError`，由工具层转成 **成功的** `found=false` |
| 14 | `orchestrator.go:368` `recordStep` | 一次工具调用 = 一行 `task_steps` | 落库失败 → 本次运行失败（fail loud） |
| 15 | 回到第 8 步 | 把 tool 结果作为 `role=tool` 消息压进历史，进入下一轮 | `tool_call_id` 必须与请求配对 |
| 16 | 模型不再请求工具时 | `run` 正常结束：`UpdateProgress(success)` | 若撞上限 → `:409 finishWithSummary`（**不报错**） |
| 17 | `main.go:413` `printOrchestratorEvent` | 实时打印轨迹 | 这就是演示 GIF 的素材来源 |

---

## 2. 逐包精读

### 2.1 `internal/config`（221 行）

| 函数 | 行 | 作用 |
|---|---|---|
| `Load` | config.go:45 | 环境变量 → `Config`，0 值走默认 |
| `LLM.Validate` / `DB.Validate` | :84 / :95 | **分开校验**：`-migrate` 不需要模型 key，`-prompt` 不需要数据库 |
| `LoadDotEnv` | dotenv.go:22 | 自己实现的 30 行 `.env` 读取器 |
| `parseDotEnvLine` | dotenv.go:51 | 引号 / `export` / 行尾注释的处理 |

**必须看懂的一点**：加载与校验分离。强制要求用不到的配置项，是本地开发最烦人的摩擦来源。
**怎么讲清楚**：*"配置只做形状解析，必需项由使用方校验——因为跑迁移不需要模型 key。"*

### 2.2 `internal/llm`（584 行）——模型抽象层

| 类型/函数 | 行 | 作用 |
|---|---|---|
| `Client` 接口 | client.go:16 | **只有 `Chat`**。多一个方法就多一处要所有厂商适配的负担 |
| `APIError` | client.go:30 | 类型化错误：`Status/Code/Type/Param/RetryAfter` + 底层 `Err` |
| `APIError.Unwrap` | client.go:69 | 让 `errors.Is(err, context.DeadlineExceeded)` 依然生效 |
| `APIError.Retryable` | client.go:76 | **只判传输层**（429/5xx/网络），业务语义留给执行器 |
| `Message` | schema.go:33 | 一个结构承载三种形态：文本 / 工具调用 / 工具结果 |
| `FunctionCall.Arguments` | schema.go:55 | **是 string，不是对象**——参数是不可信输入 |
| `ChatResponse.HasToolCalls` | schema.go:127 | 判据是 `tool_calls`，因为有的厂商多轮工具调用时仍返回 `stop` |
| `Chat` | openai_compat.go:92 | 主线：构造请求 → 处理非 2xx → 处理"200 包业务错误" → 归一化 |
| `normalizeToolCalls` / `normalizeArguments` / `normalizeUsage` / `normalizeFinishReason` | :328 / :355 / :376 / :394 | **整个适配层的存在意义就是这 60 行** |

**三处真实差异**（都有测试）：
1. 参数既可能是 JSON 字符串，也可能被网关展开成对象；
2. usage 的缓存命中：DeepSeek 直接给 `prompt_cache_hit_tokens`，OpenAI 藏在 `prompt_tokens_details.cached_tokens`；
3. 错误信封的 `code` 有时是字符串有时是数字；HTTP 200 也可能包着业务错误。

**怎么讲清楚**：*"我把思维链单独接出来，不并入下一轮上下文——否则污染上下文还重复计费。"*

### 2.3 `internal/store`（961 行）——持久化与状态机

| 文件 | 关键函数 | 作用 |
|---|---|---|
| `store.go` | `Open` :75 / `gormOptions` :117 / `utcNow` :135 / `normalizeDSN` | 装配 GORM + 连接池；**把 ORM 行为钉死在项目的不变量上** |
| `migrate.go` | `LoadMigrations` :46 / `Migrate` :89 / `loadApplied` :182 | 版本化迁移：embed + checksum + `GET_LOCK` 会话锁 |
| `models.go` | 8 张表的模型 :67 起 | 只做数据访问；**schema 的唯一事实源是 migrations/*.sql** |
| `seed.go` | `SeedDemoCatalog` :160 / `CatalogMetricNames` :30 | 可重复灌入示例数据，指标时间窗相对当前时刻生成 |
| `taskrepo.go` | `Create` :24 / `UpdateProgress` :36 / `AppendStep` :59 | 任务状态机仓储；**编排层不直接碰 GORM** |
| `toolcallrepo.go` | `BeginIntent` / `RecordResult` | `tool_calls` 仓储：**三段式落库**（1062 判重、map 更新不跳零值） |
| `approvalrepo.go` | `Create` / `Decide` / `ExpireOverdue` | 工单仓储：idem_key 唯一、只在 pending 上做决定 |
| `catalogrepo.go` | `ApplyConfig` / `SetServiceStatus` | 写操作下游：**先插台账再改配置**（下游幂等） |

**四个必须看懂的点**：
1. `normalizeDSN` 不做 `ParseDSN → FormatDSN` 往返（密码里的 `@` 会被转义坏），并强制 `tls=preferred`（MySQL 8 的 `caching_sha2_password` 在明文连接下可能要不到 RSA 公钥）；
2. `gormOptions`：`NowFunc` 钉 UTC、不用软删除、日志接 slog 且**参数脱敏**；
3. `Migrate`：MySQL 的 DDL 隐式提交、事务包不住 → 只能靠幂等 DDL + checksum 拦住"偷偷改内容"；
4. `UpdateProgress` 用 `Select` 显式列：GORM 的 `Updates` **默认跳过零值**，而 `current_step=0` 是合法值。

### 2.4 `internal/tools`（778 行）——工具层

| 类型/函数 | 行 | 作用 |
|---|---|---|
| `Spec` | spec.go:51 | 工具定义：名字/描述/schema + **权限与执行策略** |
| `Tool` 接口 / `typedTool[A]` | spec.go:81 / :117 / `New` :123 | 把接口与具体参数类型绑起来，解码校验只写一次 |
| `ArgError` | spec.go:101 | "参数不合法"——**会被回喂模型**的独立错误类型 |
| `Registry.Register` | registry.go:44 | **启动门禁**：名字/描述/等级与审批一致性/非幂等不许重试 |
| `Registry.Schemas` | registry.go:112 | **只导出 name/description/parameters**；按名排序保证 prompt 稳定 |
| `decodeArgs` | args.go:16 | 拒绝非对象 / 未知字段 / 多余内容 |
| `DataSource` 接口 | datasource.go:15 | 定义在**消费方**；实现是 MySQL，评测可换假实现 |
| `NotFoundError` / `IsNotFound` | datasource.go:59 / :73 | "查不到"是**正常结果**，不是故障 |
| `ensureService` | mysqlsource.go:66 | 区分"服务不存在"与"服务没有数据" |
| 三个工具 | readonly.go:59 / :145 / :213 | `query_metrics` / `query_config` / `list_services` |

**三个必须看懂的点**：
1. 描述是 prompt，注册时强制长度下限——**把"写得好不好"变成启动期断言**；
2. 权限字段不进 schema：模型知道"能做什么"，不该知道"要不要审批"；
3. 查不到返回**成功结果**（`found=false` + hint）——抛错误等于给模型一个失败信号，它更可能去编数字。

### 2.5 `internal/executor`（552 行）——执行器（重试与熔断）

| 类型/函数 | 行 | 作用 |
|---|---|---|
| `ErrClass` / `Classify` | errclass.go:30 / :99 | 四分类，判据是**错误类型**不是文案 |
| `CircuitOpenError` | errclass.go:61 | 熔断快速失败的错误（含还要等多久） |
| `RetryPolicy.delay` | retry.go:44 | 100→200→400ms + 抖动（上限为基础延迟的一半） |
| `sleepCtx` | retry.go:66 | **可取消**的退避等待（不用 `time.Sleep`） |
| `Breaker` | circuit.go:66 | 单个工具的熔断器：`Allow` / `OnSuccess` / `OnRetryableFailure` / `OnNeutral` |
| `BreakerSet` | circuit.go:196 | 按工具隔离熔断器 |
| `Executor.Execute` | execute.go:78 | 熔断判定 → 执行（带超时）→ 分类 → 退避重试 |
| `CanonicalJSON` | canonical.go:40 | 参数规范化：键排序 / 数字归一（big.Rat）/ 数组保序 / 深度 32 |
| `BuildIdempotencyKey` | idem.go:32 | 幂等 key：五项 + 规范化参数 + 长度前缀，取 sha256 前 32 位 |
| `PersistedExecutor.Run` | persisted.go:57 | **三段式**：写意图 → 调下游 → 写结果；success 回放 / started 重跑 |

**五个必须看懂的点**：
1. **只有可重试失败计入熔断**：参数错误是模型的错，算进去会让模型反复发错参数时把工具熔断掉；
2. **兜底是 `fatal`**（fail closed）：不认识的错误不重试，比"把确定性失败重试三遍"安全；
3. **`OnNeutral`** 只做一件事——把半开占用的试探名额还回去，否则一次参数错误就让半开永久卡死；
4. **先问熔断器再打下游**：下游已确认不可用时，连第一次请求都不该发出去。
5. **三段式里"重跑"为什么安全**：幂等 key 是确定性的，所以同一个 key 再执行一次就是"重试同一次操作"，而不是"重复一次操作"。

### 2.6 `internal/policy` + `internal/approval`（249 行）——策略与挂起

| 类型/函数 | 行 | 作用 |
|---|---|---|
| `Engine.NeedsApproval` | policy.go:44 | 三条纯规则，**签名里没有 args** |
| `Gate.Park` | gate.go:78 | 先写意图 → 再建工单；`Created` / `AlreadyDone` 两种返回 |
| `Gate.Decide` | gate.go:135 | pending → approved/rejected，其它状态拒绝 |
| `update_config` | write.go:64 | dry-run 默认、`confirm` 令牌、缺幂等 key 直接拒 |

**两个必须看懂的点**：
1. **权限是确定性的**：判定只看工具属性；同样的操作，今天和明天结论一致。
2. **先落库再挂起**：意图（"停在哪个调用"）+ 工单（"等谁批"）都进库之后，才结束本次运行。

### 2.7 `internal/orchestrator`（610 行）——编排层（核心）

| 类型/函数 | 行 | 作用 |
|---|---|---|
| `StateStore` 接口 | orchestrator.go:41 | 编排层需要的三个持久化动作 |
| `Limits` / `DefaultLimits` | :48 / :56 | 双上限 8 轮 / 20 次 |
| `StopReason` | :59 | `answered` / `max_rounds` / `max_tool_calls` |
| `ToolTrace` / `Event` / `Result` | :71 / :85 / :93 | 轨迹（含 `Class`/`Attempts`）、实时事件、最终结果 |
| `Orchestrator.Run` | :151 | **主循环** |
| `run.chat` / `chatWith` | :246 / :251 | 单次模型调用 + 超时 + 用量累计 |
| `run.handleToolCall` | :270 | 四层判定 + 调执行器 + 按分类回喂 |
| `run.recordStep` | :390 | 落一行 `task_steps`（含 class/attempts） |
| `run.finishWithSummary` | :420 | 撞上限：再调一次模型（**不带 tools**）生成总结 |
| `toolError` | :490 | 构造"工具调用失败"的回喂消息 |
| `loopGuard` | guard.go:11 起 | 双上限的计数与判定 |

**四层判定的顺序不能换**（`handleToolCall`）：预算 → 工具存在 → 权限 → 执行。
**三个最容易做错的点**（见 design.md §12.3）：
1. 撞上限要**返回结果而不是报错**；
2. 被拒绝的调用**仍然要补一条 tool 结果**，否则消息序列残缺、端点直接拒绝整个请求；
3. `bad_args` / `upstream_unavailable` / `system_error` / `circuit_open` 要分开标注，否则模型会去反复改一个它改不了的参数。

### 2.8 `cmd/agent`（约 700 行）——装配

| 函数 | 行 | 作用 |
|---|---|---|
| `realMain` | main.go:50 | 解析参数 → 装配置/日志 → 分发动作 |
| `openStore` | :133 | 配置 → `store.Config`（Debug 跟随日志级别） |
| `runAsk` | :353 | **D3 主入口**：装配 registry/executor/orchestrator 并跑循环 |
| `printOrchestratorEvent` | :413 | 轨迹实时打印 |
| `runToolProbe` | :445 | 单轮工具探针（验证工具选择） |
| `runListTools` / `runSeed` / `runMigrate` / `runDBPing` | :298 / :323 / :144 / :177 | 其余动作 |

**注意**：这里全是"接线 + 打印"，没有业务判断。**450 行以上的入口是个信号**——D6/D7 再加命令时应该拆成 `internal/cli`。

---

## 3. 三条主线在代码里的落点

| 主线 | 落点 | 一句话 |
|---|---|---|
| **幂等与状态** | `tool_calls` 主键 = 幂等 key；`executor/persisted.go` 三段式；`canonical.go` + `idem.go` 确定性 key；`taskrepo.AppendStep` | "这个操作发出去过没有"由主键回答，不靠查日志 |
| **审计与状态分离** | `audit_logs` 无外键、`tool_calls` 有级联删除 | 审计回答"谁做了什么"，状态回答"做到哪了" |
| **厂商差异收敛** | `openai_compat.go` 的四个 `normalize*` + `APIError` | 业务层永远看不到厂商字段；错误分类靠类型不靠文案 |
| **重试 vs 熔断** | `executor/errclass.go` 四分类；`circuit.go` 熔断器 | 重试解决单次抖动，熔断解决下游整体不可用；**只有可重试失败计入熔断** |
| **权限是工具的属性** | `tools.Spec.Tier/RequireApproval`；`Registry.Schemas`（不导出）；`orchestrator.handleToolCall` 第③层 | 模型连审批标记都看不到 |
| **模型做决策、不做执行** | `MaxToolCalls` + 工具内部把批量操作封装 | 上限本身就是"这个任务该不该用 Agent"的信号 |

---

## 4. 19 道自测题 + 答题要点

**1. `tool_calls` 主键为什么是 `idem_key` 而不是自增 id？**
→ 让"这个操作到底发出去过没有"变成**一次主键查询**；靠查审计日志反推进度很脆弱。

**2. `audit_logs` 为什么不建外键？级联删除为什么只作用于执行状态？**
→ 审计要能记录"任务都不存在时的操作"，且任务被删不能抹掉痕迹；执行状态可从状态机重建，所以可以级联删。

**3. 幂等 key 该由哪些字段构成？为什么参数必须先规范化？**
→ `(task_id, step_no, call_index, tool_name, canonical(args))`。map 键序、数字格式不统一就会算出不同 key，幂等直接失效。

**4. 状态列为什么用 `VARCHAR + CHECK` 而不是整数枚举？**
→ 可读（出事故直接 SELECT 就懂）+ 非法状态写不进库。代价是改枚举要发迁移。

**5. 迁移为什么不用 goose？checksum 拦的是什么？**
→ 需求只有三条（版本序 / 幂等 / 改过要报错），自己写 190 行能讲清每一步。checksum 拦的是"已应用的迁移被改动"——宁可启动失败，也不要 schema 静默漂移。

**6. `APIError.Retryable()` 为什么只判 429/5xx/网络，不判业务语义？**
→ 同一个 400 在"模型调用"和"重启服务"里含义完全不同；分类属于执行器（D4）。

**7. 换一个模型要改哪些代码？**
→ 只改 `.env` 的 `SAFEGUARD_LLM_BASE_URL` / `_MODEL` / `_API_KEY`。业务代码零改动。

**8. `reasoning_content` 为什么不能并入下一轮对话历史？**
→ 污染上下文（模型会把自己的旧思考当事实）且重复计费。

**9. 既然用了 GORM，为什么还要自己写 SQL 迁移？`AutoMigrate` 差在哪？**
→ 它表达不了 `CHECK` 约束与索引注释，没有版本与 checksum，而且是**静默 ALTER**——与我们"宁可启动失败也不要漂移"正好相反。

**10. GORM 的 `Updates` 有什么默认行为会坑到状态机？**
→ 默认**跳过零值字段**（`current_step=0`、`retry_count=0` 会被漏掉）。规避：`Select` 显式列出要写的列。

**11. 模型能从工具 schema 里看出"这个工具要不要审批"吗？为什么？**
→ 不能。`Schemas()` 只导出 name/description/parameters。把审批线索交给概率模型 = 自废权限体系。

**12. 模型给了不存在的参数名，工具怎么处理？为什么不能静默忽略？**
→ 拒绝（`DisallowUnknownFields`）并作为 `bad_args` 回喂。静默忽略会让模型以为参数生效了，拿同一个错参数反复重试。

**13. 为什么"服务不存在"要返回成功结果而不是错误？**
→ 抛错误等于给模型一个失败信号，它更可能去编一个像样的数字。返回 `found=false` + hint，它才有据可依地说"查不到"。

**14. 上限为什么要有两个？只设一个漏掉什么？**
→ 轮次拦"来回空转烧 token"，工具调用数拦"一轮并发几十个调用打垮下游"。只设一个必然漏另一种。

**15. 撞上限时，被拒绝的调用为什么仍必须回一条 tool 结果？**
→ 消息序列里"有 `tool_calls` 却没有结果"会被 OpenAI 兼容端点**直接拒绝整个请求**。

**16. 参数错误和系统错误都回喂，为什么必须标注不同 `kind`？**
→ 标注错类别，模型会去反复改一个它改不了的参数（系统故障），白烧轮次。

**17. 为什么只有"可重试失败"才计入熔断？**
→ 参数错误是**模型的错**，不是下游的健康度。算进去会让模型反复发错参数时把工具熔断掉——把模型的错误算到下游头上。

**18. `Canceled` 和 `DeadlineExceeded` 为什么一个不可重试、一个可重试？**
→ 前者表示"调用方已经不要结果了"，重试只会在一个没人要的任务上继续产生副作用；后者是"这次等太久了"，下次可能就成功。

**19. 熔断状态为什么放内存？代价是什么？**
→ 它是"**这个进程当前看到的**对端健康度"，属于运行期信号；放库里等于在热路径上给每个请求加一次读写。代价：多实例之间不共享熔断判断。

---

## 5. 读完之后怎么验证自己懂了

```bash
# 1. 先跑一遍，建立"行为印象"
go run ./cmd/agent -tools
go run ./cmd/agent -tool-probe "payment-service 最近 5 分钟的错误率怎么样"
go run ./cmd/agent -ask "checkout-service 最近是不是变慢了？"
go run ./cmd/agent -ask "把 checkout-service 的指标和配置都查一遍" -max-tool-calls 2

# 2. 用调试级别看 SQL 与调用轨迹
$env:SAFEGUARD_LOG_LEVEL="debug"; go run ./cmd/agent -ask "payment-service 的错误率？"

# 3. 挑一个"不该发生"的行为，看是不是被测试挡住了
go test ./internal/tools -run TestRegisterGate -v          # 11 类工具定义错误
go test ./internal/tools -run TestSchemasDoNotLeakPolicy -v # 权限不进 prompt
go test ./internal/orchestrator -run TestMaxToolCalls -v    # 消息序列不残缺
go test ./internal/store -run TestMigrationKeeps -v         # DDL 幂等与幂等主键

# 4. 直连数据库看状态机（不信程序自报）
#    SELECT task_id,status,current_step,total_rounds FROM tasks ORDER BY created_at DESC LIMIT 3;
#    SELECT step_no,round_no,status,plan FROM task_steps WHERE task_id='...' ORDER BY step_no;
```

**如果这些你都能解释清楚、并且能说出"为什么不是另一种做法"，D1–D4 就通了。**
