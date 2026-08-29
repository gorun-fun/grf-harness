// render.go 直写 stdout 的流式渲染（单任务 / 非 TTY REPL 共用）与
// RunTask 单任务模式。不做 markdown 渲染——按原始文本输出（需求如此）。
package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"git.gorun.fun/share/grf-harness/agent"
	"git.gorun.fun/share/grf-harness/term"
)

// stdoutUI 流式直写：思考进度行（\r 覆写）+ 正文原文增量输出 + 事件行。
// 尾部换行挂账（pending）：模型可能在正文尾部多发换行，延迟到下一段
// 内容或收尾时才输出，避免与收尾换行叠加出多余空行。
type stdoutUI struct {
	tty     bool
	w       io.Writer
	start   time.Time
	chars   int
	active  bool
	mdOpen  bool // 正文已开始输出（事件前需换行）
	pending int  // 挂账的尾部连续换行数
}

// flushPending 落盘挂账的尾部换行。
func (s *stdoutUI) flushPending() {
	for i := 0; i < s.pending; i++ {
		fmt.Fprintln(s.w)
	}
	s.pending = 0
}

func (s *stdoutUI) reasoning(delta string) {
	if !s.active {
		s.active = true
		s.start = time.Now()
	}
	s.chars += len([]rune(delta))
	if !s.tty {
		return
	}
	fmt.Fprintf(s.w, "\r\033[2K\033[2m💭 思考中… %ds · %d 字\033[0m", int(time.Since(s.start).Seconds()), s.chars)
}

func (s *stdoutUI) content(delta string) {
	s.fold()
	if !s.mdOpen {
		s.mdOpen = true
	}
	body := strings.TrimRight(delta, "\n")
	if body != "" {
		s.flushPending()
		fmt.Fprint(s.w, body)
	}
	s.pending += len(delta) - len(body)
}

func (s *stdoutUI) fold() {
	if !s.active {
		return
	}
	s.active = false
	summary := fmt.Sprintf("💭 已深度思考 %ds，%d 字", int(time.Since(s.start).Seconds()), s.chars)
	if s.tty {
		fmt.Fprintf(s.w, "\r\033[2K\033[2m%s\033[0m\n", summary)
	} else {
		fmt.Fprintln(s.w, summary)
	}
}

func (s *stdoutUI) printEvent(kind, detail string) {
	s.fold()
	s.pending = 0
	if s.mdOpen {
		fmt.Fprintln(s.w)
		s.mdOpen = false
	}
	prefix := "🔧"
	switch kind {
	case "tool":
		prefix = "[tool]"
	case "result":
		prefix = "[->]  "
	case "todos":
		prefix = "[todo]"
	case "goal":
		prefix = "[goal]"
	case "phase":
		prefix = "[flow]"
	case "notice":
		prefix = "[info]"
	}
	if s.tty {
		fmt.Fprintf(s.w, "\033[2m%s %s\033[0m\n", prefix, detail)
	} else {
		fmt.Fprintf(s.w, "%s %s\n", prefix, detail)
	}
}

// attach 挂到 agent。
func (s *stdoutUI) attach(ag *agent.Agent) {
	ag.SetStreamHooks(s.content, s.reasoning)
	ag.SetOnEvent(func(kind, detail string) {
		switch kind {
		case "tool", "result", "todos", "goal", "phase", "notice":
			s.printEvent(kind, detail)
		case "done":
			s.fold()
			s.pending = 0
			if s.mdOpen {
				fmt.Fprintln(s.w)
				s.mdOpen = false
			}
		}
	})
}

// RunTask 单次任务模式：执行任务、流式直出到 stdout、保存会话。
// 注意：内部会为根 agent 挂上自己的 stdout 渲染（覆盖之前设置的
// OnEvent/StreamHooks 回调）——需要自定义回调的嵌入方直接用
// Root().Run / RunAutoGoal 驱动，不要经过本方法。
func (a *App) RunTask(ctx context.Context, task string) error {
	ui := &stdoutUI{tty: term.IsTTY(int(os.Stdout.Fd())), w: os.Stdout}
	ui.attach(a.Root())
	host := a.opts.Host
	if host == nil {
		host = NewTaskHost(a.Harness.Settings.AutoApprove)
		a.Root().SetHost(host)
	}
	_, err := a.Root().RunAutoGoal(ctx, task)
	if err != nil {
		return err
	}
	// 后台子代理收尾：短暂等待并吸收通知
	if notices := a.Root().RunAndWaitBackground(10 * time.Second); len(notices) > 0 {
		for _, n := range notices {
			ui.printEvent("notice", term.FirstLine(n, 0))
		}
		if !ui.mdOpen {
			fmt.Fprintln(os.Stdout)
		}
		_, err = a.Root().RunAutoGoal(ctx, "Collect the settled background results above and give the user your final consolidated answer now.")
		if err != nil {
			return err
		}
	}
	a.Harness.Artifacts.Cleanup()
	if _, serr := a.SaveSession(); serr != nil {
		fmt.Fprintf(os.Stderr, "保存会话失败: %v\n", serr)
	}
	return nil
}
