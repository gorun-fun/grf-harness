// delegate_test.go 委派优先横切件测试：内联阈值 / 扇出形状 / 上下文
// 压力三档提醒的触发与闩锁、仅根生效门、orchestrator 模式的工具剔除
// 与提示词段。
package agent

import (
	"context"
	"strings"
	"testing"

	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/llm"
	"git.gorun.fun/share/grf-harness/tools"
)

// TestDelegateInlineThreshold 端到端：内联调研累计 8 次触发一次提醒，
// 20 档不再重复（9 次调用只到 8 档）。
func TestDelegateInlineThreshold(t *testing.T) {
	script := make([]string, 0, 10)
	for i := 0; i < 9; i++ {
		script = append(script, `TOOL:read:{"file_path":"no-such-file"}`)
	}
	script = append(script, "done")
	f := newFakeLLM(script)
	defer f.Close()
	h := buildTestHarness(t, f, t.TempDir())
	defer h.Close()

	if _, err := h.Root.Run(context.Background(), "调研一下"); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range h.Root.History() {
		if m.Role == "user" && strings.Contains(m.Content, "inline read/search tool calls") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 inline-threshold notice, got %d", n)
	}
	if h.Root.inlineCalls != 9 || h.Root.delegRemindStage != 1 {
		t.Fatalf("inlineCalls=%d stage=%d, want 9/1", h.Root.inlineCalls, h.Root.delegRemindStage)
	}
}

// TestDelegateFanout 单元：单批 ≥3 个内联调研调用触发一次提醒（闩锁），
// 子代理不提醒。
func TestDelegateFanout(t *testing.T) {
	f := newFakeLLM([]string{"ok"})
	defer f.Close()
	h := buildTestHarness(t, f, t.TempDir())
	defer h.Close()

	reads := []llm.ToolCall{
		{Function: llm.FunctionCall{Name: "read", Arguments: `{"file_path":"a"}`}},
		{Function: llm.FunctionCall{Name: "read", Arguments: `{"file_path":"b"}`}},
		{Function: llm.FunctionCall{Name: "grep", Arguments: `{"pattern":"x"}`}},
	}
	h.Root.noteFanout(reads)
	notices := h.Root.drainNotices()
	if len(notices) != 1 || !strings.Contains(notices[0], "fanning out 3 parallel read/search") {
		t.Fatalf("fanout notice: %v", notices)
	}
	// 闩锁：同批再次扇出不再提醒
	h.Root.noteFanout(reads)
	if got := h.Root.drainNotices(); len(got) != 0 {
		t.Fatalf("latched fanout should not re-notify: %v", got)
	}
	// 子代理是采集主体，不提醒
	child := h.Manager.NewChild(h.Root, "sub", nil)
	child.noteFanout(reads)
	if got := child.drainNotices(); len(got) != 0 {
		t.Fatalf("child should not be nudged: %v", got)
	}
}

// TestDelegatePressure 单元：估算 token 进入阈值 60% 中间带时一次性提醒；
// 更低不提醒；已过阈值走压缩不重复提醒。
func TestDelegatePressure(t *testing.T) {
	f := newFakeLLM([]string{"ok"})
	defer f.Close()
	h := buildTestHarness(t, f, t.TempDir())
	defer h.Close()
	a := h.Root

	// 垫历史到已知估算值 est，阈值设为 est/0.7（比值 0.7 ∈ [0.6,1)）
	a.mu.Lock()
	a.history = append(a.history, llm.Message{Role: "user", Content: strings.Repeat("字", 4000)})
	est := llm.EstimateMessagesTokens(a.history)
	a.mu.Unlock()
	a.SetCompactThreshold(int(est * 10 / 7))

	a.maybeAutoCompact(context.Background())
	notices := a.drainNotices()
	if len(notices) != 1 || !strings.Contains(notices[0], "context is filling") {
		t.Fatalf("pressure notice: %v", notices)
	}
	// 闩锁
	a.maybeAutoCompact(context.Background())
	if got := a.drainNotices(); len(got) != 0 {
		t.Fatalf("latched pressure should not re-notify: %v", got)
	}
}

// TestDelegatePressureBelowBand 估算远低于阈值时不提醒。
func TestDelegatePressureBelowBand(t *testing.T) {
	f := newFakeLLM([]string{"ok"})
	defer f.Close()
	h := buildTestHarness(t, f, t.TempDir())
	defer h.Close()
	a := h.Root
	a.SetCompactThreshold(1_000_000)
	a.maybeAutoCompact(context.Background())
	if got := a.drainNotices(); len(got) != 0 {
		t.Fatalf("no pressure notice expected: %v", got)
	}
}

// TestOrchestratorMode orchestrator 模式：根剔除内联调研工具但保留
// subagent，子代理保持全量；系统提示词拼编排者段。subagent 被禁用时
// 模式跳过（无委派通道的剔除只剩死路）。
func TestOrchestratorMode(t *testing.T) {
	f := newFakeLLM([]string{"ok"})
	defer f.Close()
	ws := t.TempDir()
	h, err := Build(Options{
		Workspace: ws,
		Client:    llm.NewClient(llm.Config{BaseURL: f.URL(), APIKey: "t", Model: "test-model"}),
		Settings:  &config.Settings{Policy: "write-workspace", Orchestrator: true},
		Host:      &tools.NoopHost{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	for _, name := range []string{"read", "grep", "glob", "web_search"} {
		if h.Root.reg.Has(name) {
			t.Fatalf("orchestrator root should not register %s", name)
		}
	}
	if !h.Root.reg.Has("subagent") {
		t.Fatal("orchestrator root must keep subagent")
	}
	if !strings.Contains(h.Root.SystemText(), "编排者模式") {
		t.Fatal("root system prompt should contain orchestrator section")
	}
	child := h.Manager.NewChild(h.Root, "sub", nil)
	if !child.reg.Has("read") || !child.reg.Has("grep") {
		t.Fatal("child keeps inline research tools")
	}
	if strings.Contains(child.SystemText(), "编排者模式") {
		t.Fatal("child prompt must not contain orchestrator section")
	}
}

// TestOrchestratorSkipWithoutSubagent subagent 被 DisabledTools 剔除时
// orchestrator 不生效（保留完整工具集）。
func TestOrchestratorSkipWithoutSubagent(t *testing.T) {
	f := newFakeLLM([]string{"ok"})
	defer f.Close()
	h, err := Build(Options{
		Workspace:     t.TempDir(),
		Client:        llm.NewClient(llm.Config{BaseURL: f.URL(), APIKey: "t", Model: "test-model"}),
		Settings:      &config.Settings{Policy: "write-workspace", Orchestrator: true},
		Host:          &tools.NoopHost{},
		DisabledTools: []string{"subagent", "subagent_fork"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if !h.Root.reg.Has("read") {
		t.Fatal("orchestrator must be skipped when subagent is disabled")
	}
	if strings.Contains(h.Root.SystemText(), "编排者模式") {
		t.Fatal("no orchestrator section when skipped")
	}
}
