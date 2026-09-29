// steer.go 运行中插话（dsh steering 的最小实现）：用户在回合进行中提交
// 的输入排队为下一步骤输入，在步骤边界（与通知 drain 同一点——tool 结果
// 已闭合、OpenAI 协议要求 user 消息不得插进未闭合的 tool_call）以普通
// user 消息并入，不打断在跑的工具，模型下一个请求即见。回合结束后仍在
// 排队的插话由表面决定去向（TUI 转为下一回合输入，见 finishStream 的
// DrainSteer）；runTurn 的回合开头也会 drain 一次，覆盖空闲期排入的
// 插话。内存队列不持久化：排队输入等同"已打出未发送"，进程崩溃丢失
// 可接受（与 dsh 持久化 inbox 的差异是有意的，见架构文档）。
package agent

import (
	"strings"

	"git.gorun.fun/share/grf-harness/llm"
)

// Steer 排队一条运行中插话。回合运行中：下一个步骤边界注入历史；空闲：
// 留待下一次 Run 的回合开头并入（是否自动续跑由表面决定，本方法不触发
// OnIdleNotice——插话的措辞是给模型的用户消息，不是后台事件通知）。
// 空白文本忽略。
func (a *Agent) Steer(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	a.mu.Lock()
	a.steer = append(a.steer, text)
	a.mu.Unlock()
}

// DrainSteer 取走全部排队插话（TUI 回合结束时把未注入的插话转为下一
// 回合输入；与 runTurn 步骤边界的注入共用本取口）。
func (a *Agent) DrainSteer() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.steer) == 0 {
		return nil
	}
	out := a.steer
	a.steer = nil
	return out
}

// injectSteer 步骤边界注入：每条插话一条普通 user 消息（不带标记——
// 插话内容本身即用户口吻，dsh 的来源元数据在 OpenAI 线格式里无处安放）。
func (a *Agent) injectSteer() {
	for _, s := range a.DrainSteer() {
		a.mu.Lock()
		a.history = append(a.history, llm.Message{Role: "user", Content: s})
		a.mu.Unlock()
	}
}
