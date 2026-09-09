// grf-harness（gh）：零依赖的终端 agent harness。默认进入全屏交互界面；
// `gh <任务描述>` 为单任务模式直接输出结果；亦可作为 Go 模块被其他应用
// 引用（模块路径 git.gorun.fun/share/grf-harness，app/agent/tools/llm
// 等包均可导入）。
//
// 用法：
//
//	gh                     全屏交互（-resume 恢复上次会话，-plan 计划模式）
//	gh <任务描述>          单任务模式：流式直出，完成后退出
//	gh model               模型配置页（TTY 全屏；model set/list 脚本化）
//	gh config [...]        配置摘要 / prompt / steps / policy
//	gh connect weixin      连接微信 ClawBot(后台守护进程);disconnect 停止
//	gh server              启动 Responses API 接入服务(ORS 兼容)
//	gh plugins             插件体检（download 子命令从官方服务器安装插件）
//	gh update              检查并更新自身（全屏交互启动时也会提示新版本）
//	gh version             版本与当前生效配置
//
// 子命令实现按功能线条分文件：chat.go（三种默认形态）、settings_cmd.go
// （model/config/version）、server_cmd.go（Responses API 服务）、
// plugins_cmd.go（MCP 插件体检 + 官方插件下载）、update_cmd.go（自更新，
// 探测逻辑在 selfupdate 包）；connect/disconnect weixin 在 weixin 包
// （weixin/connect.go）。
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"git.gorun.fun/share/grf-harness/app"
	"git.gorun.fun/share/grf-harness/term"
	"git.gorun.fun/share/grf-harness/weixin"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "model":
			runModelCmd(args[1:])
			return
		case "config":
			runConfigCmd(args[1:])
			return
		case "connect":
			weixin.Connect(args[1:])
			return
		case "disconnect":
			weixin.Disconnect(args[1:])
			return
		case "server":
			runServerCmd(args[1:])
			return
		case "plugins":
			runPluginsCmd(args[1:])
			return
		case "update":
			runUpdateCmd(args[1:])
			return
		case "version", "-version", "--version":
			printVersion()
			return
		case "help", "-h", "--help":
			fmt.Printf(usageText+"\n", app.Version)
			return
		}
		if !strings.HasPrefix(args[0], "-") {
			// 主程序 + 任务描述：单任务直出
			runTaskCmd(args)
			return
		}
	}

	fs := flag.NewFlagSet("gh", flag.ExitOnError)
	resume := fs.Bool("resume", false, "恢复上一次会话的上下文")
	plan := fs.Bool("plan", false, "以计划模式启动（exit_plan_mode 呈交计划）")
	_ = fs.Parse(args)

	if interactive() {
		runChat(*resume, *plan)
		return
	}
	runPlain(*resume)
}

const usageText = `grf-harness %s - 零依赖终端 agent harness（gh）

用法:
  gh                     全屏交互（默认模式）
  gh -resume             恢复上一次会话
  gh -plan               以计划模式启动
  gh <任务描述>           单任务模式：直接流式输出结果后退出
  gh !<命令>              直接执行 shell 命令（不经模型）
  gh model               模型配置页（多提供商管理）
  gh model set <p/m>     设置默认模型（provider/model）
  gh model list          逐行列出已配置 提供商/模型（* 为默认）
  gh config              配置摘要
  gh config prompt [set|reset]      自定义系统提示词
  gh config steps [n|reset]         工具循环轮次上限
  gh config policy [write-workspace|read-workspace|off]  统一规则串（默认 off）
  gh connect weixin      连接微信 ClawBot(后台守护进程,消息由本地 agent 回答)
  gh disconnect weixin   断开并停止微信 ClawBot 守护进程
  gh server              启动 Responses API 接入服务(ORS 兼容,默认 127.0.0.1:7400)
  gh plugins             列出 MCP 插件（本地 stdio / 远程 HTTP）与工具清单
  gh plugins download    从官方下载服务器安装插件到 ~/.grf/plugins/
                         (可选名称过滤; -version 指定版本,缺省配套当前主程序)
  gh update              检查并更新自身到下载服务器最新版(-y 跳过确认)
  gh version             版本号与当前生效配置

全屏快捷键: ↑↓ 历史 · PgUp/PgDn/滚轮 翻页 · Ctrl+O 展开思考/工具块 ·
F2 计划模式 · Tab 补全指令/切换鼠标 · Ctrl+C 中断执行（空闲退出） ·
Ctrl+D 退出 · /model /resume /plan /goal 指令 · !cmd 直接执行命令 ·
鼠标单击展开、拖选复制

配置: ~/.grf/settings.json（GRF_HOME 可重定位）；环境变量
OPENAI_BASE_URL / OPENAI_API_KEY / GRF_MODEL 临时覆盖。`

func interactive() bool {
	return term.IsTTY(int(os.Stdin.Fd())) && term.IsTTY(int(os.Stdout.Fd()))
}

// bootApp 构建应用（读配置、连模型）。
func bootApp() *app.App {
	a, err := app.New(app.Options{})
	if err != nil {
		fatal(err)
	}
	return a
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "错误:", err)
	os.Exit(1)
}
