// settings_cmd.go gh model / gh config / gh version 子命令：模型与配置的
// 脚本化读写（交互式配置页在 tui 包，经 runModelCmd 无参数形态进入）。
package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"git.gorun.fun/share/grf-harness/agent"
	"git.gorun.fun/share/grf-harness/app"
	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/tui"
)

// runModelCmd gh model 子命令。
func runModelCmd(args []string) {
	if len(args) == 0 {
		if !interactive() {
			fmt.Fprintln(os.Stderr, "非终端环境请使用: gh model list / gh model set <provider/model>")
			os.Exit(2)
		}
		if err := tui.RunModelPage(); err != nil {
			fatal(err)
		}
		return
	}
	switch args[0] {
	case "set":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "用法: gh model set <provider/model>")
			os.Exit(2)
		}
		s, err := config.LoadFileForUpdate()
		if err != nil {
			fatal(err)
		}
		provider, model := config.SplitDefault(args[1])
		p, ok := s.Providers[provider]
		if !ok {
			names := s.SortedProviderNames()
			if len(names) == 0 {
				fatal(fmt.Errorf("没有已配置的提供商，先运行 gh model 添加"))
			}
			fatal(fmt.Errorf("提供商 %q 不存在，可用: %v", provider, names))
		}
		if model == "" {
			model = p.Models[0]
		}
		p.AddModel(model)
		s.Default = provider + "/" + model
		if err := s.Save(); err != nil {
			fatal(err)
		}
		fmt.Printf("默认模型: %s/%s\n", provider, model)
	case "list":
		s, err := config.LoadFileForUpdate()
		if err != nil {
			fatal(err)
		}
		if len(s.Providers) == 0 {
			fmt.Println("（尚未配置任何提供商，运行 gh model 添加）")
			return
		}
		for _, pn := range s.SortedProviderNames() {
			p := s.Providers[pn]
			for _, m := range p.Models {
				mark := "  "
				if s.Default == pn+"/"+m {
					mark = "* "
				}
				fmt.Printf("%s%s/%s\n", mark, pn, m)
			}
		}
	default:
		fmt.Fprintf(os.Stderr, "未知子命令 %q，可用: set / list（无参数进入配置页）\n", args[0])
		os.Exit(2)
	}
}

// runConfigCmd gh config 子命令组。
func runConfigCmd(args []string) {
	s, err := config.LoadFileForUpdate()
	if err != nil {
		fatal(fmt.Errorf("读取配置: %w", err))
	}
	if len(args) == 0 {
		path, _ := config.Path()
		fmt.Printf("配置文件: %s\n", path)
		if s.MaxSteps > 0 {
			fmt.Printf("循环轮次上限: %d（自定义）\n", s.MaxSteps)
		} else {
			fmt.Printf("循环轮次上限: %d（默认）\n", agent.DefaultMaxSteps)
		}
		fmt.Printf("策略: %s\n", s.Policy)
		fmt.Printf("非交互自动批准: %v\n", s.AutoApprove)
		if s.SystemPrompt != "" {
			fmt.Println("提示词: 自定义（gh config prompt 预览）")
		} else {
			fmt.Println("提示词: 内置默认（gh config prompt set 自定义）")
		}
		return
	}
	switch args[0] {
	case "prompt":
		runPromptCmd(args[1:], s)
	case "steps":
		runStepsCmd(args[1:], s)
	case "policy":
		runPolicyCmd(args[1:], s)
	case "autoapprove":
		s.AutoApprove = len(args) < 2 || args[1] != "0" && args[1] != "false"
		if err := s.Save(); err != nil {
			fatal(err)
		}
		fmt.Printf("非交互自动批准: %v\n", s.AutoApprove)
	default:
		fmt.Fprintf(os.Stderr, "未知子命令 %q\n", args[0])
		os.Exit(2)
	}
}

func runStepsCmd(args []string, s *config.Settings) {
	if len(args) == 0 {
		if s.MaxSteps > 0 {
			fmt.Printf("循环轮次上限: %d（自定义）\n", s.MaxSteps)
		} else {
			fmt.Printf("循环轮次上限: %d（默认）\n", agent.DefaultMaxSteps)
		}
		return
	}
	if args[0] == "reset" {
		s.MaxSteps = 0
		if err := s.Save(); err != nil {
			fatal(err)
		}
		fmt.Printf("已恢复默认轮次（%d）\n", agent.DefaultMaxSteps)
		return
	}
	n := 0
	if _, err := fmt.Sscanf(args[0], "%d", &n); err != nil || n < 1 {
		fatal(fmt.Errorf("轮次应为正整数，收到 %q", args[0]))
	}
	s.MaxSteps = n
	if err := s.Save(); err != nil {
		fatal(err)
	}
	fmt.Printf("循环轮次上限已设为 %d\n", n)
}

// runPolicyCmd 统一规则串设置：write-workspace | read-workspace 竖线组合，
// "off" 全关（空值默认）；非法值归一为 write-workspace（保写侧）；不带参数显示当前值。
func runPolicyCmd(args []string, s *config.Settings) {
	cur := s.Policy
	if cur == "" {
		cur = config.NormalizePolicy("")
	}
	if len(args) == 0 {
		fmt.Printf("策略: %s（可选: write-workspace | read-workspace 组合，off 全关；非法值回退 write-workspace）\n", cur)
		return
	}
	v := config.NormalizePolicy(args[0])
	if args[0] != v && args[0] != "" {
		fmt.Printf("（%q 解析出错，已回退 write-workspace）\n", args[0])
	}
	s.Policy = v
	if err := s.Save(); err != nil {
		fatal(err)
	}
	fmt.Printf("策略已设为 %s\n", v)
}

func runPromptCmd(args []string, s *config.Settings) {
	if len(args) == 0 {
		a := bootApp()
		defer a.Close()
		fmt.Print(a.Root().SystemText())
		return
	}
	switch args[0] {
	case "set":
		var text string
		if len(args) > 1 {
			text = strings.TrimSpace(strings.Join(args[1:], " "))
		} else {
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				fatal(err)
			}
			text = strings.TrimSpace(string(data))
		}
		if text == "" {
			fatal(fmt.Errorf("提示词不能为空"))
		}
		s.SystemPrompt = text
		if err := s.Save(); err != nil {
			fatal(err)
		}
		fmt.Println("自定义提示词已保存（作为 custom 段生效：只替换身份/工作方式，内置工具使用说明段仍恒定拼装）")
	case "reset":
		s.SystemPrompt = ""
		if err := s.Save(); err != nil {
			fatal(err)
		}
		fmt.Println("已恢复默认提示词")
	default:
		fmt.Fprintf(os.Stderr, "未知子命令 %q，可用: set / reset\n", args[0])
		os.Exit(2)
	}
}

func printVersion() {
	fmt.Println("grf-harness", app.Version)
	path, _ := config.Path()
	fmt.Printf("配置文件: %s\n", path)
	if eff, err := config.Load(); err == nil {
		fmt.Printf("默认模型: %s/%s\n", eff.Provider, eff.Model)
		fmt.Printf("接口地址: %s\n", eff.BaseURL)
	}
}
