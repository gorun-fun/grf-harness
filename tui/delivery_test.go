// delivery_test.go 端到端回环：假 LLM（httptest）驱动一轮带 todo_write /
// create_goal 的回复，验证 todos/goal 事件送达 Chat 状态并出现在最终帧
// （事项面板 + 顶栏 goal）。Screen 未 Start，Paint 为 no-op，无需真实终端。
package tui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"grf/app"
	"grf/llm"
	"grf/term"
)

// probeLLM 模拟 OpenAI 接口（非流式回包，与 agent 包测试同格式）。
func probeLLM(responses []string) *httptest.Server {
	idx := -1
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx++
		if idx >= len(responses) {
			idx = len(responses) - 1
		}
		resp := responses[idx]
		var msg llm.Message
		if strings.HasPrefix(resp, "TOOL:") {
			body := strings.TrimPrefix(resp, "TOOL:")
			name := body
			for i := 0; i < len(body); i++ {
				if body[i] == ':' && i+1 < len(body) && body[i+1] == '{' {
					name, body = body[:i], body[i+1:]
					break
				}
			}
			msg = llm.Message{Role: "assistant",
				ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function",
					Function: llm.FunctionCall{Name: name, Arguments: body}}}}
		} else {
			msg = llm.Message{Role: "assistant", Content: resp}
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": msg}}})
	}))
}

func TestTodosGoalShownAboveInput(t *testing.T) {
	srv := probeLLM([]string{
		"TOOL:todo_write:{\"todos\":[{\"content\":\"任务A\",\"status\":\"in_progress\"},{\"content\":\"任务B\",\"status\":\"pending\"}]}",
		"TOOL:create_goal:{\"objective\":\"完成重构\"}",
		"done",
	})
	defer srv.Close()

	a, err := app.New(app.Options{
		Workspace: t.TempDir(),
		Client:    llm.NewClient(llm.Config{BaseURL: srv.URL, APIKey: "t", Model: "m"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	c := NewChat(a)
	c.ctx = context.Background()
	c.scr = term.NewScreen()
	c.width, c.height = 80, 24
	root := a.Root()
	root.SetStreamHooks(c.hub.content, c.hub.reasoning)
	root.SetOnEvent(c.onEvent)

	go func() {
		root.Run(c.ctx, "test")
		c.events <- runDoneMsg{}
	}()

	deadline := time.After(10 * time.Second)
	for {
		select {
		case msg := <-c.events:
			c.handleMsg(msg)
			if _, ok := msg.(runDoneMsg); ok {
				if len(c.todos) != 2 || c.goalText == "" {
					t.Fatalf("todos/goal 状态未送达: todos=%d goal=%q", len(c.todos), c.goalText)
				}
				lines, _, _, _ := c.frameLines()
				joined := strings.Join(lines, "\n")
				if !strings.Contains(joined, "事项") || !strings.Contains(joined, "goal: ") {
					t.Fatalf("最终帧缺少事项面板/goal 状态:\n%s", joined)
				}
				if !strings.Contains(joined, "轮 · 2 步") || !strings.Contains(joined, "LLM ") {
					t.Fatalf("底部横线缺少统计:\n%s", joined)
				}
				return
			}
		case <-deadline:
			t.Fatal("超时：runDoneMsg 未到达")
		}
	}
}
