// bridge.go 事件桥：把一个回合的 agent 回调（流式正文/思考、结构化
// ToolEvent、行式 OnEvent）重组为 ORS 的 item 生命周期事件（output_item.
// added → content_part.added → *.delta → *.done → output_item.done），
// 同时维护 output items 供建终态快照。回调全部在 agent 回合 goroutine 上
// 触发，桥本身单 goroutine 访问；sink 出口需并发安全（SSE 写出或丢弃）。
package server

import (
	"fmt"
	"strings"

	"git.gorun.fun/share/grf-harness/agent"
)

// bridge 一次回合的事件桥。
type bridge struct {
	sink     func(*event) // 事件出口（nil = 仅收集 items，非流式用）
	nextSeq  func() int64
	items    []Item
	open     map[string]int // CallID → items 下标（工具 item 生命周期）
	argsOpen map[int]int    // 参数流式已开、尚未开跑的工具 item（流内 index → items 下标）
	cur      int            // 当前开放 item 下标；-1 无
	curKind  string         // "message" | "reasoning"
	curText  strings.Builder
	itemSeq  int
}

func newBridge(sink func(*event), nextSeq func() int64) *bridge {
	return &bridge{sink: sink, nextSeq: nextSeq, open: map[string]int{}, argsOpen: map[int]int{}, cur: -1}
}

// emit 发一个事件（sink 为 nil 时静默）。
func (b *bridge) emit(ev *event) {
	ev.SequenceNumber = b.nextSeq()
	if b.sink != nil {
		b.sink(ev)
	}
}

func (b *bridge) itemID() string {
	b.itemSeq++
	return fmt.Sprintf("it_%d", b.itemSeq)
}

// closeCur 关闭当前开放的消息/思考 item（走 done 生命周期），并把累计
// 文本写回 item 的内容部件（终态快照的 output 必须携带全文）。
func (b *bridge) closeCur() {
	if b.cur < 0 {
		return
	}
	it := &b.items[b.cur]
	text := b.curText.String()
	switch b.curKind {
	case "message":
		if len(it.Content) == 0 {
			it.Content = []contentPart{{Type: "output_text"}}
		}
		it.Content[0].Text = text
		b.emit(&event{Type: "response.output_text.done", ItemID: it.ID, OutputIndex: b.cur,
			ContentIndex: 0, Text: text})
		b.emit(&event{Type: "response.content_part.done", ItemID: it.ID, OutputIndex: b.cur,
			ContentIndex: 0, Part: &contentPart{Type: "output_text", Text: text}})
	default: // reasoning
		if len(it.Summary) == 0 {
			it.Summary = []contentPart{{Type: "summary_text"}}
		}
		it.Summary[0].Text = text
		b.emit(&event{Type: "response.reasoning_summary_text.done", ItemID: it.ID, OutputIndex: b.cur,
			SummaryIndex: 0, Text: text})
		b.emit(&event{Type: "response.reasoning_summary_part.done", ItemID: it.ID, OutputIndex: b.cur,
			SummaryIndex: 0, Part: &contentPart{Type: "summary_text", Text: text}})
	}
	it.Status = "completed"
	b.emit(&event{Type: "response.output_item.done", OutputIndex: b.cur, Item: it})
	b.cur, b.curKind = -1, ""
	b.curText.Reset()
}

// openTextItem 开启 message / reasoning item（同类连续 delta 复用当前）。
func (b *bridge) openTextItem(kind string) {
	if b.cur >= 0 && b.curKind == kind {
		return
	}
	b.closeCur()
	it := Item{ID: b.itemID(), Status: "in_progress"}
	if kind == "message" {
		it.Type = itemMessage
		it.Role = "assistant"
		it.Content = []contentPart{{Type: "output_text"}}
		b.items = append(b.items, it)
		idx := len(b.items) - 1
		b.cur, b.curKind = idx, kind
		b.emit(&event{Type: "response.output_item.added", OutputIndex: idx, Item: &b.items[idx]})
		b.emit(&event{Type: "response.content_part.added", ItemID: it.ID, OutputIndex: idx,
			ContentIndex: 0, Part: &contentPart{Type: "output_text"}})
		return
	}
	// reasoning（思考流映射为 reasoning summary——ORS 的思考摘要通道）
	it.Type = itemReasoning
	it.Summary = []contentPart{{Type: "summary_text"}}
	b.items = append(b.items, it)
	idx := len(b.items) - 1
	b.cur, b.curKind = idx, kind
	b.emit(&event{Type: "response.output_item.added", OutputIndex: idx, Item: &b.items[idx]})
	b.emit(&event{Type: "response.reasoning_summary_part.added", ItemID: it.ID, OutputIndex: idx,
		SummaryIndex: 0, Part: &contentPart{Type: "summary_text"}})
}

