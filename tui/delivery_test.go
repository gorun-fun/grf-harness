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

	"git.gorun.fun/share/grf-harness/app"
	"git.gorun.fun/share/grf-harness/llm"
	"git.gorun.fun/share/grf-harness/term"
	"git.gorun.fun/share/grf-harness/tools"
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

// TestApproveEachModalAppears 审批模式：工具调用必须弹出审批模态并可批准，
// 否则回合会永久挂起（回归：handleMsg 曾遗漏 *toolApproveRequest 分发）。
func TestApproveEachModalAppears(t *testing.T) {
	srv := probeLLM([]string{
		"TOOL:bash:{\"command\":\"echo hi\"}",
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
	a.SetHost(newTUIHost(c)) // 真实运行时由 Chat.Run 接管
	root.SetApproveMode(true)

	go func() {
		root.Run(c.ctx, "test")
		c.events <- runDoneMsg{}
	}()

	approved := false
	deadline := time.After(10 * time.Second)
	for {
		select {
		case msg := <-c.events:
			c.handleMsg(msg)
			switch msg.(type) {
			case *toolApproveRequest:
				if c.ask == nil {
					t.Fatal("审批请求未安装 ask 表单")
				}
				frameLines, _, _, _ := c.frameLines()
				joined := strings.Join(frameLines, "\n")
				if !strings.Contains(joined, "工具审批") || !strings.Contains(joined, "批准执行") {
					t.Fatalf("帧内缺少工具审批选项表单:\n%s", joined)
				}
				c.askKey(termKey{Name: "enter"}) // 默认选中"批准执行"
				approved = true
			case runDoneMsg:
				if !approved {
					t.Fatal("回合结束但审批模态从未出现")
				}
				return
			}
		case <-deadline:
			t.Fatal("超时：runDoneMsg 未到达（审批请求可能被丢弃）")
		}
	}
}

// TestReplyTrailingNewlinesTrimmed 回复正文尾部换行（如 deepseek v4 flash
// 常多发 \n）不应渲染出多余空行：回复与状态分隔线之间至多一个空行。
func TestReplyTrailingNewlinesTrimmed(t *testing.T) {
	srv := probeLLM([]string{"最终回复。\n\n"})
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
	c.running = true // 真实路径由 startRun 设置；finishStream 据此固化块

	go func() {
		root.Run(c.ctx, "test")
		c.events <- runDoneMsg{}
	}()

	deadline := time.After(10 * time.Second)
	for {
		select {
		case msg := <-c.events:
			c.handleMsg(msg)
			if _, ok := msg.(runDoneMsg); !ok {
				continue
			}
			lines, _, _, _ := c.frameLines()
			idx := -1
			for i, ln := range lines {
				if strings.Contains(ln, "最终回复。") {
					idx = i
				}
			}
			if idx < 0 {
				t.Fatalf("帧内缺少回复文本:\n%q", lines)
			}
			blanks := 0
			for i := idx + 1; i < len(lines); i++ {
				if strings.TrimSpace(lines[i]) == "" {
					blanks++
					continue
				}
				break
			}
			if blanks > 1 {
				t.Fatalf("回复后出现 %d 个空行（应为 1）:\n%q", blanks, lines[idx:])
			}
			return
		case <-deadline:
			t.Fatal("超时：runDoneMsg 未到达")
		}
	}
}

// TestToolApproveFeedbackMode 反馈路径：光标移到"✎ 自定义输入"行 → 回车手输
// → 回车提交，应作为拒绝并携带反馈。
func TestToolApproveFeedbackMode(t *testing.T) {
	a, err := app.New(app.Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	c := NewChat(a)
	c.width, c.height = 80, 24
	c.scr = term.NewScreen()
	resp := make(chan tools.ApproveDecision, 1)
	c.hostRequest(&toolApproveRequest{name: "bash", args: `{"command":"rm -rf x"}`, resp: resp})
	if c.ask == nil {
		t.Fatal("工具审批表单未安装")
	}
	c.askKey(termKey{Name: "down"})  // 拒绝
	c.askKey(termKey{Name: "down"})  // ✎ 自定义输入
	c.askKey(termKey{Name: "enter"}) // 切手输模式
	c.askKey(termKey{Name: "", Rune: '别'})
	c.askKey(termKey{Name: "", Rune: '删'})
	c.askKey(termKey{Name: "enter"})
	select {
	case d := <-resp:
		if d.Approved || d.Feedback != "别删" {
			t.Fatalf("反馈拒绝结果不符: %+v", d)
		}
	default:
		t.Fatal("回车后未回传审批决定")
	}
}

// TestBangDirectExec "!" 是命令模式按键：空输入框按 ! 进入模式（不入内容），
// 所输命令回车直执行——不经模型，原始输出入帧且不带模型向 exit code 标注。
func TestBangDirectExec(t *testing.T) {
	a, err := app.New(app.Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	c := NewChat(a)
	c.ctx = context.Background()
	c.scr = term.NewScreen()
	c.width, c.height = 80, 24

	c.handleKey(termKey{Name: "", Rune: '!'})
	if !c.bang || c.ed.Value() != "" || c.ed.Prompt != "! " {
		t.Fatalf("! 未进入命令模式: bang=%v value=%q prompt=%q", c.bang, c.ed.Value(), c.ed.Prompt)
	}
	for _, r := range "echo smoke-bang" {
		c.handleKey(termKey{Name: "", Rune: r})
	}
	c.handleKey(termKey{Name: "enter"})
	if !c.bang || c.ed.Prompt != "! " || c.ed.Len() != 0 {
		t.Fatalf("发送后应留在命令模式且清空输入: bang=%v prompt=%q len=%d", c.bang, c.ed.Prompt, c.ed.Len())
	}

	deadline := time.After(10 * time.Second)
	for {
		select {
		case msg := <-c.events:
			c.handleMsg(msg)
			if _, ok := msg.(bangDoneMsg); !ok {
				continue
			}
			if c.running {
				t.Fatal("直执行落定后 running 未复位")
			}
			if n := len(a.Root().History()); n != 1 {
				t.Fatalf("直执行不应写入会话历史（仅 system，实际 %d 条）", n)
			}
			lines, _, _, _ := c.frameLines()
			out := false
			for _, ln := range lines {
				if strings.TrimSpace(ln) == "smoke-bang" {
					out = true
				}
				if strings.Contains(ln, "[exit code:") {
					t.Fatalf("帧内不应出现模型向 exit code 标注:\n%q", lines)
				}
			}
			if !out {
				t.Fatalf("帧内缺少直执行原始输出行:\n%q", lines)
			}
			return
		case <-deadline:
			t.Fatal("超时：bangDoneMsg 未到达")
		}
	}
}

// TestBangToggleExit 命令模式由 ! 切换进出：空输入框再按 ! 才退出，
// Esc/退格不退出；已有内容时 ! 是普通字符。
func TestBangToggleExit(t *testing.T) {
	a, err := app.New(app.Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	c := NewChat(a)
	c.ctx = context.Background()
	c.scr = term.NewScreen()
	c.width, c.height = 80, 24

	c.handleKey(termKey{Name: "", Rune: '!'})
	if !c.bang {
		t.Fatal("! 未进入命令模式")
	}
	c.handleKey(termKey{Name: "esc"})
	c.handleKey(termKey{Name: "backspace"})
	if !c.bang {
		t.Fatal("Esc/退格不应退出命令模式")
	}
	c.handleKey(termKey{Name: "", Rune: '!'})
	if c.bang || c.ed.Len() != 0 || c.ed.Prompt != "> " {
		t.Fatalf("空输入框再按 ! 应退出: bang=%v len=%d prompt=%q", c.bang, c.ed.Len(), c.ed.Prompt)
	}

	// 已有内容时 ! 是普通字符（不误退出）
	c.handleKey(termKey{Name: "", Rune: '!'}) // 再进模式
	c.handleKey(termKey{Name: "", Rune: 'a'})
	c.handleKey(termKey{Name: "", Rune: '!'})
	if !c.bang || c.ed.Value() != "a!" {
		t.Fatalf("内容中的 ! 应为普通字符: bang=%v value=%q", c.bang, c.ed.Value())
	}
	select {
	case msg := <-c.events:
		t.Fatalf("不应有任何执行事件，收到: %T", msg)
	default:
	}
}
