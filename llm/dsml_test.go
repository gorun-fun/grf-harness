// dsml_test.go DSML 标签工具调用解析测试。
package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestExtractDSMLCalls(t *testing.T) {
	text := `查找一下图片文件
<｜｜DSML｜｜tool_calls>
<｜｜DSML｜｜invoke name="glob">
<｜｜DSML｜｜parameter name="pattern" string="true">**/*.{png,jpg,jpeg,webp,gif}</｜｜DSML｜｜parameter>
</｜｜DSML｜｜invoke>
</｜｜DSML｜｜tool_calls>`
	calls, cleaned := extractDSMLCalls(text)
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if calls[0].Name() != "glob" {
		t.Errorf("name = %q", calls[0].Name())
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(calls[0].Arguments()), &args); err != nil {
		t.Fatalf("arguments 非 JSON: %v (%s)", err, calls[0].Arguments())
	}
	if args["pattern"] != "**/*.{png,jpg,jpeg,webp,gif}" {
		t.Errorf("pattern = %v", args["pattern"])
	}
	if got := strings.TrimSpace(cleaned); got != "查找一下图片文件" {
		t.Errorf("cleaned = %q", cleaned)
	}
}

func TestExtractDSMLCallsJSONParam(t *testing.T) {
	text := `<｜｜DSML｜｜invoke name="bash">
<｜｜DSML｜｜parameter name="command" string="true">ls -la</｜｜DSML｜｜parameter>
<｜｜DSML｜｜parameter name="timeout">30</｜｜DSML｜｜parameter>
<｜｜DSML｜｜parameter name="env">{"k":"v"}</｜｜DSML｜｜parameter>
</｜｜DSML｜｜invoke>`
	calls, cleaned := extractDSMLCalls(text)
	if len(calls) != 1 || calls[0].Name() != "bash" {
		t.Fatalf("calls = %+v", calls)
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(calls[0].Arguments()), &args); err != nil {
		t.Fatalf("arguments 非 JSON: %v", err)
	}
	if args["command"] != "ls -la" {
		t.Errorf("command = %v", args["command"])
	}
	if args["timeout"] != float64(30) {
		t.Errorf("timeout = %v (%T)", args["timeout"], args["timeout"])
	}
	env, _ := args["env"].(map[string]interface{})
	if env["k"] != "v" {
		t.Errorf("env = %v", args["env"])
	}
	if cleaned != "" {
		t.Errorf("cleaned = %q", cleaned)
	}
}

func TestFallbackDSMLMultipleInvokes(t *testing.T) {
	m := &Message{Role: "assistant", Content: `开始处理。
<｜｜DSML｜｜tool_calls>
<｜｜DSML｜｜invoke name="read">
<｜｜DSML｜｜parameter name="path" string="true">a.go</｜｜DSML｜｜parameter>
</｜｜DSML｜｜invoke>
<｜｜DSML｜｜invoke name="read">
<｜｜DSML｜｜parameter name="path" string="true">b.go</｜｜DSML｜｜parameter>
</｜｜DSML｜｜invoke>
</｜｜DSML｜｜tool_calls>`}
	if !fallbackContentToolCalls(m) {
		t.Fatal("未提取到调用")
	}
	if len(m.ToolCalls) != 2 {
		t.Fatalf("ToolCalls = %d, want 2", len(m.ToolCalls))
	}
	if m.Content != "开始处理。" {
		t.Errorf("Content = %q", m.Content)
	}
	if m.ToolCalls[0].ID == "" || m.ToolCalls[0].Type != "function" {
		t.Errorf("ID/Type 未补齐: %+v", m.ToolCalls[0])
	}
}

func TestExtractDSMLSingleCall(t *testing.T) {
	text := "<｜｜DSML｜｜tool_call>\n{\"name\": \"bash\", \"command\": \"date -r 1787660651 '+%Y-%m-%d %H:%M:%S %Z'\", \"description\": \"Convert Unix timestamp\"}\n</｜｜DSML｜｜tool_call>"
	calls, cleaned := extractDSMLCalls(text)
	if len(calls) != 1 || calls[0].Name() != "bash" {
		t.Fatalf("calls = %+v", calls)
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(calls[0].Arguments()), &args); err != nil {
		t.Fatalf("arguments 非 JSON: %v (%s)", err, calls[0].Arguments())
	}
	if args["command"] != "date -r 1787660651 '+%Y-%m-%d %H:%M:%S %Z'" {
		t.Errorf("command = %v", args["command"])
	}
	if args["description"] != "Convert Unix timestamp" {
		t.Errorf("description = %v", args["description"])
	}
	if strings.TrimSpace(cleaned) != "" {
		t.Errorf("cleaned = %q", cleaned)
	}
}

func TestExtractDSMLSingleNestedArguments(t *testing.T) {
	m := &Message{Role: "assistant", Content: "我查一下。\n<｜｜DSML｜｜tool_call>\n{\"name\": \"read\", \"arguments\": {\"path\": \"a.go\"}}\n</｜｜DSML｜｜tool_call>"}
	if !fallbackContentToolCalls(m) {
		t.Fatal("未提取到调用")
	}
	if len(m.ToolCalls) != 1 || m.ToolCalls[0].Name() != "read" {
		t.Fatalf("ToolCalls = %+v", m.ToolCalls)
	}
	if !strings.Contains(m.ToolCalls[0].Arguments(), `"path":"a.go"`) {
		t.Errorf("Arguments = %s", m.ToolCalls[0].Arguments())
	}
	if strings.TrimSpace(m.Content) != "我查一下。" {
		t.Errorf("Content = %q", m.Content)
	}
}

