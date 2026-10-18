package orchestrator

// loopGuard 是双上限保护。
//
// 为什么是**两个**上限而不是一个：轮次与工具调用次数是两种不同的失控方式。
//
//   - 轮次失控：模型每轮只发一个调用，但来回空转几十轮——烧 token、拖时间；
//   - 工具调用失控：模型一轮里并发发二十个调用——打垮下游、放大副作用。
//
// 只设一个必然漏掉另一种。所以两个都要有，而且都要能回答"撞的是哪一个"。
type loopGuard struct {
	maxRounds    int
	maxToolCalls int

	rounds    int
	toolCalls int
}

func newLoopGuard(maxRounds, maxToolCalls int) *loopGuard {
	if maxRounds <= 0 {
		maxRounds = 8
	}
	if maxToolCalls <= 0 {
		maxToolCalls = 20
	}
	return &loopGuard{maxRounds: maxRounds, maxToolCalls: maxToolCalls}
}

// canStartRound 判断还能不能再开一轮模型决策。
func (g *loopGuard) canStartRound() bool { return g.rounds < g.maxRounds }

// startRound 记一轮。返回轮次号（从 1 开始）。
func (g *loopGuard) startRound() int {
	g.rounds++
	return g.rounds
}

// canCallTool 判断还能不能再执行一次工具。
func (g *loopGuard) canCallTool() bool { return g.toolCalls < g.maxToolCalls }

// countToolCall 记一次工具调用。返回步骤号（从 1 开始，与 task_steps.step_no 对齐）。
func (g *loopGuard) countToolCall() int {
	g.toolCalls++
	return g.toolCalls
}

// limit 返回当前已经撞上的上限（没有则为空）。
//
// 注意判定顺序：先看工具调用，再看轮次。两者同时撞上时，
// 更贴近"刚刚发生了什么"的是工具调用——它解释了"为什么这一轮没能把所有调用都执行完"。
func (g *loopGuard) limit() (StopReason, bool) {
	if g.toolCalls >= g.maxToolCalls {
		return StopMaxToolCalls, true
	}
	if g.rounds >= g.maxRounds {
		return StopMaxRounds, true
	}
	return "", false
}

// uses 返回已经用掉的轮次与工具调用数。
func (g *loopGuard) uses() (rounds, toolCalls int) { return g.rounds, g.toolCalls }

// restore 用既有状态恢复计数（断点恢复用）。
//
// 不恢复计数会怎样：续跑时轮次从 0 重新开始，上限就形同虚设——
// 一个反复被挂起/恢复的任务可以无限期跑下去。
func (g *loopGuard) restore(rounds, toolCalls int) {
	if rounds > 0 {
		g.rounds = rounds
	}
	if toolCalls > 0 {
		g.toolCalls = toolCalls
	}
}
