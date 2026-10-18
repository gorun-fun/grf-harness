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

	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/term"
)

// SlashCmds 斜杠指令表：两交互表面共用的事实来源（行模式 REPL 的 /help
// 与 TUI 的前缀提示/Tab 补全都从它生成）。alias 为同效别名（提示列表
// 合并成一行展示，前缀匹配别名也会命中主指令）。新增/改名指令时同步
// tui/input.go 的 /help 文案（那里保留 TUI 特有的详细描述）。
var SlashCmds = []struct{ Name, Alias, Desc string }{
	{Name: "/help", Desc: "显示本帮助"},
	{Name: "/init", Desc: "分析代码库并生成/更新 AGENTS.md"},
	{Name: "/model", Desc: "选择/切换模型"},
	{Name: "/resume", Desc: "恢复历史会话"},
	{Name: "/plan", Desc: "进入/退出计划模式"},
	{Name: "/goal", Desc: "查看当前 goal"},
	{Name: "/compact", Desc: "压缩会话历史为摘要"},
	{Name: "/new", Alias: "/clear", Desc: "保存当前会话并开新会话"},
	{Name: "/exit", Alias: "/quit", Desc: "退出"},
}

// SlashHintLine 单条指令的提示行："name alias  desc"（别名与主指令同行
// 展示，空格分隔，保证 Tab 补全取 fields[0] 为主指令）。
func SlashHintLine(name, alias, desc string) string {
	line := name
	if alias != "" {
		line += " " + alias
	}
	return line + "  " + desc
}

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
			// 指令行从 SlashCmds 表生成（表为两表面共用的事实来源），
			// 行模式特有的行（!cmd、自动保存说明）随后追加
			var b strings.Builder
			for _, cmd := range SlashCmds {
				if cmd.Name == "/init" {
					continue // 行模式没有 /init 的专属处理
				}
				fmt.Fprintln(&b, SlashHintLine(cmd.Name, cmd.Alias, cmd.Desc))
			}
			b.WriteString("!cmd    行首感叹号直接执行 shell 命令（如 !git status，不经模型）\n")
			b.WriteString("（会话在每轮回复后自动保存）")
			fmt.Println(b.String())
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
		case "/compact":
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			summary, err := a.Root().Compact(ctx)
			cancel()
			if err != nil {
				fmt.Println("压缩失败: " + err.Error())
			} else {
				fmt.Println("已压缩会话: " + term.FirstLine(summary, 0))
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
			title, rerr := a.ResumeByIndex(n)
			if rerr != nil {
				fmt.Fprintf(os.Stderr, "错误: %v\n", rerr)
				continue
			}
			fmt.Println("已恢复会话: " + title)
			continue
		}
		if strings.HasPrefix(input, "!") {
			cmd := strings.TrimSpace(strings.TrimPrefix(input, "!"))
			if cmd == "" {
				fmt.Fprintln(os.Stderr, "用法: !<命令>")
				continue
			}
			out, berr := a.RunBang(ctx, cmd)
			if berr != nil {
				fmt.Fprintf(os.Stderr, "错误: %s\n", berr)
			} else if out != "" {
				fmt.Println(out)
			}
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
	err = a.collectBackground(ctx, 3*time.Second, func(s string) {
		fmt.Printf("[info] %s\n", s)
	}, nil)
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
		for _, e := range s.ListModels() {
			mark := "  "
			if e.Default {
				mark = "* "
			}
			fmt.Fprintf(&b, "%s%s/%s\n", mark, e.Provider, e.Model)
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
	for _, e := range s.ListModels() {
		items = append(items, e.Provider+"/"+e.Model)
	}
	return items, cur, nil
}

