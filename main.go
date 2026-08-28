// grf-harness（gh）：零依赖的终端 agent harness。默认进入全屏交互界面；
// `gh <任务描述>` 为单任务模式直接输出结果；亦可作为 Go 模块被其他应用
// 引用（grf/app、grf/agent、grf/tools、grf/llm 等包）。
//
// 用法：
//
//	gh                     全屏交互（-resume 恢复上次会话，-plan 计划模式）
//	gh <任务描述>          单任务模式：流式直出，完成后退出
//	gh model               模型配置页（TTY 全屏；model set/list 脚本化）
//	gh config [...]        配置摘要 / prompt / steps / sandbox / fspolicy
//	gh connect weixin      连接微信 ClawBot(后台守护进程);disconnect 停止
//	gh version             版本与当前生效配置
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"git.gorun.fun/share/grf-harness/agent"
	"git.gorun.fun/share/grf-harness/app"
	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/server"
	"git.gorun.fun/share/grf-harness/term"
	"git.gorun.fun/share/grf-harness/tui"
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
			runConnectCmd(args[1:])
			return
		case "disconnect":
			runDisconnectCmd(args[1:])
			return
		case "server":
			runServerCmd(args[1:])
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
  gh model               模型配置页（多提供商管理）
  gh model set <p/m>     设置默认模型（provider/model）
  gh model list          逐行列出已配置 提供商/模型（* 为默认）
  gh config              配置摘要
  gh config prompt [set|reset]      自定义系统提示词
  gh config steps [n|reset]         工具循环轮次上限
  gh config sandbox [off|workspace-write]  bash 文件沙箱
  gh config fspolicy [observe|off]         写前先读策略
  gh connect weixin      连接微信 ClawBot(后台守护进程,消息由本地 agent 回答)
  gh disconnect weixin   断开并停止微信 ClawBot 守护进程
  gh server              启动 Responses API 接入服务(ORS 兼容,默认 127.0.0.1:7400)
  gh version             版本号与当前生效配置

全屏快捷键: ↑↓ 历史 · PgUp/PgDn/滚轮 翻页 · Ctrl+O 展开思考/工具块 ·
F2 计划模式 · Tab 补全指令/切换鼠标 · Ctrl+C 中断执行（空闲退出） ·
Ctrl+D 退出 · /model /resume /plan /goal 指令 · 鼠标单击展开、拖选复制

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
		fmt.Printf("bash 沙箱: %s\n", s.Sandbox)
		fmt.Printf("写前先读策略: %s\n", s.FsPolicy)
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
	case "sandbox":
		runChoiceCmd(args[1:], s, "sandbox", "bash 文件沙箱", []string{"off", "workspace-write"})
	case "fspolicy":
		runChoiceCmd(args[1:], s, "fs_policy", "写前先读策略", []string{"observe", "off"})
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

func runChoiceCmd(args []string, s *config.Settings, field, name string, allowed []string) {
	get := func() string {
		if field == "sandbox" {
			if s.Sandbox == "" {
				return allowed[0]
			}
			return s.Sandbox
		}
		if s.FsPolicy == "" {
			return allowed[0]
		}
		return s.FsPolicy
	}
	set := func(v string) {
		if field == "sandbox" {
			s.Sandbox = v
		} else {
			s.FsPolicy = v
		}
	}
	if len(args) == 0 {
		fmt.Printf("%s: %s（可选: %s）\n", name, get(), strings.Join(allowed, " | "))
		return
	}
	v := args[0]
	ok := false
	for _, a := range allowed {
		if a == v {
			ok = true
		}
	}
	if !ok {
		fatal(fmt.Errorf("%s 可选值: %s", name, strings.Join(allowed, " | ")))
	}
	set(v)
	if err := s.Save(); err != nil {
		fatal(err)
	}
	fmt.Printf("%s已设为 %s\n", name, v)
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
		fmt.Println("自定义提示词已保存")
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

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "错误:", err)
	os.Exit(1)
}

