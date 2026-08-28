// repl.go 非 TTY（管道/重定向）回退的行模式 REPL：逐行读输入，回复
// 流式直出原文；斜杠指令与全屏版同构。
package app

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"git.gorun.fun/share/grf-harness/agent"
	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/store"
)

// PlainREPL 行模式 REPL（stdin/stdout 非 TTY 时使用）。
func (a *App) PlainREPL(ctx context.Context) {
	ui := &stdoutUI{tty: false, w: os.Stdout}
	ui.attach(a.Root())
	fmt.Printf("grf-harness %s - 输入消息开始对话，/help 查看命令\n", Version)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)

	for {
		fmt.Print("\n> ")
		if !scanner.Scan() {
			fmt.Println("\n再见！")
			return
		}
		input := strings.TrimSpace(scanner.Text())
		switch input {
		case "":
			continue
		case "/exit", "/quit":
			fmt.Println("再见！")
			return
		case "/help":
			fmt.Println(`/help   显示本帮助
/plan   进入计划模式（exit_plan_mode 呈计划）；再输一次退出
/model  查看已配置模型；/model 提供商/模型 切换
/resume 列出历史会话；/resume N 恢复第 N 条
/goal   查看当前 goal 状态
/new    保存当前会话并开新会话（/clear 同效）
/exit   退出（/quit 同效；会话在每轮回复后自动保存）`)
			continue
		case "/new", "/clear":
			a.NewSession()
			fmt.Println("已保存当前会话并开启新会话")
			continue
		case "/plan":
			a.Root().SetPlanMode(!a.Root().InPlanMode())
			fmt.Printf("计划模式: %v\n", a.Root().InPlanMode())
			continue
		case "/goal":
			if g := a.Harness.Goals.Marshal(); g != "" {
				fmt.Println(g)
			} else {
				fmt.Println("（当前无 goal）")
			}
			continue
		}
		if strings.HasPrefix(input, "/model") {
			arg := strings.TrimSpace(strings.TrimPrefix(input, "/model"))
			if text, err := a.ModelCmd(arg); err != nil {
				fmt.Fprintf(os.Stderr, "错误: %v\n", err)
			} else {
				fmt.Println(text)
			}
			continue
		}
		if strings.HasPrefix(input, "/resume") {
			arg := strings.TrimSpace(strings.TrimPrefix(input, "/resume"))
			if arg == "" {
				for i, s := range a.Harness.Store.ListSessions(20) {
					when := s.When.Format("01-02 15:04")
					if s.When.IsZero() {
						when = "更早"
					}
					fmt.Printf("%2d. %s · %s\n", i+1, when, s.Title)
				}
				fmt.Println("恢复: /resume N")
				continue
			}
			n, err := strconv.Atoi(arg)
			if err != nil || n < 1 {
				fmt.Fprintln(os.Stderr, "用法: /resume N")
				continue
			}
			sessions := a.Harness.Store.ListSessions(0)
			if n > len(sessions) {
				fmt.Fprintf(os.Stderr, "序号超出范围（共 %d 条会话）\n", len(sessions))
				continue
			}
			sess, err := store.LoadSessionFull(sessions[n-1].Path)
			if err != nil {
				fmt.Fprintf(os.Stderr, "错误: %v\n", err)
				continue
			}
			a.Restore(sess)
			fmt.Println("已恢复会话: " + sessions[n-1].Title)
			continue
		}
		if err := a.runInput(ctx, input); err != nil {
			if ctx.Err() != nil {
				fmt.Println("(已中断)")
				continue
			}
			fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		}
	}
}

// runInput 执行一条输入（goal 自动续跑 + 后台收尾 + 回合结束自动保存）。
func (a *App) runInput(ctx context.Context, input string) error {
	_, err := a.Root().RunAutoGoal(ctx, input)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	if notices := a.Root().RunAndWaitBackground(3 * time.Second); len(notices) > 0 {
		for _, n := range notices {
			fmt.Printf("[info] %s\n", firstLineOf(n))
		}
		_, err = a.Root().RunAutoGoal(ctx, "Collect the settled background results above and give the user your final consolidated answer now.")
	}
	a.AutoSave()
	return err
}

// modelCmd /model 指令：无参列出；带参切换。
func (a *App) ModelCmd(arg string) (string, error) {
	if arg == "" {
		s, err := config.LoadFileForUpdate()
		if err != nil {
			return "", err
		}
		if len(s.Providers) == 0 {
			return "（尚未配置任何提供商）\n添加: gh model", nil
		}
		var b strings.Builder
		if m := a.Root().Model(); m != "" {
			fmt.Fprintf(&b, "当前模型: %s\n", m)
		}
		for _, pn := range s.SortedProviderNames() {
			p := s.Providers[pn]
			models := append([]string(nil), p.Models...)
			for _, m := range models {
				mark := "  "
				if s.Default == pn+"/"+m {
					mark = "* "
				}
				fmt.Fprintf(&b, "%s%s/%s\n", mark, pn, m)
			}
		}
		b.WriteString("切换: /model <提供商/模型>")
		return b.String(), nil
	}
	provider, model := config.SplitDefault(arg)
	if err := a.SwitchModel(provider, model); err != nil {
		return "", err
	}
	return "已切换会话模型（仅本次会话，不改默认）: " + provider + "/" + model, nil
}

// ListModelChoices 全部可选 p/m（TUI 弹窗用）。
func (a *App) ListModelChoices() (items []string, cur string, err error) {
	s, err := config.LoadFileForUpdate()
	if err != nil {
		return nil, "", err
	}
	name, _, model := s.Active()
	if name != "" && model != "" {
		cur = name + "/" + model
	}
	for _, pn := range s.SortedProviderNames() {
		p := s.Providers[pn]
		models := append([]string(nil), p.Models...)
		for _, m := range models {
			items = append(items, pn+"/"+m)
		}
	}
	return items, cur, nil
}

// Agent 别名导出（嵌入方按 agent.Agent 使用）。
type Agent = agent.Agent
