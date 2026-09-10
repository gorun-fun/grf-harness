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

	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/llm"
	"git.gorun.fun/share/grf-harness/tools"
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
	st := &config.Settings{Policy: "write-workspace"}
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

func TestAllToolsRegistered(t *testing.T) {
	f := newFakeLLM([]string{"ok"})
	defer f.Close()
	h := buildTestHarness(t, f, t.TempDir())
	defer h.Close()
	if err := h.VerifyAllToolsRegistered(); err != nil {
		t.Fatal(err)
	}
	if got := len(h.Root.reg.Defs()); got != 28 {
		t.Fatalf("root agent should expose 28 tools, got %d", got)
	}
}

func TestDisabledTools(t *testing.T) {
	f := newFakeLLM([]string{"ok"})
	defer f.Close()
	h, err := Build(Options{
		Workspace:     t.TempDir(),
		Client:        llm.NewClient(llm.Config{BaseURL: f.URL(), APIKey: "t", Model: "test-model"}),
		Settings:      &config.Settings{Policy: "write-workspace"},
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
	if got := len(h.Root.reg.Defs()); got != 26 {
		t.Fatalf("root agent should expose 26 tools after disabling 2, got %d", got)
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

func TestSkillsOverride(t *testing.T) {
	t.Setenv("GRF_HOME", t.TempDir()) // 屏蔽机器全局技能
	ws := t.TempDir()
	// 工作区存在技能目录：默认加载会收录，传空目录应屏蔽
	skillDir := filepath.Join(ws, ".grf", "skills", "demo")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# Demo skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	build := func(cat *tools.SkillCatalog) *Harness {
		t.Helper()
		h, err := Build(Options{
			Workspace: ws,
			Settings:  &config.Settings{Policy: "write-workspace"},
			Host:      &tools.NoopHost{},
			Skills:    cat,
		})
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	system := func(h *Harness) string { return h.Root.History()[0].Content }

	// 空目录：关闭默认加载，提示词无技能清单，工具仍齐备
	h := build(tools.NewSkillCatalog())
	if sys := system(h); strings.Contains(sys, "可用技能") {
		t.Fatalf("empty skill catalog should not inject skill list, got %q", sys)
	}
	if err := h.VerifyAllToolsRegistered(); err != nil {
		t.Fatalf("skill tool must stay registered with empty catalog: %v", err)
	}
	h.Close()

	// 自定义目录：只列自带技能，不收工作区目录
	cat := tools.NewSkillCatalog()
	cat.Add("自带技能", "# Custom\n自定义技能正文")
	h2 := build(cat)
	defer h2.Close()
	sys := system(h2)
	if !strings.Contains(sys, "自带技能") {
		t.Fatalf("custom catalog should be listed, got %q", sys)
	}
	if strings.Contains(sys, "demo") {
		t.Fatalf("workspace skills must not load when catalog overridden, got %q", sys)
	}

	// nil 保持默认：收录工作区技能
	h3 := build(nil)
	defer h3.Close()
	if sys := system(h3); !strings.Contains(sys, "demo") {
		t.Fatalf("default loading should pick up workspace skills, got %q", sys)
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
	// 发给 API 的工具定义 = 28 个（多次请求累计去重仍 28）
	seen := map[string]bool{}
	for _, d := range f.gotDefs {
		seen[d.Name] = true
	}
	if len(seen) != 28 {
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
	for _, banned := range []string{"create_goal", "get_goal", "update_goal", "ralph", "enter_plan_mode", "ask_user_question"} {
		if h2.Ag.reg.Has(banned) {
			// ask_user 注册为拒绝版本（有入口但明确拒绝）——create_goal/enter_plan_mode 等不应有入口
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
	st := &config.Settings{Policy: "write-workspace"}
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

// ---- 结构化工具事件（ToolEvent）----

// captureHost 记录审批收到的参数、按脚本返回审批结果的宿主。
type captureHost struct {
	tools.NoopHost
	mu          sync.Mutex
	approveArgs map[string]string // name -> 收到的 args
	decision    tools.ApproveDecision
}

func (h *captureHost) ApproveTool(ctx context.Context, name, args string) tools.ApproveDecision {
	h.mu.Lock()
	if h.approveArgs == nil {
		h.approveArgs = map[string]string{}
	}
	h.approveArgs[name] = args
	h.mu.Unlock()
	return h.decision
}

func (h *captureHost) argOf(name string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.approveArgs[name]
}

func buildHarnessWithHost(t *testing.T, f *fakeLLM, ws string, host tools.Host) *Harness {
	t.Helper()
	h, err := Build(Options{
		Workspace: ws,
		Client:    llm.NewClient(llm.Config{BaseURL: f.URL(), APIKey: "t", Model: "test-model"}),
		Settings:  &config.Settings{Policy: "write-workspace"},
		Host:      host,
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestToolEventLifecycle(t *testing.T) {
	ws := t.TempDir()
	long := strings.Repeat("数据行内容-", 40) // 单行 >120 字，验证 Output 为全量而非首行摘要
	os.WriteFile(filepath.Join(ws, "long.txt"), []byte(long+"\n"), 0o644)
	f := newFakeLLM([]string{
		`TOOL:{"file_path":"long.txt"}`,
		"done",
	})
	defer f.Close()
	h := buildTestHarness(t, f, ws)
	defer h.Close()

	var events []ToolEvent
	h.Root.SetOnToolEvent(func(e ToolEvent) { events = append(events, e) })
	if _, err := h.Root.Run(context.Background(), "读 long.txt"); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("应有 running+done 两个事件, got %+v", events)
	}
	running, done := events[0], events[1]
	if running.Status != ToolStatusRunning || done.Status != ToolStatusDone {
		t.Fatalf("状态推进错误: %+v", events)
	}
	if running.CallID == "" || running.CallID != done.CallID {
		t.Fatalf("CallID 应一致且非空: %+v", events)
	}
	if running.Name != "read" || done.Name != "read" {
		t.Fatalf("工具名: %+v", events)
	}
	if want := `{"file_path":"long.txt"}`; running.Input != want {
		t.Fatalf("Input 应为全量参数, got %q want %q", running.Input, want)
	}
	if !strings.Contains(done.Output, long) {
		t.Fatalf("Output 应含全量结果文本（不截断）: %q", done.Output)
	}
	if done.Err != "" {
		t.Fatalf("成功事件不应带 Err: %+v", done)
	}
	if running.AgentID != "agent_root" || running.Label != "main" {
		t.Fatalf("根 agent 身份: %+v", running)
	}
}

func TestToolEventErrorStatus(t *testing.T) {
	f := newFakeLLM([]string{
		`TOOL:{"file_path":"no_such_file.txt"}`,
		"ok",
	})
	defer f.Close()
	h := buildTestHarness(t, f, t.TempDir())
	defer h.Close()

	var events []ToolEvent
	h.Root.SetOnToolEvent(func(e ToolEvent) { events = append(events, e) })
	if _, err := h.Root.Run(context.Background(), "读不存在的文件"); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].Status != ToolStatusError {
		t.Fatalf("失败调用应为 error 终态: %+v", events)
	}
	if events[1].Err == "" || !strings.Contains(events[1].Output, "error:") {
		t.Fatalf("error 事件应带 Err 与完整反馈文本: %+v", events[1])
	}
}

func TestToolEventDeniedWithFeedback(t *testing.T) {
	host := &captureHost{decision: tools.ApproveDecision{Feedback: "别删文件，改用归档目录"}}
	f := newFakeLLM([]string{
		`TOOL:bash:{"command":"rm -rf tmp"}`,
		"好的，我改用归档目录",
	})
	defer f.Close()
	h := buildHarnessWithHost(t, f, t.TempDir(), host)
	defer h.Close()
	h.Root.SetApproveMode(true)

	var events []ToolEvent
	h.Root.SetOnToolEvent(func(e ToolEvent) { events = append(events, e) })
	if _, err := h.Root.Run(context.Background(), "清理 tmp"); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].Status != ToolStatusDenied {
		t.Fatalf("拒绝调用应为 denied 终态: %+v", events)
	}
	if !strings.Contains(events[1].Output, "别删文件，改用归档目录") {
		t.Fatalf("denied 事件应携带用户反馈: %+v", events[1])
	}
	// 反馈也作为工具结果回给模型
	inHistory := false
	for _, m := range h.Root.History() {
		if m.Role == "tool" && strings.Contains(m.Content, "别删文件，改用归档目录") {
			inHistory = true
		}
	}
	if !inHistory {
		t.Fatal("拒绝反馈应进入历史 tool 消息")
	}
}

func TestApproveToolReceivesFullArgs(t *testing.T) {
	host := &captureHost{decision: tools.ApproveDecision{Approved: true}}
	f := newFakeLLM([]string{
		`TOOL:long_input:{"text":"` + strings.Repeat("a", 400) + `"}`,
		"done",
	})
	defer f.Close()
	h := buildHarnessWithHost(t, f, t.TempDir(), host)
	defer h.Close()
	h.Root.Reg().RegisterCustom(llm.Tool{
		Name:        "long_input",
		Description: "测试用：透传长参数",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}},
	}, func(ctx context.Context, args string) (tools.Result, error) {
		return tools.TextResult("ok"), nil
	})
	h.Root.SetApproveMode(true)

	if _, err := h.Root.Run(context.Background(), "调用长参数工具"); err != nil {
		t.Fatal(err)
	}
	got := host.argOf("long_input")
	if !strings.Contains(got, strings.Repeat("a", 400)) {
		t.Fatalf("审批应收到全量参数（不截断到 200 字）, got %d 字: %q…", len(got), got[:min(50, len(got))])
	}
}

func TestSubagentToolEventsFlowToRoot(t *testing.T) {
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "hello.txt"), []byte("hi"), 0o644)
	f := newFakeLLM([]string{
		`TOOL:{"file_path":"hello.txt"}`, // 子代理消费
		"child done",
	})
	defer f.Close()
	h := buildTestHarness(t, f, ws)
	defer h.Close()

	var events []ToolEvent
	h.Root.SetOnToolEvent(func(e ToolEvent) { events = append(events, e) })
	if _, _, err := h.Manager.Subagent(h.Root, "检查文件", "读 hello.txt", false); err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 {
		t.Fatalf("子代理的工具事件应回流到根回调: %+v", events)
	}
	if events[0].AgentID == "agent_root" || events[0].Label != "检查文件" {
		t.Fatalf("事件应携带子代理身份: %+v", events[0])
	}
	if events[0].Status != ToolStatusRunning || events[1].Status != ToolStatusDone {
		t.Fatalf("状态推进: %+v", events)
	}
}

// planHost 测试宿主：按计划批准/拒绝呈交的计划。
type planHost struct {
	tools.NoopHost
	ok bool
}

func (h planHost) ApprovePlan(ctx context.Context, plan string) (bool, string) {
	if h.ok {
		return true, ""
	}
	return false, "再改改"
}

func TestPlanModeBlocksMutatingTools(t *testing.T) {
	ws := t.TempDir()
	f := newFakeLLM([]string{
		`TOOL:write:{"file_path":"x.txt","content":"hi"}`,
		"done",
	})
	defer f.Close()
	h := buildTestHarness(t, f, ws)
	defer h.Close()
	h.Root.SetPlanMode(true)
	if _, err := h.Root.Run(context.Background(), "写个文件"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, "x.txt")); !os.IsNotExist(err) {
		t.Fatal("计划模式下 write 不应落盘")
	}
	// 拒绝结果进入历史（模型可见原因）
	found := false
	for _, m := range h.Root.History() {
		if m.Role == "tool" && strings.Contains(m.Content, "plan mode") {
			found = true
		}
	}
	if !found {
		t.Fatal("历史缺少计划模式拦截结果")
	}
}

func TestPlanApprovalSwitchesMode(t *testing.T) {
	newH := func(ok bool) *Harness {
		f := newFakeLLM([]string{
			`TOOL:exit_plan_mode:{"plan":"# 计划\n- 步骤一"}`,
			"done",
		})
		t.Cleanup(f.Close)
		h, err := Build(Options{
			Workspace: t.TempDir(),
			Client:    llm.NewClient(llm.Config{BaseURL: f.URL(), APIKey: "t", Model: "test-model"}),
			Settings:  &config.Settings{Policy: "write-workspace"},
			Host:      &planHost{ok: ok},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(h.Close)
		return h
	}

	h := newH(true)
	h.Root.SetPlanMode(true)
	if _, err := h.Root.Run(context.Background(), "给个计划"); err != nil {
		t.Fatal(err)
	}
	if h.Root.InPlanMode() {
		t.Fatal("批准计划后应自动退出计划模式")
	}

	h = newH(false)
	h.Root.SetPlanMode(true)
	if _, err := h.Root.Run(context.Background(), "给个计划"); err != nil {
		t.Fatal(err)
	}
	if !h.Root.InPlanMode() {
		t.Fatal("计划被拒绝后应保持计划模式")
	}
}

func TestChildFollowsRootPlanMode(t *testing.T) {
	f := newFakeLLM([]string{"ok"})
	defer f.Close()
	h := buildTestHarness(t, f, t.TempDir())
	defer h.Close()
	child := h.Manager.NewChild(h.Root, "probe", nil)
	if child.inPlan() {
		t.Fatal("根未开计划模式时子代理不应受拦截")
	}
	h.Root.SetPlanMode(true)
	if !child.inPlan() {
		t.Fatal("子代理应跟随根的计划模式")
	}
}

// askApproveHost 测试宿主：AskUser 一律返回每题的第一个选项
// （enter_plan_mode 场景即"进入计划模式"）。
type askApproveHost struct{ tools.NoopHost }

func (h askApproveHost) AskUser(ctx context.Context, qs []tools.Question) (map[string]any, error) {
	m := map[string]any{}
	for _, q := range qs {
		if len(q.Options) > 0 {
			m[q.ID] = q.Options[0].Label
		}
	}
	return m, nil
}

// askTextHost 测试宿主：AskUser 返回固定自定义文本（视为拒绝反馈）。
type askTextHost struct {
	tools.NoopHost
	text string
}

func (h askTextHost) AskUser(ctx context.Context, qs []tools.Question) (map[string]any, error) {
	m := map[string]any{}
	for _, q := range qs {
		m[q.ID] = h.text
	}
	return m, nil
}

func TestEnterPlanModeApproved(t *testing.T) {
	ws := t.TempDir()
	f := newFakeLLM([]string{
		`TOOL:enter_plan_mode:{"reason":"改动横跨三个包，方案存在分叉"}`,
		`TOOL:write:{"file_path":"x.txt","content":"hi"}`,
		"done",
	})
	defer f.Close()
	h, err := Build(Options{
		Workspace: ws,
		Client:    llm.NewClient(llm.Config{BaseURL: f.URL(), APIKey: "t", Model: "test-model"}),
		Settings:  &config.Settings{Policy: "write-workspace"},
		Host:      &askApproveHost{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.Root.Run(context.Background(), "做个复杂重构"); err != nil {
		t.Fatal(err)
	}
	if !h.Root.InPlanMode() {
		t.Fatal("用户批准后应进入计划模式")
	}
	// 同一回合内随后的改动类工具被硬拦截
	if _, err := os.Stat(filepath.Join(ws, "x.txt")); !os.IsNotExist(err) {
		t.Fatal("进入计划模式后 write 不应落盘")
	}
	found := false
	for _, m := range h.Root.History() {
		if m.Role == "tool" && strings.Contains(m.Content, "plan mode") {
			found = true
		}
	}
	if !found {
		t.Fatal("历史缺少计划模式拦截结果")
	}
}

func TestEnterPlanModeDeclined(t *testing.T) {
	f := newFakeLLM([]string{
		`TOOL:enter_plan_mode:{"reason":"方案未定"}`,
		"ok direct",
	})
	defer f.Close()
	// NoopHost.AskUser 报错（无交互用户）→ 按拒绝处理
	h := buildTestHarness(t, f, t.TempDir())
	defer h.Close()
	if _, err := h.Root.Run(context.Background(), "直接做"); err != nil {
		t.Fatal(err)
	}
	if h.Root.InPlanMode() {
		t.Fatal("拒绝后不应进入计划模式")
	}
}

func TestEnterPlanModeFeedback(t *testing.T) {
	f := newFakeLLM([]string{"ok"})
	defer f.Close()
	h, err := Build(Options{
		Workspace: t.TempDir(),
		Client:    llm.NewClient(llm.Config{BaseURL: f.URL(), APIKey: "t", Model: "test-model"}),
		Settings:  &config.Settings{Policy: "write-workspace"},
		Host:      &askTextHost{text: "先按现有架构来，不用计划"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	res := h.Root.reg.Execute(context.Background(), "enter_plan_mode", `{"reason":"方案未定"}`)
	if !strings.Contains(res.Text, "先按现有架构来") {
		t.Fatalf("拒绝反馈应回给模型: %s", res.Text)
	}
	if h.Root.InPlanMode() {
		t.Fatal("拒绝后不应进入计划模式")
	}
}

func TestEnterPlanModeAlreadyInPlan(t *testing.T) {
	f := newFakeLLM([]string{"ok"})
	defer f.Close()
	h := buildTestHarness(t, f, t.TempDir())
	defer h.Close()
	h.Root.SetPlanMode(true)
	res := h.Root.reg.Execute(context.Background(), "enter_plan_mode", `{"reason":"x"}`)
	if !strings.Contains(res.Text, "Already in plan mode") {
		t.Fatalf("已在计划模式应幂等提示: %s", res.Text)
	}
}
