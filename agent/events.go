// events.go 过程事件：UI 展示用的粗粒度 OnEvent 与结构化工具事件
// ToolEvent（一次工具调用的完整生命周期，供嵌入方重建时间线）。
package agent

import "sync/atomic"

// OnEvent 过程事件回调（UI 展示）。kind：
//
//	tool    工具调用开始（detail: "name args摘要"）
//	result  工具结果首行
//	done    一次 Run 结束
//	notice  系统通知（子代理落定等）
//	todos   todo 列表变化（detail 为 JSON）
//	goal    goal 状态变化（detail 为摘要）
//	plan    计划模式变化
//	phase   workflow phase/log
type OnEvent func(kind, detail string)

// 工具事件状态（ToolEvent.Status）。
const (
	ToolStatusRunning = "running" // 开始执行（含审批前；等待用户审批时也处于此状态）
	ToolStatusDone    = "done"    // 执行成功
	ToolStatusError   = "error"   // 执行失败（Err 携带错误信息）
	ToolStatusDenied  = "denied"  // 审批未通过（Output 携带拒绝与反馈文本）
)

// ToolEvent 结构化工具事件：一次工具调用的完整生命周期，供嵌入方重建
// 工具时间线（进行中 → 完成/失败卡片、展开看全量输入输出）。同一
// CallID 依状态推进：running → done | error | denied。Input/Output 均为
// 全量原文不截断，展示裁剪由宿主自理。子代理的工具事件也会回流到根
// agent 的回调（用 AgentID/Label 区分来源）。
type ToolEvent struct {
	CallID  string // 工具调用 ID（LLM 返回的 tool_call id；正文回退解析生成；均为进程内唯一）
	Name    string
	Status  string // ToolStatusRunning / Done / Error / Denied
	Input   string // 完整参数 JSON
	Output  string // 完整结果文本（running 时为空）
	Err     string // Status=error 时的错误信息
	AgentID string // 发起调用的 agent（根为 agent_root）
	Label   string // agent 标签（根为 main，子代理为其 description）
}

// OnToolEvent 结构化工具事件回调。
type OnToolEvent func(ToolEvent)

// toolEventSeq 兜底 ID 计数：个别服务端不回 tool_call id 时进程内唯一。
var toolEventSeq atomic.Int64

func (a *Agent) emit(kind, detail string) {
	if a.onEvent != nil {
		a.onEvent(kind, detail)
	}
	if a.host != nil {
		a.host.NotifyEvent(kind, detail)
	}
}

// emitToolEvent 结构化工具事件：本 agent 未注册回调时，子代理的事件
// 回流到根 agent 的回调（时间线需要覆盖整棵 agent 树）。
func (a *Agent) emitToolEvent(e ToolEvent) {
	e.AgentID = a.ID
	e.Label = a.Label
	fn := a.onToolEvent
	if fn == nil && a.mgr != nil {
		if root := a.mgr.Root(); root != nil && root != a {
			fn = root.onToolEvent
		}
	}
	if fn != nil {
		fn(e)
	}
}
