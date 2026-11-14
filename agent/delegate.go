// delegate.go 委派优先横切件（delegation-first）：根会话在内联调研达到
// 阈值、单批扇出读取、上下文接近压缩阈值三种形状下，经通知通道提醒模型
// 改用子代理采集、根会话只做综合。三档提醒各有闩锁（每会话至多 2/1/1
// 次，防反复注入干扰任务）；子代理不提醒——子代理是采集主体，劝其委派
// 只会造出无界代理链。所有计数仅在回合 goroutine 访问（同 trackRepeat，
// 不加锁）；通知经 EnqueueNotice 在下次 LLM 请求前并入，tool 结果闭合
// 约束不被破坏。
//
// 阈值与文案经 2026-11 配对实验校准（8 任务 × 2 重复 × 3 臂，glm-5.3）：
// 旧参数（阈值 8/20 + 建议式文案）委派率 64%，短会话够不到第一档；提前
// 到 4/10 并换祈使句 + 委派简报三要素后 86%，根上下文体量降约三成。
package agent

import (
	"fmt"

	"git.gorun.fun/share/grf-harness/llm"
)

// inlineResearchTools 内联调研类工具：根会话里累积这些调用就是在消耗
// 根上下文做"本可委派"的采集。bash 不计入——读写混杂，无法静态判定。
var inlineResearchTools = map[string]bool{
	"read":       true,
	"grep":       true,
	"glob":       true,
	"web_search": true,
}

// delegateRemindThresholds 内联调研累计调用的递进提醒阈值：命中各档
// 提醒一次。第一档 4 落在"采集将成规模"的临界点（8 太晚——多数调研任务
// 全程内联也只 5~10 次，够不到就结束了），第二档 10 接近失控。
var delegateRemindThresholds = []int{4, 10}

// fanoutRemindMin 单批并行扇出的最小读取调用数：模型一批发出这么多
// 并行读取，正是"本该开子代理"的瞬间形状。
const fanoutRemindMin = 3

// trackDelegateNudge 更新内联调研累计数并在命中阈值时入队委派提醒。
// 与 trackRepeat 同点调用（dispatchBatch，仅回合 goroutine）。
func (a *Agent) trackDelegateNudge(call llm.ToolCall) {
	if !a.delegateNudgeable() || !inlineResearchTools[call.Name()] {
		return
	}
	a.inlineCalls++
	if a.delegRemindStage < len(delegateRemindThresholds) &&
		a.inlineCalls >= delegateRemindThresholds[a.delegRemindStage] {
		a.delegRemindStage++
		a.EnqueueNotice(delegateStageNotice(a.delegRemindStage, a.inlineCalls))
	}
}

// delegateStageNotice 累计档提醒文案：第一档祈使句 + 委派简报三要素
//（问题/范围/回传物——只喊"去委派"不给模板，模型常写出贫瘠简报）；第二档
// 点名"已建议过仍内联"升级施压。实验显示建议式文案（consider ...）约
// 三分之一会被无视，祈使句显著更有效。
func delegateStageNotice(stage, n int) string {
	if stage == 1 {
		return fmt.Sprintf("you have made %d inline read/search calls in this root conversation; their results occupy this context permanently. Do the remaining gathering in a subagent instead (subagent tool). Write the subagent brief with three parts: (1) the exact question to answer, (2) the scope — which files/paths to search and with which tools, (3) what to return — a synthesized answer, not raw dumps. Keep only the synthesis in this conversation", n)
	}
	return fmt.Sprintf("inline gathering continued after being advised to delegate (%d inline read/search calls now). Delegation is the intended path for gathering: send the next gathering step as a subagent task; if you already have enough material, stop reading and write the final answer now", n)
}

// noteFanout 检测模型单批回复里的并行读取扇出（≥ fanoutRemindMin 个
// 内联调研调用），一次性提醒改用子代理。runTurn 在 dispatchBatch 前调用
//（按模型发起的整批计数，不受并行分组切分影响）。
func (a *Agent) noteFanout(calls []llm.ToolCall) {
	if a.fanoutReminded || !a.delegateNudgeable() {
		return
	}
	n := 0
	for _, c := range calls {
		if inlineResearchTools[c.Name()] {
			n++
		}
	}
	if n < fanoutRemindMin {
		return
	}
	a.fanoutReminded = true
	a.EnqueueNotice(fmt.Sprintf("you are fanning out %d parallel read/search calls inside this conversation. Break that pattern: one subagent (subagent tool) can do this gathering in its own context — brief it with the question to answer, the scope to search, and what to return, then keep only the synthesis here", n))
}

// notePressure 上下文接近压缩阈值时的一次性委派提醒（maybeAutoCompact
// 在新回合入口调用，通知在本回合首个请求前并入）。委派"省根上下文"的
// 价值只有存在压力时可见——这条把它显式化。实验归因显示这是主力档
//（多数委派由它触发）。
func (a *Agent) notePressure(est, threshold int64) {
	if a.pressureReminded || !a.delegateNudgeable() || est < threshold*6/10 {
		return
	}
	a.pressureReminded = true
	a.EnqueueNotice(fmt.Sprintf("this conversation's context is filling (estimated ~%dk of the ~%dk compact threshold). Every further inline read/search call spends root context you will not get back: delegate the remaining gathering to a subagent now, and synthesize from what is already here", est/1000, threshold/1000))
}

// delegateNudgeable 委派提醒生效前提：根会话且 subagent 工具实际可用
//（被禁用时提醒只是噪声）。
func (a *Agent) delegateNudgeable() bool {
	return a.rootAgent && a.reg.Has("subagent")
}