// runConnectCmd connect 子命令:连接消息渠道,当前支持 weixin(微信 ClawBot)。
// 默认拉起独立守护进程(输出落 ~/.grf/weixin.log,由 disconnect 停止);
// 带 --daemon 为子进程内部入口,前台直跑。可选项:
//
//	-prompt <s>      自定义系统提示词
//	-workspace <dir> agent 工作区(默认用户 home)
func runConnectCmd(args []string) {
	// flag 包遇到首个位置参数即停止解析,这里把旗标前置,
	// 保证 "connect weixin -workspace xx" 与 "connect -workspace xx weixin" 均可。
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-prompt" || a == "-workspace" || a == "--prompt" || a == "--workspace" {
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "%s 需要一个值\n", a)
				os.Exit(2)
			}
			flags = append(flags, a, args[i+1])
			i++
		} else if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
		} else {
			pos = append(pos, a)
		}
	}
	fs := flag.NewFlagSet("connect", flag.ExitOnError)
	prompt := fs.String("prompt", "", "自定义系统提示词")
	workspace := fs.String("workspace", "", "agent 工作区(默认用户 home)")
	daemon := fs.Bool("daemon", false, "内部:守护进程前台直跑")
	fresh := fs.Bool("fresh", false, "内部:丢弃旧登录态,新建 ClawBot")
	_ = fs.Parse(append(flags, pos...))
	if fs.NArg() == 0 || fs.Arg(0) != "weixin" {
		fmt.Fprintln(os.Stderr, "用法: gh connect weixin [-prompt 提示词] [-workspace 目录]")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *daemon {
		// 登录流程已在父进程前台完成,这里直接用落盘的登录态收发消息。
		if err := weixin.Run(ctx, *prompt, *workspace); err != nil {
			fatal(err)
		}
		fmt.Println("已断开微信 ClawBot 连接")
		return
	}

	// 已在运行则不重复拉起
	if pid := weixinDaemonPID(); pid > 0 {
		fmt.Printf("微信 agent 已在运行(pid %d),如需重启请先 gh disconnect weixin\n", pid)
		return
	}

	// 存在旧登录态(如守护进程意外退出后残留):让用户选复用还是新建。
	needLogin := !weixin.HasSavedLogin() // 首次连接必然要扫码
	if !needLogin && !*fresh {
		if askWeixinReuse() == "new" {
			*fresh = true
		}
	}
	if *fresh {
		if err := weixin.ResetState(); err != nil {
			fatal(fmt.Errorf("清空登录态: %w", err))
		}
		needLogin = true
	}

	// 新建 ClawBot:前台把扫码/配对码流程走完,登录态落盘后再守护化。
	if needLogin {
		if err := weixin.LoginInteractive(ctx); err != nil {
			fatal(err)
		}
	}

	self, err := os.Executable()
	if err != nil {
		fatal(fmt.Errorf("定位可执行文件: %w", err))
	}
	logPath := filepath.Join(config.Home(), "weixin.log")
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fatal(fmt.Errorf("打开日志文件: %w", err))
	}
	defer logf.Close()

	childArgs := []string{"connect", "weixin", "--daemon"}
	if *prompt != "" {
		childArgs = append(childArgs, "-prompt", *prompt)
	}
	if *workspace != "" {
		childArgs = append(childArgs, "-workspace", *workspace)
	}
	cmd := exec.Command(self, childArgs...)
	cmd.Stdin = nil // /dev/null;扫码链接写日志,配对码输入在守护模式下不可用
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = detachProcAttr() // 脱离会话(unix setsid),Ctrl+C 不影响
	if err := cmd.Start(); err != nil {
		fatal(fmt.Errorf("启动守护进程: %w", err))
	}
	pidPath := filepath.Join(config.Home(), "weixin.pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0o644); err != nil {
		fatal(fmt.Errorf("写 pid 文件: %w", err))
	}
	fmt.Printf("🚀 微信 agent 守护进程已启动(pid %d)\n", cmd.Process.Pid)
	if *fresh {
		fmt.Println("   模式: 新建 ClawBot(旧登录态已丢弃)")
	}
	fmt.Printf("   日志: %s(扫码链接/运行过程见此)\n", logPath)
	fmt.Printf("   停止: gh disconnect weixin\n")
}

