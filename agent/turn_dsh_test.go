// turn_dsh_test.go dsh 拟合机制的回归测试：工具结果裁剪网、
// aborted-before-dispatch 合成结果、outcome-unknown 占位、同批并行分发、
// 连续重复调用递进提醒、取消流保留已交付前缀。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/llm"
	"git.gorun.fun/share/grf-harness/tools"
)

func TestPruneResultText(t *testing.T) {
	short := strings.Repeat("a", 100)
	if got := pruneResultText(short); got != short {
		t.Fatalf("short result must pass through unchanged")
	}
	long := strings.Repeat("h", 6000) + strings.Repeat("m", 6000) + strings.Repeat("t", 3000)
	got := pruneResultText(long)
	if !strings.HasPrefix(got, strings.Repeat("h", pruneHead)) {
		t.Fatalf("head not preserved")
	}
	if !strings.HasSuffix(got, strings.Repeat("t", pruneTail)) {
		t.Fatalf("tail not preserved")
	}
	if !strings.Contains(got, "middle omitted") {
		t.Fatalf("omission marker missing")
	}
	if want := len(long) - pruneHead - pruneTail; !strings.Contains(got, fmt.Sprint(want)) {
		t.Fatalf("omitted char count %d not reported", want)
	}
}

func TestPruneResultTextUTF8Boundary(t *testing.T) {
	// 中文字符（3 字节）铺满切点附近，切点不得劈开多字节字符
	long := strings.Repeat("好", 3000) + strings.Repeat("了", 3000)
	got := pruneResultText(long)
	for _, r := range got { // rangestring：非法 UTF-8 会以 RuneError 出现
		if r == 0xFFFD {
			t.Fatalf("prune split a multi-byte rune")
		}
	}
}

