// bridge_args_test.go 工具参数流式桥接测试：grf:tool_call.args_delta 事件
// 序列、执行事件收编待定项（不重复建 item）、暂停复用、中断残留收尾。
package server

import (
	"strings"
	"testing"

	"git.gorun.fun/share/grf-harness/agent"
)

// collect 收集桥发出的事件类型序列（附 ItemID）。
func collect(b *bridge) *[]event {
	var evs []event
	b.sink = func(ev *event) { evs = append(evs, *ev) }
	return &evs
}

func types(evs *[]event) []string {
	out := make([]string, 0, len(*evs))
	for _, e := range *evs {
		out = append(out, e.Type)
	}
	return out
}

// 参数流式 → 执行收编：args_delta 逐片发出，执行事件复用待定 item，
// 全程只有一次 output_item.done、不出现第二个 tool item。
func TestToolArgsStreamMergedIntoExecution(t *testing.T) {
	b := newBridge(nil, func() int64 { return 1 })
	evs := collect(b)

	b.OnToolArgs(0, "c1", "edit", `{"file_path":"/a.ts","old_str`)
	b.OnToolArgs(0, "c1", "edit", `ing":"x"}`) // id/name 重复到达幂等
	b.OnToolEvent(agent.ToolEvent{CallID: "c1", Name: "edit", Status: agent.ToolStatusRunning,
		Input: `{"file_path":"/a.ts","old_string":"x"}`})
	b.OnToolEvent(agent.ToolEvent{CallID: "c1", Name: "edit", Status: agent.ToolStatusDone,
		Output: "edited /a.ts (1 replacement)"})
	out := b.finish("done")

	// args_delta 每片一个;added 不发(标准 ORS 客户端无感)
	argsDeltas := 0
	added := 0
	for _, e := range *evs {
		switch e.Type {
		case "grf:tool_call.args_delta":
			argsDeltas++
		case "response.output_item.added":
			if e.Item != nil && e.Item.Type == itemGrfToolCall {
				added++
			}
		}
	}
	if argsDeltas != 2 {
		t.Fatalf("args_delta 应逐片发出 2 个,实际 %d: %v", argsDeltas, types(evs))
	}
	if added != 0 {
		t.Fatalf("参数流式路径不应发 grf:tool_call 的 output_item.added,实际 %d", added)
	}
	// 快照里只有一个 tool item,参数/结果齐全
	toolItems := 0
	for _, it := range out {
		if it.Type == itemGrfToolCall {
			toolItems++
			if it.CallID != "c1" || it.Status != "completed" {
				t.Fatalf("合并后 item 不符: %+v", it)
			}
			if it.Input != `{"file_path":"/a.ts","old_string":"x"}` || it.Output != "edited /a.ts (1 replacement)" {
				t.Fatalf("Input/Output 应以执行事件全量为准: %+v", it)
			}
		}
	}
	if toolItems != 1 {
		t.Fatalf("应恰好一个 tool item,实际 %d", toolItems)
	}
	// 收编时 in_progress 携带 Item(宿主据此补 call_id/全量参数)
	hasItemInProgress := false
	for _, e := range *evs {
		if e.Type == "grf:tool_call.in_progress" && e.Item != nil && e.Item.CallID == "c1" {
			hasItemInProgress = true
		}
	}
	if !hasItemInProgress {
		t.Fatalf("收编的 in_progress 应携带 Item: %v", types(evs))
	}
}