// OnContent 正文增量（agent.SetStreamHooks.Content）。
func (b *bridge) OnContent(delta string) {
	if delta == "" {
		return
	}
	b.openTextItem("message")
	b.curText.WriteString(delta)
	it := &b.items[b.cur]
	b.emit(&event{Type: "response.output_text.delta", ItemID: it.ID, OutputIndex: b.cur,
		ContentIndex: 0, Delta: delta})
}

// OnReasoning 思考增量（agent.SetStreamHooks.Reasoning）。
func (b *bridge) OnReasoning(delta string) {
	if delta == "" {
		return
	}
	b.openTextItem("reasoning")
	b.curText.WriteString(delta)
	it := &b.items[b.cur]
	b.emit(&event{Type: "response.reasoning_summary_text.delta", ItemID: it.ID, OutputIndex: b.cur,
		SummaryIndex: 0, Delta: delta})
}

// OnToolArgs 工具调用生成增量（llm.StreamHooks.ToolArgs）：头片（name/id
// 先到、参数为空）即开 grf:tool_call 待定项——宿主在模型开始生成就有卡
// 可看，不再干等参数；参数增量逐片发 grf:tool_call.args_delta 扩展事件
// （如 edit/write 的 +nn/-nn 实时统计）。不发 output_item.added——标准
// ORS 客户端对整个过程无感，待定项由执行/暂停事件收编或收尾兜底。不动
// b.cur：正文与参数可交错，文本 item 的生命周期不受影响。
func (b *bridge) OnToolArgs(index int, id, name, delta string) {
	idx, ok := b.argsOpen[index]
	fresh := false
	if !ok {
		it := Item{Type: itemGrfToolCall, ID: b.itemID(), Status: "in_progress",
			CallID: id, Name: name}
		b.items = append(b.items, it)
		idx = len(b.items) - 1
		b.argsOpen[index] = idx
		fresh = true
	}
	it := &b.items[idx]
	if id != "" {
		it.CallID = id
	}
	nameSet := false
	if name != "" && it.Name == "" {
		it.Name = name
		nameSet = true
	}
	it.Input += delta
	// 仅在有待发布信息时发事件：新开 item、工具名刚到位或参数有增量
	// （name 碎片重发对宿主无信息量）。
	if fresh || nameSet || delta != "" {
		b.emit(&event{Type: "grf:tool_call.args_delta", ItemID: it.ID, OutputIndex: idx,
			Kind: it.Name, Delta: delta})
	}
}

// takeArgsItem 取出与执行事件匹配的参数流式 item：优先 call_id 精确匹配，
// 其次同名的未标识项（个别服务端不回 tool_call id）。取出即从待定表移除。
func (b *bridge) takeArgsItem(callID, name string) (int, bool) {
	found := -1
	if callID != "" {
		for _, v := range b.argsOpen {
			if b.items[v].CallID == callID {
				found = v
				break
			}
		}
	}
	if found < 0 {
		for _, v := range b.argsOpen {
			if it := b.items[v]; it.CallID == "" && it.Name == name {
				found = v
				break
			}
		}
	}
	if found < 0 {
		return 0, false
	}
	for k, v := range b.argsOpen {
		if v == found {
			delete(b.argsOpen, k)
		}
	}
	return found, true
}

// OnToolEvent 工具事件：running 开 grf:tool_call item，终态关闭。
func (b *bridge) OnToolEvent(e agent.ToolEvent) {
	switch e.Status {
	case agent.ToolStatusRunning:
		b.closeCur() // 文本段与工具调用是不同 item
		if idx, ok := b.takeArgsItem(e.CallID, e.Name); ok {
			// 参数流式已开的 item：补全执行信息后复用（不重发 output_item.
			// added，宿主已建卡）；Input 以 ToolEvent 全量参数为准。
			it := &b.items[idx]
			it.CallID, it.Input = e.CallID, e.Input
			it.AgentID, it.Label = e.AgentID, e.Label
			b.open[e.CallID] = idx
			b.emit(&event{Type: "grf:tool_call.in_progress", ItemID: it.ID, OutputIndex: idx,
				Kind: e.Name, Detail: e.Input, Item: it})
			return
		}
		it := Item{
			Type: itemGrfToolCall, ID: b.itemID(), Status: "in_progress",
			CallID: e.CallID, Name: e.Name, Input: e.Input,
			AgentID: e.AgentID, Label: e.Label,
		}
		b.items = append(b.items, it)
		idx := len(b.items) - 1
		b.open[e.CallID] = idx
		b.emit(&event{Type: "response.output_item.added", OutputIndex: idx, Item: &b.items[idx]})
		b.emit(&event{Type: "grf:tool_call.in_progress", ItemID: it.ID, OutputIndex: idx,
			Kind: e.Name, Detail: e.Input})
	default:
		idx, ok := b.open[e.CallID]
		if !ok {
			return
		}
		delete(b.open, e.CallID)
		it := &b.items[idx]
		it.Output = e.Output
		it.Err = e.Err
		switch e.Status {
		case agent.ToolStatusDone:
			it.Status = "completed"
			b.emit(&event{Type: "grf:tool_call.completed", ItemID: it.ID, OutputIndex: idx,
				Kind: e.Name, Detail: e.Output})
		case agent.ToolStatusError:
			it.Status = "failed"
			b.emit(&event{Type: "grf:tool_call.failed", ItemID: it.ID, OutputIndex: idx,
				Kind: e.Name, Detail: e.Output})
		default: // denied
			it.Status = "denied" // ORS 允许扩展 item 状态值
			b.emit(&event{Type: "grf:tool_call.denied", ItemID: it.ID, OutputIndex: idx,
				Kind: e.Name, Detail: e.Output})
		}
		b.emit(&event{Type: "response.output_item.done", OutputIndex: idx, Item: it})
	}
}

