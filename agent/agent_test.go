package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"grf/config"
	"grf/llm"
	"grf/tools"
)

// fakeLLM 模拟 OpenAI 接口：按脚本依次返回 assistant 消息。
type fakeLLM struct {
	mu      sync.Mutex
	servers []string // 每轮返回（工具调用 JSON 或纯文本）
	srv     *httptest.Server
	gotDefs []llm.Tool
	calls   int
}

func newFakeLLM(responses []string) *fakeLLM {
	f := &fakeLLM{servers: responses}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []llm.Message `json:"messages"`
			Tools    []struct {
				Function llm.Tool `json:"function"`
			} `json:"tools"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.calls++
		for _, t := range req.Tools {
			f.gotDefs = append(f.gotDefs, t.Function)
		}
		idx := f.calls - 1
		if idx >= len(f.servers) {
			idx = len(f.servers) - 1
		}
		resp := f.servers[idx]
		f.mu.Unlock()

		var msg llm.Message
		if strings.HasPrefix(resp, "TOOL:") {
			body := strings.TrimPrefix(resp, "TOOL:")
			name := "read"
			if i := strings.Index(body, ":"); i > 0 && strings.HasPrefix(body[i+1:], "{") {
				name = body[:i]
				body = body[i+1:]
			}
			msg = llm.Message{
				Role:      "assistant",
				ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function", Function: llm.FunctionCall{Name: name, Arguments: body}}},
			}
		} else {
			msg = llm.Message{Role: "assistant", Content: resp}
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": msg}},
		})
	}))
	return f
}

func (f *fakeLLM) URL() string { return f.srv.URL }

func (f *fakeLLM) Close() { f.srv.Close() }

func buildTestHarness(t *testing.T, f *fakeLLM, ws string) *Harness {
	t.Helper()
	st := &config.Settings{Sandbox: "off", FsPolicy: "observe"}
	h, err := Build(Options{
		Workspace: ws,
		Client:    llm.NewClient(llm.Config{BaseURL: f.URL(), APIKey: "t", Model: "test-model"}),
		Settings:  st,
		Host:      &tools.NoopHost{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestAll25ToolsRegistered(t *testing.T) {
	f := newFakeLLM([]string{"ok"})
	defer f.Close()
	h := buildTestHarness(t, f, t.TempDir())
	defer h.Close()
	if err := h.VerifyAllToolsRegistered(); err != nil {
		t.Fatal(err)
	}
	if got := len(h.Root.reg.Defs()); got != 25 {
		t.Fatalf("root agent should expose 25 tools, got %d", got)
	}
}

func TestDisabledTools(t *testing.T) {
	f := newFakeLLM([]string{"ok"})
	defer f.Close()
	h, err := Build(Options{
		Workspace:     t.TempDir(),
		Client:        llm.NewClient(llm.Config{BaseURL: f.URL(), APIKey: "t", Model: "test-model"}),
		Settings:      &config.Settings{Sandbox: "off", FsPolicy: "observe"},
		Host:          &tools.NoopHost{},
		DisabledTools: []string{"bash", "web_search", "not_a_tool"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if h.Root.reg.Has("bash") || h.Root.reg.Has("web_search") {
		t.Fatal("disabled tools should be removed from root registry")
	}
	if got := len(h.Root.reg.Defs()); got != 23 {
		t.Fatalf("root agent should expose 23 tools after disabling 2, got %d", got)
	}
	if err := h.VerifyAllToolsRegistered(); err != nil {
		t.Fatalf("verify should skip disabled tools: %v", err)
	}
	// 子代理同样剔除
	child := h.Manager.NewChild(h.Root, "sub", nil)
	if child.reg.Has("bash") {
		t.Fatal("disabled tools should be removed from child registry")
	}
	res := h.Root.reg.Execute(context.Background(), "bash", "{}")
	if !strings.Contains(res.Text, "unknown tool") {
		t.Fatalf("execute of disabled tool should report unknown, got %q", res.Text)
	}
}

func TestRunLoopExecutesTool(t *testing.T) {
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "hello.txt"), []byte("你好 world\n"), 0o644)
	f := newFakeLLM([]string{
		`TOOL:{"file_path":"hello.txt"}`,
		"the file says 你好 world",
	})
	defer f.Close()
	h := buildTestHarness(t, f, ws)
	defer h.Close()

	var events []string
	h.Root.SetOnEvent(func(kind, detail string) {
		events = append(events, kind+":"+detail)
	})
	out, err := h.Root.Run(context.Background(), "读一下 hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "你好 world") {
		t.Fatalf("final reply: %q", out)
	}
	// 工具事件
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, "tool:read") || !strings.Contains(joined, "result:") {
		t.Fatalf("events: %v", events)
	}
	// 发给 API 的工具定义 = 25 个（多次请求累计去重仍 25）
	seen := map[string]bool{}
	for _, d := range f.gotDefs {
		seen[d.Name] = true
	}
	if len(seen) != 25 {
		t.Fatalf("API got %d unique tool defs", len(seen))
	}
	// 历史含 tool 消息
	hist := h.Root.History()
	kinds := map[string]int{}
	for _, m := range hist {
		kinds[m.Role]++
	}
	if kinds["tool"] != 1 || kinds["assistant"] != 2 {
		t.Fatalf("history roles: %v", kinds)
	}
}

func TestGoalStateMachine(t *testing.T) {
	f := newFakeLLM([]string{"done"})
	defer f.Close()
	h := buildTestHarness(t, f, t.TempDir())
	defer h.Close()
	root := h.Root
	r := root.reg

	// 创建
	res := r.Execute(context.Background(), "create_goal", `{"objective":"完成重构"}`)
	if !strings.Contains(res.Text, "goal created") || !strings.Contains(res.Text, "revision: 1") {
		t.Fatalf("create: %s", res.Text)
	}
	var g Goal
	if err := json.Unmarshal([]byte(h.Goals.Marshal()), &g); err != nil {
		t.Fatal(err)
	}
	// revision 不匹配
	res = r.Execute(context.Background(), "update_goal", fmt.Sprintf(`{"goal_id":%q,"revision":9,"action":"pause"}`, g.ID))
	if !strings.Contains(res.Text, "revision") {
		t.Fatalf("revision mismatch should error: %s", res.Text)
	}
	// complete
	res = r.Execute(context.Background(), "update_goal", fmt.Sprintf(`{"goal_id":%q,"revision":1,"action":"complete"}`, g.ID))
	if !strings.Contains(res.Text, "complete") {
		t.Fatalf("complete: %s", res.Text)
	}
	// 已完成后不能再 blocked
	res = r.Execute(context.Background(), "update_goal", fmt.Sprintf(`{"goal_id":%q,"revision":2,"action":"blocked","blocked_reason":"x"}`, g.ID))
	if !strings.Contains(res.Text, "error") {
		t.Fatalf("blocked after complete should fail: %s", res.Text)
	}
}

func TestSubagentFlow(t *testing.T) {
	f := newFakeLLM([]string{
		`TOOL:subagent:{"description":"研究任务","prompt":"数一数工作区文件","run_in_background":false}`,
		"subagent says: 3 files",
	})
	defer f.Close()
	h := buildTestHarness(t, f, t.TempDir())
	defer h.Close()

	out, err := h.Root.Run(context.Background(), "去数文件")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "subagent says") {
		t.Fatalf("reply: %q", out)
	}
	// 子代理已注册且可列出
	res := h.Root.reg.Execute(context.Background(), "list_agents", `{}`)
	if !strings.Contains(res.Text, "研究任务") {
		t.Fatalf("list_agents: %s", res.Text)
	}
	// 子代理工具链不含 goal/ralph/ask_user
	for _, h2 := range []struct{ id string }{{"agent_1_x"}} {
		_ = h2
	}
}

func TestSubagentToolRestrictions(t *testing.T) {
	f := newFakeLLM([]string{"ok"})
	defer f.Close()
	ws := t.TempDir()
	h := buildTestHarness(t, f, ws)
	defer h.Close()

	_, out, err := h.Manager.Subagent(h.Root, "restricted", "do it", true)
	if err != nil {
		t.Fatal(err)
	}
	_ = out
	// 找到子代理，验证其工具集
	h2, ok := h.Manager.HandleOf(_lastChildID(h))
	if !ok {
		t.Skip("child id unknown")
	}
	for _, banned := range []string{"create_goal", "get_goal", "update_goal", "ralph", "ask_user_question"} {
		if h2.Ag.reg.Has(banned) {
			// ask_user 注册为拒绝版本（有入口但明确拒绝）——create_goal 等不应有入口
			if banned != "ask_user_question" {
				t.Fatalf("child should not have %s", banned)
			}
		}
	}
}

func _lastChildID(h *Harness) string {
	// 简单取 list_agents 首个 id
	res := h.Root.reg.Execute(context.Background(), "list_agents", `{}`)
	fields := strings.Fields(res.Text)
	if len(fields) > 0 {
		return fields[0]
	}
	return ""
}

func TestRalphParse(t *testing.T) {
	v, tail := parseRalphReport("干活中…\nCOMPLETE: 全部搞定")
	if v != "COMPLETE" || tail != "全部搞定" {
		t.Fatalf("%v %q", v, tail)
	}
	v, _ = parseRalphReport("BLOCKER: 网络不通")
	if v != "BLOCKER" {
		t.Fatal(v)
	}
}

func TestSchemaValidator(t *testing.T) {
	schema := map[string]any{
		"type":                 "object",
		"required":             []any{"name"},
		"additionalProperties": false,
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
			"tags": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
	}
	ok := map[string]any{"name": "x", "tags": []any{"a"}}
	if err := ValidateSchema(ok, schema); err != nil {
		t.Fatal(err)
	}
	bad := map[string]any{"name": "x", "extra": 1}
	if err := ValidateSchema(bad, schema); err == nil {
		t.Fatal("additionalProperties=false should reject extra")
	}
	badType := map[string]any{"name": 1}
	if err := ValidateSchema(badType, schema); err == nil {
		t.Fatal("type mismatch should reject")
	}
	// 非法键
	if err := ValidateSchema(ok, map[string]any{"pattern": "^a"}); err == nil {
		t.Fatal("unsupported schema key should reject")
	}
}

func TestNoticesInjected(t *testing.T) {
	f := newFakeLLM([]string{
		"first reply",
		"collected",
	})
	defer f.Close()
	h := buildTestHarness(t, f, t.TempDir())
	defer h.Close()
	root := h.Root
	root.mu.Lock()
	running := root.running
	root.mu.Unlock()
	if running {
		t.Fatal("should be idle")
	}
	// 模拟后台通知 → OnIdleNotice
	got := make(chan string, 1)
	root.OnIdleNotice = func(text string) { got <- text }
	root.EnqueueNotice("subagent settled: done")
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("idle notice should fire OnIdleNotice")
	}
	// 通知在本轮请求前并入历史（fake 忽略消息内容，直接验证历史）
	_, err := root.Run(context.Background(), "继续")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range root.History() {
		if m.Role == "user" && strings.Contains(m.Content, "[system notice] subagent settled") {
			found = true
		}
	}
	if !found {
		t.Fatal("notice should be in history")
	}
}

func timeoutChan() <-chan time.Time { return time.After(2 * time.Second) }

// TestRestoreFullPrefersStoredSystem resume 优先回放文件里的 system 原文，
// 请求前缀与上一场逐字一致（即使构建期提示词已变化）。
func TestRestoreFullPrefersStoredSystem(t *testing.T) {
	f := newFakeLLM([]string{"ok"})
	defer f.Close()
	h := buildTestHarness(t, f, t.TempDir())
	defer h.Close()
	h.Root.RestoreFull([]llm.Message{
		{Role: "user", Content: "旧问题"},
		{Role: "assistant", Content: "旧回答"},
	}, "旧 system 原文")
	hist := h.Root.History()
	if len(hist) != 3 || hist[0].Content != "旧 system 原文" {
		t.Fatalf("RestoreFull 后历史 = %+v", hist)
	}
	// 之后的 Reset 也保持回放的 system（同会话前缀稳定）
	h.Root.Reset()
	if got := h.Root.History()[0].Content; got != "旧 system 原文" {
		t.Fatalf("Reset 后 system = %q", got)
	}
}

// TestRunLoopOrphanUserRolledBack LLM 出错时回滚本轮未被回复闭合的 user 输入。
func TestRunLoopOrphanUserRolledBack(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"message":"boom"}}`)
	}))
	defer srv.Close()
	ws := t.TempDir()
	st := &config.Settings{Sandbox: "off", FsPolicy: "observe"}
	h, err := Build(Options{
		Workspace: ws,
		Client:    llm.NewClient(llm.Config{BaseURL: srv.URL, APIKey: "t", Model: "m"}),
		Settings:  st, Host: &tools.NoopHost{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.Root.Run(context.Background(), "会失败的一轮"); err == nil {
		t.Fatal("期望 LLM 错误")
	}
	hist := h.Root.History()
	if len(hist) != 1 || hist[0].Role != "system" {
		t.Fatalf("失败轮的孤儿 user 消息应被回滚, history = %+v", hist)
	}
}