// 工具调用头(name/id 先到、参数为空):即建待定 item 并发事件(无 delta),
// 宿主在模型开始生成参数前就有卡可看;参数增量随后补齐。
func TestToolArgsHeaderOnly(t *testing.T) {
	b := newBridge(nil, func() int64 { return 1 })
	evs := collect(b)

	b.OnToolArgs(0, "c1", "edit", "")   // 头片:name 先到
	b.OnToolArgs(0, "c1", "edit", "{}") // 参数

	if len(*evs) != 2 {
		t.Fatalf("头片+参数应各发一个事件,实际 %d: %v", len(*evs), types(evs))
	}
	first := (*evs)[0]
	if first.Type != "grf:tool_call.args_delta" || first.Kind != "edit" || first.Delta != "" {
		t.Fatalf("头片事件应为无 delta 的 args_delta(带工具名): %+v", first)
	}
	if b.items[0].Name != "edit" || b.items[0].Input != "{}" {
		t.Fatalf("item 应随参数补齐: %+v", b.items[0])
	}
}

// 未流式过的工具(如 DSML 回退/旧路径):保持原行为——added + in_progress。
func TestToolWithoutArgsStreamKeepsLegacyPath(t *testing.T) {
	b := newBridge(nil, func() int64 { return 1 })
	evs := collect(b)

	b.OnToolEvent(agent.ToolEvent{CallID: "c9", Name: "bash", Status: agent.ToolStatusRunning, Input: "{}"})
	b.OnToolEvent(agent.ToolEvent{CallID: "c9", Name: "bash", Status: agent.ToolStatusDone, Output: "ok"})

	added := 0
	for _, e := range *evs {
		if e.Type == "response.output_item.added" && e.Item != nil && e.Item.Type == itemGrfToolCall {
			added++
		}
	}
	if added != 1 {
		t.Fatalf("非流式路径应有一次 added,实际 %d: %v", added, types(evs))
	}
}

// 生成中断:待定项收尾标 incomplete,不发 done,不残留 argsOpen。
func TestToolArgsStreamAbortedCleanup(t *testing.T) {
	b := newBridge(nil, func() int64 { return 1 })
	evs := collect(b)

	b.OnToolArgs(0, "", "write", `{"file_path":"/x.md","content":"a\nb`) // id 缺失的极端路径
	out := b.finish("")

	if len(b.argsOpen) != 0 {
		t.Fatalf("finish 后 argsOpen 应清空: %v", b.argsOpen)
	}
	for _, e := range *evs {
		if e.Type == "response.output_item.done" && strings.HasPrefix(e.ItemID, "it_") {
			t.Fatalf("未发过 added 的待定项不应补 done: %v", types(evs))
		}
	}
	found := false
	for _, it := range out {
		if it.Type == itemGrfToolCall && it.Status == "incomplete" {
			found = true
		}
	}
	if !found {
		t.Fatalf("快照应含 incomplete 的待定 item: %+v", out)
	}
}

// 暂停(approval):pausedItems 复用待定 item,不重复建卡。
func TestToolArgsStreamPausedReusesItem(t *testing.T) {
	b := newBridge(nil, func() int64 { return 1 })
	evs := collect(b)

	b.OnToolArgs(0, "c2", "edit", `{"file_path":"/a.ts","old_string":"x"`)
	_, reqs := b.pausedItems(&agent.PausedTurn{Calls: []agent.PauseCall{{
		CallID: "c2", Name: "edit", Kind: agent.PauseKindApproval,
		Arguments: `{"file_path":"/a.ts","old_string":"x"}`,
	}}})
	out := b.finish("")

	if len(reqs) != 1 || reqs[0].CallID != "c2" {
		t.Fatalf("交互请求应原样透出: %+v", reqs)
	}
	toolItems := 0
	for _, it := range out {
		if it.Type == itemGrfToolCall {
			toolItems++
		}
	}
	if toolItems != 1 {
		t.Fatalf("暂停应复用待定 item,实际 %d 个: %+v", toolItems, out)
	}
	reused := false
	for _, e := range *evs {
		if e.Type == "grf:tool_call.in_progress" && e.Item != nil && e.Item.CallID == "c2" {
			reused = true
		}
	}
	if !reused {
		t.Fatalf("暂停复用应发携带 Item 的 in_progress: %v", types(evs))
	}
}