func TestAbortedBeforeDispatchSynthesized(t *testing.T) {
	f := newFakeLLM([]string{"ok"})
	defer f.Close()
	h := buildHarnessURL(t, f.URL(), t.TempDir())
	defer h.Close()

	var ran atomic.Bool
	h.Root.reg.Register("read", func(ctx context.Context, args string) (tools.Result, error) {
		ran.Store(true)
		return tools.TextResult("ran"), nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 回合已取消：调用必须被合成结果闭合而非真的执行
	call := llm.ToolCall{ID: "cx", Type: "function", Function: llm.FunctionCall{Name: "read", Arguments: `{"file_path":"x"}`}}
	h.Root.execToolCall(ctx, call, "cx")

	if ran.Load() {
		t.Fatalf("handler must not run when the turn is already cancelled")
	}
	hist := h.Root.History()
	last := hist[len(hist)-1]
	if last.Role != "tool" || last.ToolCallID != "cx" {
		t.Fatalf("expected closing tool message for cx, got %+v", last)
	}
	if !strings.Contains(last.Content, "aborted before dispatch") || !strings.Contains(last.Content, "never ran") {
		t.Fatalf("unexpected synthetic result: %q", last.Content)
	}
}

func TestOutcomeUnknownPlaceholder(t *testing.T) {
	// 崩溃/中断残留的未闭合调用：请求快照以 outcome-unknown 引导占位
	h := []llm.Message{
		{Role: "system", Content: "s"},
		{Role: "user", Content: "q"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c9", Type: "function", Function: llm.FunctionCall{Name: "bash", Arguments: "{}"}}}},
		{Role: "user", Content: "next"},
	}
	got := canonicalizeToolResults(h)
	found := false
	for _, m := range got {
		if m.Role == "tool" && m.ToolCallID == "c9" {
			found = true
			if !strings.Contains(m.Content, "outcome unknown") || !strings.Contains(m.Content, "idempotent") {
				t.Fatalf("placeholder must carry outcome-unknown retry guidance, got %q", m.Content)
			}
		}
	}
	if !found {
		t.Fatalf("unclosed call c9 not closed in snapshot")
	}
}

// multiLLM 按脚本返回多工具调用批次的伪 LLM（fakeLLM 一次只回一个调用）。
type multiLLM struct {
	mu      sync.Mutex
	calls   int
	script  []string // "MULTI:[name,args] 列表 JSON" 或纯文本
	srv     *httptest.Server
	gotMsgs [][]llm.Message
}

func newMultiLLM(script []string) *multiLLM {
	m := &multiLLM{script: script}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []llm.Message `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		m.mu.Lock()
		m.calls++
		m.gotMsgs = append(m.gotMsgs, req.Messages)
		idx := m.calls - 1
		if idx >= len(m.script) {
			idx = len(m.script) - 1
		}
		resp := m.script[idx]
		m.mu.Unlock()

		var msg llm.Message
		if strings.HasPrefix(resp, "MULTI:") {
			var pairs [][2]string
			if err := json.Unmarshal([]byte(strings.TrimPrefix(resp, "MULTI:")), &pairs); err != nil {
				panic(err)
			}
			for i, p := range pairs {
				msg.ToolCalls = append(msg.ToolCalls, llm.ToolCall{
					ID: fmt.Sprintf("c%d", i+1), Type: "function",
					Function: llm.FunctionCall{Name: p[0], Arguments: p[1]},
				})
			}
			msg.Role = "assistant"
		} else {
			msg = llm.Message{Role: "assistant", Content: resp}
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": msg}}})
	}))
	return m
}

func (m *multiLLM) URL() string { return m.srv.URL }
func (m *multiLLM) Close()      { m.srv.Close() }

// buildHarnessURL 按端点 URL 构建测试 harness（buildTestHarness 只收
// *fakeLLM，multiLLM/SSE 服务器等自建端点走这里）。
func buildHarnessURL(t *testing.T, url, ws string) *Harness {
	t.Helper()
	h, err := Build(Options{
		Workspace: ws,
		Client:    llm.NewClient(llm.Config{BaseURL: url, APIKey: "t", Model: "test-model"}),
		Settings:  &config.Settings{Policy: "write-workspace"},
		Host:      &tools.NoopHost{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestDispatchBatchParallelAndOrder(t *testing.T) {
	f := newMultiLLM([]string{
		`MULTI:[["read","{\"file_path\":\"a\"}"],["read","{\"file_path\":\"b\"}"],["read","{\"file_path\":\"c\"}"]]`,
		"done",
	})
	defer f.Close()
	h := buildHarnessURL(t, f.URL(), t.TempDir())
	defer h.Close()

	h.Root.reg.Register("read", func(ctx context.Context, args string) (tools.Result, error) {
		time.Sleep(200 * time.Millisecond)
		return tools.TextResult(args), nil
	})

	t0 := time.Now()
	if _, err := h.Root.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(t0)
	// 串行 ≥600ms；并行（默认上限 8）≈200ms+两轮请求开销
	if elapsed > 500*time.Millisecond {
		t.Fatalf("parallel-safe batch ran serially: %v", elapsed)
	}

	// 请求快照按发起 assistant 的 tool_calls 顺序闭合（canonicalize 归位）
	snap := canonicalizeToolResults(sanitizeSnapshot(h.Root.History()))
	var order []string
	for i, m := range snap {
		if m.Role == "assistant" && len(m.ToolCalls) == 3 {
			for j := 1; j <= 3 && i+j < len(snap); j++ {
				tm := snap[i+j]
				if tm.Role != "tool" {
					break
				}
				order = append(order, tm.ToolCallID)
			}
			break
		}
	}
	if strings.Join(order, ",") != "c1,c2,c3" {
		t.Fatalf("tool results not closed in model order: %v", order)
	}
}

func TestRepeatReminderThreshold(t *testing.T) {
	f := newMultiLLM([]string{
		`MULTI:[["read","{\"file_path\":\"same\"}"]]`,
		`MULTI:[["read","{\"file_path\":\"same\"}"]]`,
		`MULTI:[["read","{\"file_path\":\"same\"}"]]`,
		"done",
	})
	defer f.Close()
	h := buildHarnessURL(t, f.URL(), t.TempDir())
	defer h.Close()

	h.Root.reg.Register("read", func(ctx context.Context, args string) (tools.Result, error) {
		return tools.TextResult("ok"), nil
	})
	if _, err := h.Root.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	var reminders int
	for _, m := range h.Root.History() {
		if m.Role == "user" && strings.Contains(m.Content, "[system notice]") && strings.Contains(m.Content, "3 times in a row") {
			reminders++
		}
	}
	if reminders != 1 {
		t.Fatalf("expected exactly one threshold-3 reminder, got %d", reminders)
	}
}

func TestCancelledStreamKeepsDeliveredPrefix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", mustJSONString(t, map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": "Partial answer"}}},
		}))
		w.(http.Flusher).Flush()
		<-r.Context().Done() // 挂住流，直到客户端取消
	}))
	defer srv.Close()

	h, err := Build(Options{
		Workspace: t.TempDir(),
		Client:    llm.NewClient(llm.Config{BaseURL: srv.URL, APIKey: "t", Model: "test-model"}),
		Settings:  &config.Settings{Policy: "write-workspace"},
		Host:      &tools.NoopHost{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	ctx, cancel := context.WithCancel(context.Background())
	h.Root.SetStreamHooks(func(string) { cancel() }, nil)
	out, err := h.Root.Run(ctx, "go")
	if err == nil || out != "" {
		t.Fatalf("cancelled run must return an error, got %q err=%v", out, err)
	}
	var found *llm.Message
	hist := h.Root.History()
	for i := range hist {
		if hist[i].Role == "assistant" && strings.Contains(hist[i].Content, "Partial answer") {
			found = &hist[i]
		}
	}
	if found == nil {
		t.Fatalf("delivered prefix not preserved in history after cancellation")
	}
	if !strings.Contains(found.Content, "[interrupted:") {
		t.Fatalf("interrupted marker missing: %q", found.Content)
	}
}

func mustJSONString(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
