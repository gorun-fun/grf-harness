// connect.go gh connect/disconnect weixin：微信 ClawBot 渠道的守护进程
// 生命周期管理。登录（扫码/配对码）在父进程前台完成，登录态落盘后再
// re-exec 自身守护化（日志 ~/.grf/weixin.log，pid ~/.grf/weixin.pid）。
// 平台差异（setsid / 进程存活探测）在 daemon_unix.go / daemon_other.go。
package weixin

import (
	"context"
	"errors"
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
)

// Connect connect 子命令:连接消息渠道,当前支持 weixin(微信 ClawBot)。
// 默认拉起独立守护进程(输出落 ~/.grf/weixin.log,由 Disconnect 停止);
// 带 --daemon 为子进程内部入口,前台直跑。可选项:
//
//	-prompt <s>      自定义系统提示词
//	-workspace <dir> agent 工作区(默认 ~/.grf/weixin/ws；微信消息是外部
//	               输入，不再默认整个 home)
func Connect(args []string) {
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
	workspace := fs.String("workspace", "", "agent 工作区(默认 ~/.grf/weixin/ws)")
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
		if err := Run(ctx, *prompt, *workspace); err != nil {
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
	needLogin := !HasSavedLogin() // 首次连接必然要扫码
	if !needLogin && !*fresh {
		if askReuse() == "new" {
			*fresh = true
		}
	}
	if *fresh {
		if err := ResetState(); err != nil {
			fatal(fmt.Errorf("清空登录态: %w", err))
		}
		needLogin = true
	}

	// 新建 ClawBot:前台把扫码/配对码流程走完,登录态落盘后再守护化。
	if needLogin {
		if err := LoginInteractive(ctx); err != nil {
			fatal(err)
		}
	}

	self, err := os.Executable()
	if err != nil {
		fatal(fmt.Errorf("定位可执行文件: %w", err))
	}
	logPath := filepath.Join(config.Home(), "weixin.log")
	// 日志含完整消息文本与扫码登录链接（他者扫码即可接管会话），0600
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
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
	// pid 文件以 O_EXCL 原子创建做拉起互斥：并发两次 connect 只有一个
	// 成功（旧竞态：双方都过了"无 pid"预检后各起一个守护进程，长轮询
	// 同一 token 互踩）；登录等耗时流程都在此前，抢占点紧贴 spawn
	pidPath := filepath.Join(config.Home(), "weixin.pid")
	claim, aerr := claimPidFile(pidPath)
	if aerr != nil {
		fatal(fmt.Errorf("另一实例正在启动或已在运行(如需重启请先 gh disconnect weixin): %w", aerr))
	}
	if err := cmd.Start(); err != nil {
		claim.Close()
		_ = os.Remove(pidPath)
		fatal(fmt.Errorf("启动守护进程: %w", err))
	}
	if _, err := claim.WriteString(strconv.Itoa(cmd.Process.Pid)); err != nil {
		fatal(fmt.Errorf("写 pid 文件: %w", err))
	}
	claim.Close()
	fmt.Printf("🚀 微信 agent 守护进程已启动(pid %d)\n", cmd.Process.Pid)
	if *fresh {
		fmt.Println("   模式: 新建 ClawBot(旧登录态已丢弃)")
	}
	fmt.Printf("   日志: %s(扫码链接/运行过程见此)\n", logPath)
	fmt.Printf("   停止: gh disconnect weixin\n")
}

// Disconnect disconnect 子命令:停止 weixin 守护进程。
func Disconnect(args []string) {
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
		removeDaemonFiles()
		return
	}
	_ = proc.Signal(syscall.SIGTERM)
	// 优雅退出窗口:notifystart/stop 等收尾最多等 10 秒
	for i := 0; i < 100; i++ {
		time.Sleep(100 * time.Millisecond)
		if !procAlive(pid) {
			fmt.Printf("🛑 微信 agent(pid %d)已停止\n", pid)
			removeDaemonFiles()
			return
		}
	}
	_ = proc.Kill()
	removeDaemonFiles()
	fmt.Printf("已强制停止微信 agent(pid %d)\n", pid)
}

// fatal 打印错误并退出(connect/disconnect 属 CLI 入口,直接退出可接受)。
func fatal(err error) {
	fmt.Fprintln(os.Stderr, "错误:", err)
	os.Exit(1)
}

// askReuse 检测到旧登录态时询问:继续用之前的 ClawBot 还是新建。
// 返回 "reuse"/"new";非 TTY 默认复用。
func askReuse() string {
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

// claimPidFile 以 O_EXCL 原子创建抢占 pid 文件（互斥语义）：成功返回
// 打开的句柄由调用方写入 pid；文件已存在且其中进程仍活着报错（已在
// 运行），进程已死视为陈旧残留——清掉重试一次。
func claimPidFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		return f, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	if weixinDaemonPID() > 0 {
		return nil, fmt.Errorf("already running")
	}
	_ = os.Remove(path)
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
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

// removeDaemonFiles 清理 pid 文件(日志保留供查看)。
func removeDaemonFiles() {
	_ = os.Remove(filepath.Join(config.Home(), "weixin.pid"))
}