// askWeixinReuse 检测到旧登录态时询问:继续用之前的 ClawBot 还是新建。
// 返回 "reuse"/"new";非 TTY 默认复用。
func askWeixinReuse() string {
	if !term.IsTTY(int(os.Stdin.Fd())) || !term.IsTTY(int(os.Stdout.Fd())) {
		return "reuse"
	}
	fmt.Println("检测到之前的微信 ClawBot 登录态(守护进程可能意外退出了)。")
	fmt.Print("回车继续使用之前的 ClawBot,输入 n 新建一个: ")
	var ans string
	if _, err := fmt.Scanln(&ans); err != nil && ans == "" {
		return "reuse"
	}
	ans = strings.ToLower(strings.TrimSpace(ans))
	if ans == "n" || ans == "new" {
		return "new"
	}
	return "reuse"
}

// runServerCmd server 子命令：启动 Responses API 接入服务（ORS 兼容）。
// 可选项：
//
//	-addr <host:port>   监听地址（默认 127.0.0.1:7400；服务会执行 bash 与
//	                    文件工具，绑非环回地址等于暴露本机 RCE，谨慎）
//	-data <dir>         数据目录（默认 ~/.grf/server）
//	-token <token>      Bearer 令牌（默认读取/自动生成 <data>/token）
func runServerCmd(args []string) {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:7400", "监听地址(默认仅环回)")
	data := fs.String("data", "", "数据目录(默认 ~/.grf/server)")
	token := fs.String("token", "", "Bearer 令牌(默认读/生成 <data>/token)")
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "用法: gh server [-addr 127.0.0.1:7400] [-data 目录] [-token 令牌]")
		os.Exit(2)
	}
	if *data == "" {
		*data = filepath.Join(config.Home(), "server")
	}
	srv, err := server.New(*data, *token)
	if err != nil {
		fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()
	fmt.Printf("🚀 gh server 已启动: http://%s/v1/responses\n", *addr)
	fmt.Printf("   令牌: %s\n", srv.Token())
	fmt.Printf("   数据: %s(会话/存档/快照)\n", srv.DataDir())
	fmt.Printf("   接入: OpenAI Responses 形状,ORS 2026-04-24 合规;gh 扩展见 docs/server.md\n")
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fatal(err)
	}
	fmt.Println("gh server 已停止")
}

// runDisconnectCmd disconnect 子命令:停止 weixin 守护进程。
func runDisconnectCmd(args []string) {
	if len(args) == 0 || args[0] != "weixin" {
		fmt.Fprintln(os.Stderr, "用法: gh disconnect weixin")
		os.Exit(2)
	}
	pid := weixinDaemonPID()
	if pid <= 0 {
		fmt.Println("微信 agent 未在运行")
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		fmt.Printf("找不到进程 %d,清理 pid 文件\n", pid)
		removeWeixinFiles()
		return
	}
	_ = proc.Signal(syscall.SIGTERM)
	// 优雅退出窗口:notifystart/stop 等收尾最多等 10 秒
	for i := 0; i < 100; i++ {
		time.Sleep(100 * time.Millisecond)
		if !procAlive(pid) {
			fmt.Printf("🛑 微信 agent(pid %d)已停止\n", pid)
			removeWeixinFiles()
			return
		}
	}
	_ = proc.Kill()
	removeWeixinFiles()
	fmt.Printf("已强制停止微信 agent(pid %d)\n", pid)
}

// weixinDaemonPID 读取守护进程 pid;文件缺失或进程已死返回 0。
func weixinDaemonPID() int {
	b, err := os.ReadFile(filepath.Join(config.Home(), "weixin.pid"))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0
	}
	if !procAlive(pid) {
		return 0 // 进程不存在(或被别的进程复用前的窗口期)
	}
	return pid
}

// removeWeixinFiles 清理 pid 文件(日志保留供查看)。
func removeWeixinFiles() {
	_ = os.Remove(filepath.Join(config.Home(), "weixin.pid"))
}