// OnNotice 行式过程事件（agent.OnEvent 的 kind/detail）。
func (b *bridge) OnNotice(kind, detail string) {
	b.emit(&event{Type: "grf:notice", Kind: kind, Detail: detail})
}

// finish 回合收尾：确保最终回复落在 message item（非流式/截断兜底），
// 关闭所有开放 item。返回 output items 副本。
func (b *bridge) finish(reply string) []Item {
	// 最终回复兜底：流式路径已通过 OnContent 建好 item；整包回退路径
	// hooks 未流过——比对当前累计文本与 reply，不符则一次性补齐。
	if b.cur >= 0 && b.curKind == "message" {
		if b.curText.String() != reply {
			b.closeCur()
		}
	}
	if reply != "" && (b.cur < 0 || b.curKind != "message" || b.curText.String() != reply) {
		b.openTextItem("message")
		b.curText.WriteString(reply)
		it := &b.items[b.cur]
		it.Content = []contentPart{{Type: "output_text", Text: reply}}
	}
	b.closeCur()
	// 参数流式中断（生成失败/取消）残留的待定项：标 incomplete 留在快照
	// 里（未发过 added，不补 done；宿主侧按终态事件自行收口）。
	for _, v := range b.argsOpen {
		b.items[v].Status = "incomplete"
	}
	b.argsOpen = map[int]int{}
	// 暂停中未闭合的工具调用保持 in_progress（暂停语义：等交互应答）
	out := make([]Item, len(b.items))
	copy(out, b.items)
	return out
}

// pausedItems 把暂停调用补进 output：function 暂停映射为标准 function_call
// item（ORS 客户端执行工具形状）；交互暂停（approval/question）补
// grf:tool_call item 保持 in_progress。同时发 grf:input_required 扩展事件
// （交互暂停时）。
func (b *bridge) pausedItems(paused *agent.PausedTurn) (bool, []IncompleteRequest) {
	hasInteraction := false
	var reqs []IncompleteRequest
	for _, pc := range paused.Calls {
		reqs = append(reqs, IncompleteRequest{CallID: pc.CallID, Name: pc.Name, Kind: pc.Kind, Arguments: pc.Arguments})
		switch pc.Kind {
		case agent.PauseKindFunction:
			// 标准 function_call item：客户端执行（ORS 合规形状）
			it := Item{
				Type: itemFunctionCall, ID: b.itemID(), Status: "completed",
				CallID: pc.CallID, Name: pc.Name, Arguments: pc.Arguments,
			}
			b.items = append(b.items, it)
			idx := len(b.items) - 1
			b.emit(&event{Type: "response.output_item.added", OutputIndex: idx, Item: &b.items[idx]})
			b.emit(&event{Type: "response.output_item.done", OutputIndex: idx, Item: &b.items[idx]})
		default:
			hasInteraction = true
			// 交互请求 item（in_progress：应答后经 Resume 续跑）。参数流式
			// 已开的待定项直接收编（补 call_id/全量参数），避免与生成期
			// 卡片重复。
			if _, exists := b.open[pc.CallID]; !exists {
				if idx, ok := b.takeArgsItem(pc.CallID, pc.Name); ok {
					it := &b.items[idx]
					it.CallID, it.Input = pc.CallID, pc.Arguments
					b.emit(&event{Type: "grf:tool_call.in_progress", ItemID: it.ID, OutputIndex: idx,
						Kind: pc.Name, Detail: pc.Arguments, Item: it})
					continue
				}
				it := Item{
					Type: itemGrfToolCall, ID: b.itemID(), Status: "in_progress",
					CallID: pc.CallID, Name: pc.Name, Input: pc.Arguments,
				}
				b.items = append(b.items, it)
				idx := len(b.items) - 1
				b.emit(&event{Type: "response.output_item.added", OutputIndex: idx, Item: &b.items[idx]})
			}
		}
	}
	if hasInteraction {
		b.emit(&event{Type: "grf:input_required", GrfInput: &IncompleteDetails{Reason: inputRequired, Requests: reqs}})
	}
	out := make([]Item, len(b.items))
	copy(out, b.items)
	return hasInteraction, reqs
}
