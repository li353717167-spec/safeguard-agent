# 架构：三张图看懂 safeguard-agent

> 这个项目不是"又一个 Agent demo"。它要回答的是**上线前那些绕不开的问题**：
> 高危操作谁来批、进程挂了任务丢不丢、重试会不会把线上改两遍、评测能不能复现。
>
> 三张图分别是：**分层**（谁依赖谁）、**一次调用的控制流**（权限在哪一步生效）、
> **状态机**（挂起与恢复怎么闭环）。

---

## 图 1 · 分层与依赖方向

```mermaid
flowchart TB
    subgraph ENTRY["入口层 · cmd/agent"]
        CLI["-ask / -resume<br/>-approve / -reject<br/>-eval / -tools / -migrate"]
    end

    subgraph ORCH["编排层 · internal/orchestrator"]
        LOOP["ReAct 主循环<br/>双上限 · 消息序列完整"]
        RESUME["Resume<br/>从状态机重建对话"]
    end

    subgraph GOV["治理层 · policy / approval"]
        POLICY["policy.Engine<br/>纯规则判定"]
        GATE["approval.Gate<br/>先落库再挂起"]
    end

    subgraph EXEC["执行层 · internal/executor"]
        CLS["Classify<br/>四分类"]
        RETRY["RetryPolicy<br/>退避 + 抖动"]
        BRK["Breaker<br/>熔断 + 半开"]
        PERS["PersistedExecutor<br/>三段式 + 回放"]
        IDEM["CanonicalJSON<br/>+ 幂等 key"]
    end

    subgraph TOOL["工具层 · internal/tools"]
        REG["Registry<br/>注册门禁"]
        READ["只读工具 ×3"]
        WRITE["写/高危工具 ×3"]
        DS[("DataSource<br/>接口")]
    end

    subgraph INFRA["基础设施"]
        LLM["internal/llm<br/>厂商适配"]
        STORE["internal/store<br/>GORM + 版本化迁移"]
        DB[("MySQL 8.0<br/>9 张表")]
    end

    CLI --> LOOP
    CLI --> RESUME
    LOOP --> POLICY
    LOOP --> GATE
    LOOP --> PERS
    RESUME --> PERS
    POLICY -.->|"只看静态属性"| REG
    GATE --> STORE
    PERS --> CLS
    PERS --> RETRY
    PERS --> BRK
    PERS --> IDEM
    PERS --> WRITE
    LOOP --> READ
    READ --> DS
    WRITE --> DS
    DS --> STORE
    LOOP --> LLM
    STORE --> DB
```

**这张图要说明的三件事**：

1. **依赖方向永远朝内**：`orchestrator` 不知道 MySQL，`tools` 不知道谁在调用它，
   `executor` 不知道任务是什么。每一层只依赖下一层的**接口**（`ToolRunner`、`DataSource`、`StateStore`）。
2. **权限治理是横切的双层**：`policy` 判定"要不要批"，`approval` 负责"批之前先落库"。
   它们都不碰业务参数——判定依据只有工具声明的静态属性。
3. **执行层可以脱离编排层单独测**：`executor` 的全部测试都不需要模型，
   `tools` 的测试不需要数据库。这是"能测"的前提。

> 怎么讲清楚："我用依赖方向来保证'权限不可被绕过'：模型只能通过 orchestrator 提请求，
> 而 orchestrator 在调用工具**之前**必须经过 policy 与 gate。它没有别的路可走。"

---

## 图 2 · 一次只读调用的控制流

```mermaid
sequenceDiagram
    autonumber
    participant U as 用户
    participant O as orchestrator
    participant L as 模型
    participant T as Registry
    participant E as PersistedExecutor
    participant D as MySQL

    U->>O: -ask "checkout-service 是不是变慢了"
    O->>O: 落 tasks(status=running)
    O->>L: 消息 + 工具 schema
    L-->>O: tool_calls: query_metrics
    O->>T: Get(name)
    T-->>O: Tool + Spec
    O->>O: Validate(args) 参数校验
    O->>O: policy.NeedsApproval → false
    O->>E: Run(tool, args, CallRef{task, step=1})
    E->>E: key = sha256(task|step|tool|canonical args)
    E->>D: ① INSERT tool_calls (status=started)
    E->>D: ② 查指标（真查询）
    E->>D: ③ UPDATE 结果 (status=success)
    E-->>O: Outcome{Result, Class=ok}
    O->>D: task_steps 追加一行（含回喂原文）
    O->>L: tool 结果
    L-->>O: 最终回答
    O->>D: tasks(status=success)
    O-->>U: 答案 + 轨迹 + token 用量
```

