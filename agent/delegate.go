// delegate.go 委派优先横切件（delegation-first）：根会话在内联调研达到
// 阈值、单批扇出读取、上下文接近压缩阈值三种形状下，经通知通道提醒模型
// 改用子代理采集、根会话只做综合。三档提醒各有闩锁（每会话至多 2/1/1
// 次，防反复注入干扰任务）；子代理不提醒——子代理是采集主体，劝其委派
// 只会造出无界代理链。所有计数仅在回合 goroutine 访问（同 trackRepeat，
// 不加锁）；通知经 EnqueueNotice 在下次 LLM 请求前并入，tool 结果闭合
// 约束不被破坏。
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
// 提醒一次（8 次时采集已成规模且多半还有剩余工作，20 次时接近失控）。
var delegateRemindThresholds = []int{8, 20}

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
		a.EnqueueNotice(fmt.Sprintf(
			"this conversation has already made %d inline read/search tool calls; the gathered material stays in this context permanently. For further gathering, consider delegating it to a subagent (subagent tool) and synthesizing the result here instead",
			a.inlineCalls))
	}
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
	a.EnqueueNotice(fmt.Sprintf(
		"you are fanning out %d parallel read/search calls inside this conversation; a subagent could do this gathering in its own context and return only the synthesis — consider delegating it (subagent tool)", n))
}

// notePressure 上下文接近压缩阈值时的一次性委派提醒（maybeAutoCompact
// 在新回合入口调用，通知在本回合首个请求前并入）。委派"省根上下文"的
// 价值只有存在压力时可见——这条把它显式化。
func (a *Agent) notePressure(est, threshold int64) {
	if a.pressureReminded || !a.delegateNudgeable() || est < threshold*6/10 {
		return
	}
	a.pressureReminded = true
	a.EnqueueNotice(fmt.Sprintf(
		"this conversation's context is filling (estimated ~%dk of the ~%dk compact threshold); prefer delegating further gathering to subagents instead of reading more into this context", est/1000, threshold/1000))
}

// delegateNudgeable 委派提醒生效前提：根会话且 subagent 工具实际可用
//（被禁用时提醒只是噪声）。
func (a *Agent) delegateNudgeable() bool {
	return a.rootAgent && a.reg.Has("subagent")
}
