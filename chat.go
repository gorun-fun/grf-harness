// chat.go 默认入口三形态：全屏交互（TTY）、非 TTY 行模式 REPL、单任务直出。
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"git.gorun.fun/share/grf-harness/tui"
)

// runChat 全屏交互。
func runChat(resume, plan bool) {
	a := bootApp()
	defer a.Close()
	if resume {
		if sess, err := a.LatestSession(); err == nil && len(sess.Messages) > 0 {
			a.Restore(sess)
		} else {
			fmt.Println("没有可恢复的历史会话")
		}
	}
	if plan {
		a.Root().SetPlanMode(true)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	// SIGINT 由 raw 模式接管（ctrl+c 按键）；外部信号仍可终止
	chat := tui.NewChat(a)
	if err := chat.Run(ctx); err != nil {
		fatal(err)
	}
	fmt.Println("再见！")
}

// runPlain 非 TTY 回退：行模式 REPL。
func runPlain(resume bool) {
	a := bootApp()
	defer a.Close()
	if resume {
		if sess, err := a.LatestSession(); err == nil && len(sess.Messages) > 0 {
			a.Restore(sess)
			fmt.Println("已恢复上一次会话的上下文")
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	a.PlainREPL(ctx)
}

// runTaskCmd 单任务模式。
func runTaskCmd(args []string) {
	task := strings.TrimSpace(strings.Join(args, " "))
	a := bootApp()
	defer a.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := a.RunTask(ctx, task); err != nil {
		fatal(err)
	}
}