**关键点**：`step_no` 在**执行之前**就确定了，它是幂等 key 的一部分，
也是 `task_steps` 的主键之一——三者由同一个编号串起来，这是恢复能成立的前提。

---

## 图 3 · 高危调用：挂起 → 审批 → 续跑

```mermaid
sequenceDiagram
    autonumber
    participant O as orchestrator
    participant P as policy
    participant G as Gate
    participant DB as MySQL
    participant H as 人
    participant R as Resume

    O->>P: NeedsApproval(spec, args)
    P-->>O: require=true（restart_service 是高危）
    O->>G: Park(意图 + 工单)
    G->>DB: ① INSERT tool_calls(status=waiting_approval)
    G->>DB: ② INSERT approvals(status=pending, idem_key 唯一)
    G-->>O: ticket=tk_xxx
    O->>DB: tasks(status=waiting_approval)
    O-->>H: 打印工单 + 预览 + 审批命令
    Note over O,H: 进程可以退出——三处状态都已落库

    H->>DB: -approve tk_xxx (pending → approved)
    H->>R: -resume task_xxx
    R->>DB: 读 tasks + task_steps（重建对话）
    R->>G: Park(同一 key) → 命中已批工单
    G-->>R: status=approved → 放行
    R->>DB: 在原 step_no 上执行（不进新步骤！）
    R->>DB: tool_calls: waiting_approval → success
    R-->>H: 继续循环并给出最终回答
```

**这张图是项目的高光**：`-approve` 只改工单，不动执行状态（人批的是"这件事"，
而不是"这一次进程"）；`-resume` 用**原步骤号**重放，所以幂等 key 不变、
写操作不会生效两次。实测证据见 [design.md §16](design.md)。

---

## 图 4 · 状态机（三张表各自的合法迁移）

> 三张表分三张图：状态名在 `tasks` 与 `tool_calls` 里是重名的（都有 `success`/`failed`），
> 放进同一张 Mermaid 图里会被合并成同一个节点——这是画图时最容易出的错。

**`tasks`（任务）**

```mermaid
stateDiagram-v2
    state "running" as tr
    state "waiting_approval" as tw
    state "success" as to
    state "failed" as tf
    state "canceled" as tc

    [*] --> tr
    tr --> to : 模型给出最终答案
    tr --> tf : 模型/落库失败
    tr --> tw : 工具需要审批
    tw --> tr : -resume（工单已批），继续循环
    tw --> tf : 工单被拒 / 过期
    tr --> tc : 人工取消
```

**`tool_calls`（执行记录，主键 = 幂等 key）**

```mermaid
stateDiagram-v2
    state "started" as cs
    state "waiting_approval" as cw
    state "success" as co
    state "failed" as cf
    state "skipped" as ck

    [*] --> cs
    cs --> co : 执行成功
    cs --> cf : 执行失败（含重试耗尽）
    cs --> cw : 策略判定需要审批
    cw --> co : 获批后执行成功
    cw --> cf : 被拒 / 过期
    co --> co : 重复到达 → 回放上次结果（不重复执行）
```

**`approvals`（工单）**

```mermaid
stateDiagram-v2
    state "pending" as ap
    state "approved" as aa
    state "rejected" as ar
    state "expired" as ae

    [*] --> ap
    ap --> aa : -approve
    ap --> ar : -reject
    ap --> ae : 超过 TTL（-approvals 时清理）
```

**为什么值得画出来**：这三张表的状态迁移**都只用条件更新**实现
（`WHERE status='pending'`），所以"批两次"、"批完再拒"这类竞态在数据库层面就失败了。
挂起链路的正确性不依赖应用层的判断——这是能扛住进程重启的原因。

---

## 表结构与它们各自回答的问题

| 表 | 回答的问题 | 生命周期 |
|---|---|---|
| `tasks` | 这个任务做到哪了 | 可清理 |
| `task_steps` | 每一步做了什么、回喂了什么 | 可清理 |
| `tool_calls` | 这个操作发出去过没有、结果是什么 | 可清理 |
| `approvals` | 等谁批、批了没有 | 可清理 |
| `audit_logs` | **谁在什么时候做了什么** | **只增不删**（无外键） |
| `services` / `service_configs` / `service_metrics` | 被运维的对象（演示数据） | 可重灌 |
| `config_changes` | 下游幂等的台账（同一 key 只生效一次） | 只增 |

细节与"为什么要拆开"见 [design.md §3](design.md)。
