// steer_test.go 排队插话（steer）与 Ctrl+C 中断的交互语义：中断必须是
// 完全停下——丢弃未注入的排队消息、不触发续跑；正常结束的回合仍把
// 排队消息转入下一轮（回归保护，中断语义收紧不得波及正常收尾）。
package tui

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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

// TestInterruptDeliversRunDone 运行中 Ctrl+C 的回合落定消息（runDoneMsg）
// 必须送达事件循环：投递守卫不得用 runCtx（Ctrl+C 恰恰取消它，两个 case
// 同时就绪时 select 随机吞消息，running 永久卡 true，之后 Ctrl+C 全部
// 失效）。假 LLM 挂住流模拟模型持续吐字，多轮迭代使竞态必现。
func TestInterruptDeliversRunDone(t *testing.T) {
	entered := make(chan struct{}) // 请求到达（模型开始吐字）
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 排干请求体：未读 body 时服务端不启动客户端断连探测，
		// r.Context() 不会随 Ctrl+C 取消，handler 会挂死拖住 srv.Close
		io.Copy(io.Discard, r.Body)
		entered <- struct{}{}
		<-r.Context().Done() // 挂住直到 Ctrl+C 取消请求
	}))
	defer srv.Close()
	a, err := app.New(app.Options{
		Workspace: t.TempDir(),
		Client:    llm.NewClient(llm.Config{BaseURL: srv.URL, APIKey: "t", Model: "m"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	c := newSteerChat(t, a)
	for i := 0; i < 20; i++ {
		c.startRun("任务")
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatalf("第 %d 轮未进入 LLM 请求", i+1)
		}
		if quit := c.interruptKey(); quit {
			t.Fatal("运行中 Ctrl+C 不应退出")
		}
		deadline := time.After(10 * time.Second)
		for c.running {
			select {
			case msg := <-c.events:
				c.handleMsg(msg)
			case <-deadline:
				t.Fatalf("第 %d 次 Ctrl+C 后回合落定消息丢失（running 卡 true，中断失效）", i+1)
			}
		}
	}
}

// TestFinishRunWaitsGoroutineNotFlag 运行中退出的收尾等待：等的是运行
// goroutine 的退出信号（runDone），而非只由事件循环复位的 running 标志
// ——事件循环此刻正阻塞在 finishRun 里，等它必然空转满 2 秒。
func TestFinishRunWaitsGoroutineNotFlag(t *testing.T) {
	entered := make(chan struct{}) // 请求到达（回合在跑）
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		entered <- struct{}{}
		<-r.Context().Done() // 挂住直到取消
	}))
	defer srv.Close()
	a, err := app.New(app.Options{
		Workspace: t.TempDir(),
		Client:    llm.NewClient(llm.Config{BaseURL: srv.URL, APIKey: "t", Model: "m"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	c := newSteerChat(t, a)
	c.startRun("任务")
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("未进入 LLM 请求")
	}
	t0 := time.Now()
	c.finishRun()
	// 旧实现在此恒定白等满 2 秒；取消传播 + goroutine 退出是毫秒级，
	// 1.5 秒上限留足 CI 余量仍能区分两者
	if el := time.Since(t0); el >= 1500*time.Millisecond {
		t.Fatalf("finishRun 耗时 %v：取消后运行 goroutine 毫秒级即应落定", el)
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
