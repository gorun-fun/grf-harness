// compact_test.go 会话压缩回归：手动压缩替换历史并保留 system 头、
// 暂停态拒绝、自动阈值触发/禁用、子代理委派范围声明。
package agent

import (
	"context"
	"strings"
	"testing"

	"git.gorun.fun/share/grf-harness/llm"
)

func TestCompactReplacesHistory(t *testing.T) {
	f := newMultiLLM([]string{"answer one", "SUMMARY: did thing one", "answer two"})
	defer f.Close()
	h := buildHarnessURL(t, f.URL(), t.TempDir())
	defer h.Close()
	root := h.Root

	if _, err := root.Run(context.Background(), "do thing one"); err != nil {
		t.Fatal(err)
	}
	sysBefore := root.History()[0].Content

	summary, err := root.Compact(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary != "did thing one" {
		t.Fatalf("summary must be the normalized model reply, got %q", summary)
	}
	// 压缩请求：system 为总结提示词，user 为 transcript（含既有对话原文）
	req := f.request(1)
	if len(req) < 2 || req[0].Role != "system" || !strings.Contains(req[0].Content, "compact conversation transcripts") {
		t.Fatalf("compact request must carry the summarizer prompt: %+v", req)
	}
	if !strings.Contains(req[1].Content, "do thing one") {
		t.Fatalf("transcript must carry prior conversation: %q", req[1].Content)
	}
	// 历史收敛为 [system 原文, 总结消息]
	hist := root.History()
	if len(hist) != 2 || hist[0].Role != "system" || hist[0].Content != sysBefore {
		t.Fatalf("history must collapse to [system verbatim, summary], got %d msgs", len(hist))
	}
	if hist[1].Role != "user" || !strings.HasPrefix(hist[1].Content, "[conversation summary]") {
		t.Fatalf("unexpected summary message: %+v", hist[1])
	}

	// 压缩后会话可继续：请求 = [system, 总结, 新输入]
	if _, err := root.Run(context.Background(), "next"); err != nil {
		t.Fatal(err)
	}
	req2 := f.request(2)
	var sawSummary, sawNext bool
	for _, m := range req2 {
		if strings.HasPrefix(m.Content, "[conversation summary]") {
			sawSummary = true
		}
		if m.Role == "user" && m.Content == "next" {
			sawNext = true
		}
	}
	if !sawSummary || !sawNext {
		t.Fatalf("post-compact request must carry summary + new input (summary=%v next=%v)", sawSummary, sawNext)
	}
}

func TestCompactRefusesPausedCalls(t *testing.T) {
	f := newMultiLLM([]string{"done"})
	defer f.Close()
	h := buildHarnessURL(t, f.URL(), t.TempDir())
	defer h.Close()
	root := h.Root

	root.RestoreFull([]llm.Message{
		{Role: "user", Content: "q"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function",
			Function: llm.FunctionCall{Name: "bash", Arguments: "{}"}}}},
	}, "")
	if !root.HasPausedCalls() {
		t.Fatal("expected paused tail")
	}
	if _, err := root.Compact(context.Background()); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("compact must refuse paused history, got err=%v", err)
	}
	if n := len(root.History()); n != 3 { // system + user + assistant(tool_calls)
		t.Fatalf("history must be untouched on refusal, got %d msgs", n)
	}
}

func TestAutoCompactFiresAtThreshold(t *testing.T) {
	f := newMultiLLM([]string{"answer one", "AUTO: summary of thing one", "answer two"})
	defer f.Close()
	h := buildHarnessURL(t, f.URL(), t.TempDir())
	defer h.Close()
	root := h.Root
	root.SetCompactThreshold(10) // 极低阈值：第二轮必触发

	if _, err := root.Run(context.Background(), "do thing one"); err != nil {
		t.Fatal(err)
	}
	if got := len(f.request(0)); got > 0 && f.callsCount() != 1 {
		t.Fatalf("first turn must not compact (empty transcript)")
	}
	if _, err := root.Run(context.Background(), "do thing two"); err != nil {
		t.Fatal(err)
	}
	// 请求序：0=首轮，1=自动压缩（总结提示词），2=第二轮（system+总结+新输入）
	req := f.request(1)
	if len(req) < 2 || !strings.Contains(req[0].Content, "compact conversation transcripts") {
		t.Fatalf("second turn should have auto-compacted first, req1=%+v", req)
	}
	req2 := f.request(2)
	var sawSummary bool
	for _, m := range req2 {
		if strings.HasPrefix(m.Content, "[conversation summary] AUTO") {
			sawSummary = true
		}
	}
	if !sawSummary {
		t.Fatalf("post-auto-compact request must carry the summary: %s", roleShape(req2))
	}
	if n := len(root.History()); n != 4 { // system, summary, user two, assistant answer
		t.Fatalf("unexpected history size after auto-compact: %d", n)
	}
}

func TestAutoCompactDisabledByNegativeThreshold(t *testing.T) {
	f := newMultiLLM([]string{"answer one", "answer two"})
	defer f.Close()
	h := buildHarnessURL(t, f.URL(), t.TempDir())
	defer h.Close()
	root := h.Root
	root.SetCompactThreshold(-1)

	if _, err := root.Run(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Run(context.Background(), "two"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		for _, m := range f.request(i) {
			if strings.Contains(m.Content, "compact conversation transcripts") {
				t.Fatalf("auto-compact must stay disabled with negative threshold")
			}
		}
	}
	if n := len(root.History()); n != 5 { // system + 2×(user, assistant)
		t.Fatalf("history must be untouched, got %d", n)
	}
}

func TestSubagentDelegationScopeDeclaration(t *testing.T) {
	p := BuildSystemPrompt(PromptOptions{Workspace: "/w", Model: "m", Subagent: true, Label: "audit"},
		"custom body")
	if !strings.Contains(p, "delegated subagent") || !strings.Contains(p, "cannot be widened") {
		t.Fatal("subagent prompt must carry the delegation-scope declaration")
	}
	if !strings.Contains(p, "state the limitation in your reply") {
		t.Fatal("declaration must tell the child to report denials instead of retrying")
	}
}