func TestExtractDSMLSingleParameterForm(t *testing.T) {
	text := `<｜｜DSML｜｜tool_call>
<｜｜DSML｜｜parameter name="name">bash</｜｜DSML｜｜parameter>
<｜｜DSML｜｜parameter name="arguments">{"command": "date -r 1787660651 \"+%Y-%m-%d %H:%M:%S %Z\"", "description": "Convert timestamp"}</｜｜DSML｜｜parameter>
</｜｜DSML｜｜tool_call>`
	calls, cleaned := extractDSMLCalls(text)
	if len(calls) != 1 || calls[0].Name() != "bash" {
		t.Fatalf("calls = %+v", calls)
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(calls[0].Arguments()), &args); err != nil {
		t.Fatalf("arguments 非 JSON: %v (%s)", err, calls[0].Arguments())
	}
	if args["command"] != `date -r 1787660651 "+%Y-%m-%d %H:%M:%S %Z"` {
		t.Errorf("command = %v", args["command"])
	}
	if args["description"] != "Convert timestamp" {
		t.Errorf("description = %v", args["description"])
	}
	if strings.TrimSpace(cleaned) != "" {
		t.Errorf("cleaned = %q", cleaned)
	}
}

func TestFallbackDSMLInReasoning(t *testing.T) {
	m := &Message{Role: "assistant", Reasoning: "需要先看文件。<｜｜DSML｜｜invoke name=\"read\">\n<｜｜DSML｜｜parameter name=\"path\" string=\"true\">x.go</｜｜DSML｜｜parameter>\n</｜｜DSML｜｜invoke>"}
	if !fallbackContentToolCalls(m) {
		t.Fatal("未提取到调用")
	}
	if len(m.ToolCalls) != 1 || m.ToolCalls[0].Name() != "read" {
		t.Fatalf("ToolCalls = %+v", m.ToolCalls)
	}
	if m.Reasoning != "需要先看文件。" {
		t.Errorf("Reasoning = %q", m.Reasoning)
	}
}

// 竖线全半角/空格变体与流截断无闭合
func TestExtractDSMLVariants(t *testing.T) {
	cases := map[string]string{
		"ASCII竖线": "查一下。\n<||DSML||tool_call>\n{\"name\": \"read\", \"arguments\": {\"path\": \"a.go\"}}\n</||DSML||tool_call>",
		"混搭竖线":  "查一下。\n<|｜DSML｜|tool_call>\n{\"name\": \"read\", \"arguments\": {\"path\": \"a.go\"}}\n</|｜DSML｜|tool_call>",
		"闭合截断":  "查一下。\n<｜｜DSML｜｜tool_call>\n{\"name\": \"read\", \"arguments\": {\"path\": \"a.go\"}}\n</｜｜DSML｜｜tool_cal>",
		"无闭合":   "查一下。\n<｜｜DSML｜｜tool_call>\n{\"name\": \"read\", \"arguments\": {\"path\": \"a.go\"}}",
	}
	for name, text := range cases {
		calls, cleaned := extractDSMLCalls(text)
		if len(calls) != 1 || calls[0].Name() != "read" {
			t.Fatalf("%s：calls = %+v", name, calls)
		}
		if !strings.Contains(calls[0].Arguments(), "a.go") {
			t.Fatalf("%s：arguments = %s", name, calls[0].Arguments())
		}
		if strings.Contains(cleaned, "DSML") || strings.Contains(cleaned, "tool_call") {
			t.Fatalf("%s：cleaned = %q", name, cleaned)
		}
	}
}

// DSML 开标签配普通 </tool_call> 闭合（混搭反方向，用户实测）
func TestExtractDSMLOpenWithPlainCloser(t *testing.T) {
	text := "<｜｜DSML｜｜tool_call>\n{\"name\": \"bash\", \"arguments\": {\"command\": \"pwd && ls -la\", \"description\": \"确认当前目录并列出项目文件\"}}\n</tool_call>"
	m := &Message{Role: "assistant", Content: text}
	if !fallbackContentToolCalls(m) {
		t.Fatal("应识别出 DSML 开标签 + 普通闭合的调用")
	}
	if len(m.ToolCalls) != 1 || m.ToolCalls[0].Name() != "bash" {
		t.Fatalf("调用解析错误: %+v", m.ToolCalls)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(m.ToolCalls[0].Arguments()), &args); err != nil || args["command"] != "pwd && ls -la" {
		t.Fatalf("arguments = %s (err %v)", m.ToolCalls[0].Arguments(), err)
	}
	if strings.Contains(m.Content, "tool_call") || strings.Contains(m.Content, "DSML") {
		t.Fatalf("正文残留标签: %q", m.Content)
	}
}

// 闭合名称截断到 tool_c（用户实测）
func TestExtractDSMLShortTruncatedCloser(t *testing.T) {
	text := "<｜｜DSML｜｜tool_call>\n{\"name\": \"bash\", \"arguments\": {\"command\": \"pwd && echo '---' && ls -la\", \"description\": \"Confirm working directory and list contents\"}}\n</｜｜DSML｜｜tool_c>"
	m := &Message{Role: "assistant", Content: text}
	if !fallbackContentToolCalls(m) {
		t.Fatal("应识别出短截断闭合的调用")
	}
	if len(m.ToolCalls) != 1 || m.ToolCalls[0].Name() != "bash" {
		t.Fatalf("调用解析错误: %+v", m.ToolCalls)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(m.ToolCalls[0].Arguments()), &args); err != nil || args["command"] != "pwd && echo '---' && ls -la" {
		t.Fatalf("arguments = %s (err %v)", m.ToolCalls[0].Arguments(), err)
	}
	if strings.Contains(m.Content, "tool_c") || strings.Contains(m.Content, "DSML") {
		t.Fatalf("正文残留标签: %q", m.Content)
	}
}
