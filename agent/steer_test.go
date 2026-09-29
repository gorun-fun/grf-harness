// steer_test.go 运行中插话（steer）回归：步骤边界注入（tool 结果闭合
// 之后）、空闲排队待下回合、DrainSteer 取空语义。
package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"git.gorun.fun/share/grf-harness/llm"
	"git.gorun.fun/share/grf-harness/tools"
)

// request 取第 i 次模型请求的消息（multiLLM 记录并发保护）。
func (m *multiLLM) request(i int) []llm.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i >= len(m.gotMsgs) {
		return nil
	}
	return m.gotMsgs[i]
}

func TestSteerInjectsAtStepBoundary(t *testing.T) {
	f := newMultiLLM([]string{
		`MULTI:[["read","{\"file_path\":\"a\"}"]]`,
		"done",
	})
	defer f.Close()
	h := buildHarnessURL(t, f.URL(), t.TempDir())
	defer h.Close()

	h.Root.reg.Register("read", func(ctx context.Context, args string) (tools.Result, error) {
		time.Sleep(400 * time.Millisecond) // 给插话留出在工具执行期间到达的窗口
		return tools.TextResult("ok"), nil
	})

	go func() {
		time.Sleep(150 * time.Millisecond)
		h.Root.Steer("pivot: use approach B instead")
	}()

	if _, err := h.Root.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	msgs := f.request(1)
	if msgs == nil {
		t.Fatal("expected a second model request")
	}
	// 插话必须以普通 user 消息出现，且位于闭合 tool 结果之后（OpenAI
	// 协议：user 不得插进未闭合的 tool_call 序列）
	var steerIdx, toolIdx = -1, -1
	for i, m := range msgs {
		if m.Role == "user" && m.Content == "pivot: use approach B instead" {
			steerIdx = i
		}
		if m.Role == "tool" {
			toolIdx = i
		}
	}
	if steerIdx < 0 {
		t.Fatalf("steering input not injected into the second request: %v", roleShape(msgs))
	}
	if toolIdx < 0 || steerIdx < toolIdx {
		t.Fatalf("steering user message must follow the closed tool result (tool=%d steer=%d)", toolIdx, steerIdx)
	}
}

func TestSteerIdleQueuedUntilNextTurn(t *testing.T) {
	f := newMultiLLM([]string{"done"})
	defer f.Close()
	h := buildHarnessURL(t, f.URL(), t.TempDir())
	defer h.Close()

	h.Root.Steer("late thought") // 空闲期排队：不触发任何运行
	time.Sleep(50 * time.Millisecond)
	if h.Root.IsRunning() {
		t.Fatal("Steer must not start a run by itself")
	}
	if _, err := h.Root.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	msgs := f.request(0)
	var sawHi, sawSteer bool
	for _, m := range msgs {
		if m.Role == "user" && m.Content == "hi" {
			sawHi = true
		}
		if m.Role == "user" && m.Content == "late thought" {
			sawSteer = true
		}
	}
	if !sawHi || !sawSteer {
		t.Fatalf("idle-queued steer must enter the next turn's first request (hi=%v steer=%v): %v", sawHi, sawSteer, roleShape(msgs))
	}
}

func TestDrainSteerTakesAll(t *testing.T) {
	f := newMultiLLM([]string{"done"})
	defer f.Close()
	h := buildHarnessURL(t, f.URL(), t.TempDir())
	defer h.Close()

	h.Root.Steer("one")
	h.Root.Steer("two")
	h.Root.Steer("") // 空白忽略
	got := h.Root.DrainSteer()
	if len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("unexpected drain: %v", got)
	}
	if again := h.Root.DrainSteer(); again != nil {
		t.Fatalf("drain must empty the queue, got %v", again)
	}
}

func roleShape(msgs []llm.Message) string {
	var b strings.Builder
	for i, m := range msgs {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(m.Role)
		if m.Role == "tool" {
			b.WriteString("(" + m.ToolCallID + ")")
		}
	}
	return b.String()
}
