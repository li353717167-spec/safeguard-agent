-- 0004_claim.sql · 执行租约（并发恢复保护）
--
-- 为什么需要它：幂等 key 只能挡住"**已完成的**重复"，挡不住"**同时在飞的**重复"。
-- 两个进程同时恢复同一步，都读到 waiting_approval、都没看到 success，于是都会执行。
-- 解法是乐观 CAS + 租约：抢到"执行权"的那个才许执行，其余立刻返回"处理中"。
--
-- 为什么用租约而不是数据库锁：调下游要几秒到几十秒，不能把事务开那么久。
-- 租约到期后允许别人重抢——这覆盖了"进程崩在中间"的情况；
-- 代价是"执行得比租约还慢"时可能被重抢，所以租约必须显著长于最长执行时间
-- （工具超时 × 重试次数 + 余量），并且被重抢的情况要能从 attempt 列看出来。

ALTER TABLE tool_calls
    ADD COLUMN claim_owner      VARCHAR(64) NULL COMMENT '当前执行者标识（host:pid:随机）',
    ADD COLUMN claim_expires_at DATETIME(3) NULL COMMENT '执行租约到期时间；过期后可被重抢',
    ADD COLUMN attempt          INT NOT NULL DEFAULT 0 COMMENT '抢到执行权的次数（含过期重抢）';

-- 扩展状态机：新增 executing
-- 语义变了：started = 意图已落库但还没抢到执行权；executing = 已抢到执行权，正在调下游。
ALTER TABLE tool_calls DROP CHECK chk_tool_calls_status;

ALTER TABLE tool_calls ADD CONSTRAINT chk_tool_calls_status
    CHECK (status IN ('started', 'executing', 'waiting_approval', 'success', 'failed', 'skipped'));
