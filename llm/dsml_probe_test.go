package llm

import "testing"

func TestProbeUserPaste(t *testing.T) {
	paste := "<｜｜DSML｜｜tool_calls>\n<｜｜DSML｜｜invoke name=\"update_goal\">\n<｜｜DSML｜｜parameter name=\"action\" string=\"true\">edit</｜｜DSML｜｜parameter>\n<｜｜DSML｜｜parameter name=\"goal_id\" string=\"true\">goal_1_e62f9</｜｜DSML｜｜parameter>\n<｜｜DSML｜｜parameter name=\"objective\" string=\"true\">测试目标管理功能</｜｜DSML｜｜parameter>\n<｜｜DSML｜｜parameter name=\"revision\" string=\"false\">1</｜｜DSML｜｜parameter>\n</｜｜DSML｜｜invoke>\n</｜｜DSML｜｜tool_calls>"
	m := Message{Role: "assistant", Content: "说明文字\n" + paste}
	if !fallbackContentToolCalls(&m) {
		t.Logf("no calls extracted; content=%q", m.Content)
	} else {
		t.Logf("extracted %d calls; cleaned=%q", len(m.ToolCalls), m.Content)
		for _, c := range m.ToolCalls {
			t.Logf("  %s %s", c.Function.Name, c.Function.Arguments)
		}
	}
	// 变体：ASCII 竖线
	ascii := "<||DSML||tool_calls>\n<||DSML||invoke name=\"create_goal\">\n<||DSML||parameter name=\"objective\" string=\"true\">测试</||DSML||parameter>\n</||DSML||invoke>\n</||DSML||tool_calls>"
	m2 := Message{Role: "assistant", Content: ascii}
	if !fallbackContentToolCalls(&m2) {
		t.Logf("ASCII variant: no calls extracted; content=%q", m2.Content)
	} else {
		t.Logf("ASCII variant: %d calls", len(m2.ToolCalls))
		for _, c := range m2.ToolCalls {
			t.Logf("  %s %s", c.Function.Name, c.Function.Arguments)
		}
	}
	// 变体：invoke 出现在 reasoning
	m3 := Message{Role: "assistant", Reasoning: paste}
	if !fallbackContentToolCalls(&m3) {
		t.Logf("reasoning: no calls extracted")
	} else {
		t.Logf("reasoning: %d calls", len(m3.ToolCalls))
	}
}
