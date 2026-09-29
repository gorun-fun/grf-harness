// steer_test.go 排队插话（steer）与 Ctrl+C 中断的交互语义：中断必须是
// 完全停下——丢弃未注入的排队消息、不触发续跑；正常结束的回合仍把
// 排队消息转入下一轮（回归保护，中断语义收紧不得波及正常收尾）。
package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"git.gorun.fun/share/grf-harness/app"
	"git.gorun.fun/share/grf-harness/llm"
	"git.gorun.fun/share/grf-harness/term"
)

// newSteerChat 构造不启动事件循环的 Chat（测试 goroutine 即事件循环）。
func newSteerChat(t *testing.T, a *app.App) *Chat {
	t.Helper()
	c := NewChat(a)
	c.ctx = context.Background()
	c.scr = term.NewScreen()
	c.width, c.height = 80, 24
	return c
}

// blocksText 全部内容块拼接（提示文案断言用）。
func blocksText(c *Chat) string {
	var sb strings.Builder
	for _, b := range c.blocks {
		sb.WriteString(b.text)
		sb.WriteString("\n")
	}
	return sb.String()
}

// TestInterruptDropsQueuedSteer 运行中 Ctrl+C：取消当前回合、丢弃排队
// 插话并提示、不退出。
func TestInterruptDropsQueuedSteer(t *testing.T) {
	a, err := app.New(app.Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	c := newSteerChat(t, a)
	c.running = true
	runCtx, cancel := context.WithCancel(c.ctx)
	c.cancel = cancel
	a.Root().Steer("排队一")
	a.Root().Steer("排队二")

	if quit := c.interruptKey(); quit {
		t.Fatal("运行中 Ctrl+C 不应退出")
	}
	if runCtx.Err() == nil {
		t.Fatal("Ctrl+C 未取消运行上下文")
	}
	if left := a.Root().DrainSteer(); len(left) != 0 {
		t.Fatalf("排队消息未被丢弃: %v", left)
	}
	if got := blocksText(c); !strings.Contains(got, "已丢弃 2 条排队消息") {
		t.Fatalf("缺少丢弃提示:\n%s", got)
	}
}

// TestFinishStreamCanceledDropsSteer 被中断的回合收尾：残留排队消息一并
// 丢弃并提示，不转入新一轮（兜底 interruptKey 之后才排入的竞态）。
func TestFinishStreamCanceledDropsSteer(t *testing.T) {
	a, err := app.New(app.Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	c := newSteerChat(t, a)
	c.running = true
	_, cancel := context.WithCancel(c.ctx)
	c.cancel = cancel
	a.Root().Steer("迟到消息")

	c.finishStream(context.Canceled)
	if c.running {
		t.Fatal("被中断的回合不应续跑新一轮")
	}
	if left := a.Root().DrainSteer(); len(left) != 0 {
		t.Fatalf("残留排队消息未被丢弃: %v", left)
	}
	if got := blocksText(c); !strings.Contains(got, "已丢弃 1 条排队消息") {
		t.Fatalf("缺少丢弃提示:\n%s", got)
	}
}

// TestFinishStreamDrainsToNextTurn 正常结束：排队消息仍自动转入新一轮。
func TestFinishStreamDrainsToNextTurn(t *testing.T) {
	srv := probeLLM([]string{"done"})
	defer srv.Close()
	a, err := app.New(app.Options{
		Workspace: t.TempDir(),
		Client:    llm.NewClient(llm.Config{BaseURL: srv.URL, APIKey: "t", Model: "m"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	c := newSteerChat(t, a)
	c.running = true
	_, cancel := context.WithCancel(c.ctx)
	c.cancel = cancel
	a.Root().Steer("转入下一轮")

	c.finishStream(nil)
	if !c.running {
		t.Fatal("排队消息未转入新一轮")
	}
	if got := blocksText(c); !strings.Contains(got, "排队消息转入新一轮") {
		t.Fatalf("缺少转入提示:\n%s", got)
	}
	// 驱动新一轮落定（假 LLM 一次回包即结束），避免 goroutine 悬挂
	deadline := time.After(10 * time.Second)
	for c.running {
		select {
		case msg := <-c.events:
			c.handleMsg(msg)
		case <-deadline:
			t.Fatal("新一轮未落定")
		}
	}
}
