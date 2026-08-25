package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFallbackAttrForm(t *testing.T) {
	// 用户实测的畸形属性形式（arguments 用冒号、值为裸 JSON）
	m := Message{Content: `我来读取该文件。<tool_call name="read" arguments: { "file_path": "/Users/wenjun/AiProjects/test/src/snake.js" } />`}
	if !fallbackContentToolCalls(&m) {
		t.Fatal("应识别出属性形式工具调用")
	}
	if len(m.ToolCalls) != 1 || m.ToolCalls[0].Name() != "read" {
		t.Fatalf("调用解析错误: %+v", m.ToolCalls)
	}
	if want := `{ "file_path": "/Users/wenjun/AiProjects/test/src/snake.js" }`; m.ToolCalls[0].Arguments() != want {
		t.Fatalf("arguments = %q, want %q", m.ToolCalls[0].Arguments(), want)
	}
	if m.ToolCalls[0].ID == "" || m.ToolCalls[0].Type != "function" {
		t.Fatalf("缺 ID/Type: %+v", m.ToolCalls[0])
	}
	if m.Content != `我来读取该文件。` {
		t.Fatalf("正文未剔除标签: %q", m.Content)
	}
}

func TestFallbackBodyForm(t *testing.T) {
	// Qwen 标准 JSON 体形式，含嵌套对象参数
	m := Message{Content: "<tool_call>\n{\"name\": \"write\", \"arguments\": {\"file_path\": \"a.go\", \"content\": \"x{1}\"}}\n</tool_call>"}
	if !fallbackContentToolCalls(&m) {
		t.Fatal("应识别出正文形式工具调用")
	}
	if len(m.ToolCalls) != 1 || m.ToolCalls[0].Name() != "write" {
		t.Fatalf("调用解析错误: %+v", m.ToolCalls)
	}
	if m.ToolCalls[0].Arguments() == "" || m.Content != "" {
		t.Fatalf("arguments=%q content=%q", m.ToolCalls[0].Arguments(), m.Content)
	}
}

func TestFallbackDedupWithProtocolCalls(t *testing.T) {
	// 服务端半解析：glob 已进协议层，其余留在正文；重复项跳过、其余补齐
	m := Message{
		Content: "<tool_call>\n{\"name\": \"read\", \"arguments\": {\"file_path\": \"AGENTS.md\"}}\n</tool_call>\n" +
			"<tool_call>\n{\"name\": \"glob\", \"arguments\": {\"pattern\": \"src/**/*\"}}\n</tool_call>",
		ToolCalls: []ToolCall{{ID: "call_192-192-4", Type: "function",
			Function: FunctionCall{Name: "glob", Arguments: "{\"pattern\": \"src/**/*\"}"}}},
	}
	if !fallbackContentToolCalls(&m) {
		t.Fatal("应补齐正文里未被协议层解析的调用")
	}
	if len(m.ToolCalls) != 2 {
		t.Fatalf("应剩 2 个调用（glob 去重 + read 补齐），got %d", len(m.ToolCalls))
	}
	if m.ToolCalls[1].Name() != "read" {
		t.Fatalf("补齐的应是 read: %+v", m.ToolCalls)
	}
}

func TestFallbackNoTag(t *testing.T) {
	m := Message{Content: "普通回复，没有标签"}
	if fallbackContentToolCalls(&m) {
		t.Fatal("误报")
	}
}

func TestFallbackInReasoning(t *testing.T) {
	// 工具调用写在思考流里、正文为空
	m := Message{Reasoning: "用户要读文件，我调用 read。\n<tool_call>\n{\"name\": \"read\", \"arguments\": {\"file_path\": \"a.go\"}}\n</tool_call>\n"}
	if !fallbackContentToolCalls(&m) {
		t.Fatal("应从思考内容中识别出工具调用")
	}
	if len(m.ToolCalls) != 1 || m.ToolCalls[0].Name() != "read" {
		t.Fatalf("调用解析错误: %+v", m.ToolCalls)
	}
	if strings.Contains(m.Reasoning, "<tool_call>") {
		t.Fatalf("思考内容未剔除标签: %q", m.Reasoning)
	}
}

func TestFallbackReasoningAndContentDedup(t *testing.T) {
	// 同一调用同时出现在思考与正文：只执行一次
	m := Message{
		Content:   "<tool_call>\n{\"name\": \"read\", \"arguments\": {\"file_path\": \"a.go\"}}\n</tool_call>",
		Reasoning: "<tool_call>\n{\"name\": \"read\", \"arguments\": {\"file_path\": \"a.go\"}}\n</tool_call>",
	}
	if !fallbackContentToolCalls(&m) {
		t.Fatal("应识别出工具调用")
	}
	if len(m.ToolCalls) != 1 {
		t.Fatalf("重复调用未去重: %+v", m.ToolCalls)
	}
}

func TestFallbackGarbageTailArgs(t *testing.T) {
	// 参数 JSON 后面被模型追加了垃圾尾巴，应提取第一个配平对象
	m := Message{Content: `<tool_call name="bash" arguments={"command": "mkdir -p src", "description": "Create src directory", "workdir": "/Users/wenjun/AiProjects/test/sokoban"},"name":"bash"} />`}
	if !fallbackContentToolCalls(&m) {
		t.Fatal("应识别出工具调用")
	}
	if len(m.ToolCalls) != 1 || m.ToolCalls[0].Name() != "bash" {
		t.Fatalf("调用解析错误: %+v", m.ToolCalls)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(m.ToolCalls[0].Arguments()), &args); err != nil {
		t.Fatalf("提取的参数不是合法 JSON: %v (%q)", err, m.ToolCalls[0].Arguments())
	}
	if args["command"] != "mkdir -p src" {
		t.Fatalf("command = %v", args["command"])
	}
}

func TestFallbackArgsWithRedirect(t *testing.T) {
	// 参数 JSON 内含 ">"（shell 重定向）不能被截断
	m := Message{Content: `<tool_call name="bash" arguments={"command": "echo hi > out.txt"} />`}
	if !fallbackContentToolCalls(&m) {
		t.Fatal("应识别出工具调用")
	}
	if !strings.Contains(m.ToolCalls[0].Arguments(), "> out.txt") {
		t.Fatalf("参数被截断: %q", m.ToolCalls[0].Arguments())
	}
}
