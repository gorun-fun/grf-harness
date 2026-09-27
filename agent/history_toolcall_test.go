// history_toolcall_test.go 带图工具结果与历史损伤的 tool_calls 闭合回归：
// GLM 等严格供应商逐条校验 assistant.tool_calls 的紧后消息必须闭合全部
// tool_call_id，历史里任何"未闭合/隔位闭合"都会让整单请求 400。
package agent

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/llm"
	"git.gorun.fun/share/grf-harness/tools"
)

func buildToolCallHarness(t *testing.T, baseURL string) *Harness {
	t.Helper()
	h, err := Build(Options{
		Workspace: t.TempDir(),
		Client:    llm.NewClient(llm.Config{BaseURL: baseURL, APIKey: "t", Model: "m"}),
		Settings:  &config.Settings{Policy: "write-workspace"},
		Host:      &tools.NoopHost{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// 带图工具结果应落两条：tool 文本消息闭合 tool_call_id + user 伪消息回传图像。
func TestAppendToolResultImageClosesCall(t *testing.T) {
	h := buildToolCallHarness(t, "http://127.0.0.1:1")
	h.Root.appendToolResult(
		llm.ToolCall{ID: "c1", Type: "function", Function: llm.FunctionCall{Name: "screenshot"}},
		tools.Result{Images: []tools.Image{{Mime: "image/png", B64: "aGk="}}},
	)
	hist := h.Root.History()
	if len(hist) < 2 {
		t.Fatalf("应追加两条消息,实际 %d 条", len(hist))
	}
	closing, imgMsg := hist[len(hist)-2], hist[len(hist)-1]
	if closing.Role != "tool" || closing.ToolCallID != "c1" || closing.Content == "" {
		t.Fatalf("带图结果应先落非空 tool 消息闭合调用,实际: %+v", closing)
	}
	if imgMsg.Role != "user" || len(imgMsg.ContentParts) == 0 ||
		!strings.Contains(imgMsg.ContentParts[0].Text, "[tool result: screenshot]") {
		t.Fatalf("图像应经 user 伪消息回传,实际: %+v", imgMsg)
	}
}

func TestCanonicalizeToolResults(t *testing.T) {
	asst := llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{
		{ID: "c1", Function: llm.FunctionCall{Name: "shot"}},
	}}
	pseudo := llm.Message{Role: "user", ContentParts: []llm.Part{
		llm.TextPart("[tool result: shot] ok"), llm.ImagePart("image/png", "aGk="),
	}}
	tool := llm.Message{Role: "tool", ToolCallID: "c1", Content: "ok"}
	fin := llm.Message{Role: "assistant", Content: "done"}

	cases := []struct {
		name string
		in   []llm.Message
		want []string // 消息签名:assistant / tool:<id> / pseudo / 其余按 role
	}{
		{"旧版伪消息未闭合", []llm.Message{asst, pseudo, fin},
			[]string{"assistant", "tool:c1", "pseudo", "assistant"}},
		{"闭合错位到队尾", []llm.Message{asst, pseudo, tool, fin},
			[]string{"assistant", "tool:c1", "pseudo", "assistant"}},
		{"已规范原样", []llm.Message{asst, tool, fin},
			[]string{"assistant", "tool:c1", "assistant"}},
		{"队尾悬挂(暂停态快照)", []llm.Message{asst},
			[]string{"assistant", "tool:c1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := canonicalizeToolResults(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("长度 %d != 期望 %d: %+v", len(got), len(c.want), got)
			}
			for i, g := range got {
				sig := g.Role
				if g.Role == "tool" {
					sig = "tool:" + g.ToolCallID
				} else if g.Role == "user" && len(g.ContentParts) > 0 {
					sig = "pseudo"
				}
				if sig != c.want[i] {
					t.Fatalf("第 %d 条签名 %q != 期望 %q(全部: %+v)", i, sig, c.want[i], got)
				}
			}
		})
	}
}

// 旧版带图历史(assistant tool_calls 后只跟 user 伪消息)恢复后继续对话,
// 发出的请求必须满足:每条带 tool_calls 的 assistant 紧后是闭合其 id 的
// tool 消息。
func TestLegacyImageHistoryPassesStrictToolCallCheck(t *testing.T) {
	var msgs [][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Role       string `json:"role"`
				ToolCallID string `json:"tool_call_id"`
				ToolCalls  []struct {
					ID string `json:"id"`
				} `json:"tool_calls"`
			}
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("请求体解析失败: %v", err)
		}
		msgs = msgs[:0]
		for _, m := range req.Messages {
			if m.Role == "assistant" && len(m.ToolCalls) > 0 {
				msgs = append(msgs, []string{"assistant", m.ToolCalls[0].ID})
			} else {
				msgs = append(msgs, []string{m.Role, m.ToolCallID})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": llm.Message{Role: "assistant", Content: "ok"}}},
		})
	}))
	defer srv.Close()

	h := buildToolCallHarness(t, srv.URL)
	h.Root.RestoreFull([]llm.Message{
		{Role: "user", Content: "看图"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: "c9", Function: llm.FunctionCall{Name: "read_image"}},
		}},
		{Role: "user", ContentParts: []llm.Part{
			llm.TextPart("[tool result: read_image] ok"), llm.ImagePart("image/png", "aGk="),
		}},
		{Role: "assistant", Content: "图里是一只猫"},
	}, "")
	if _, err := h.Root.Run(t.Context(), "继续"); err != nil {
		t.Fatalf("旧版带图历史应能继续对话: %v", err)
	}
	for i, m := range msgs {
		if m[0] != "assistant" || m[1] == "" {
			continue
		}
		if i+1 >= len(msgs) || msgs[i+1][0] != "tool" || msgs[i+1][1] != m[1] {
			t.Fatalf("assistant tool_calls(%s) 紧后缺少闭合 tool 消息: %+v", m[1], msgs)
		}
	}
}
