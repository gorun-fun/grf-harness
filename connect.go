// connect.go gh connect/disconnect weixin：微信 ClawBot 渠道的守护进程
// 生命周期管理。登录（扫码/配对码）在父进程前台完成，登录态落盘后再
// re-exec 自身守护化（日志 ~/.grf/weixin.log，pid ~/.grf/weixin.pid）。
// 平台差异（setsid / 进程存活探测）在 daemon_unix.go / daemon_other.go。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/term"
	"git.gorun.fun/share/grf-harness/weixin"
)

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
