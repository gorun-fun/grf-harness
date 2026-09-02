// history_test.go sanitizeSnapshot：中断残留的空 assistant/user 消息在
// 请求前剔除，避免服务端以缺 messages.content 拒绝（火山 400）。
package agent

import (
	"testing"

	"git.gorun.fun/share/grf-harness/llm"
)

func TestSanitizeSnapshotDropsEmptyMessages(t *testing.T) {
	in := []llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: ""}, // Ctrl+C 中断残留的半截消息
		{Role: "user", Content: ""},      // 同上
		{Role: "user", Content: "next"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{Type: "function", Function: llm.FunctionCall{Name: "read"}}}}, // 未闭合 tool_call：暂停态合法，保留
	}
	out := sanitizeSnapshot(in)
	want := []string{"system", "user", "user", "assistant"}
	if len(out) != len(want) {
		t.Fatalf("want %d messages, got %d: %+v", len(want), len(out), out)
	}
	for i, m := range out {
		if m.Role != want[i] {
			t.Fatalf("msg %d: want role %s, got %s", i, want[i], m.Role)
		}
	}
	// 原历史不被就地修改
	if len(in) != 6 || in[2].Role != "assistant" {
		t.Fatal("input slice must not be mutated")
	}
}
