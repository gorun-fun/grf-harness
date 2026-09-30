// bash.go bash 工具：bash -c 执行，每次独立 shell（无状态跨调用），
// workdir 定位；非零退出以 [exit code: N] 报告；长输出截尾、全文落盘；
// run_in_background 立即返回 job id（job_output/job_kill 管理）；前台
// 命令超时未结束不杀——转后台 job 交接（promoteOnTimeout，对齐 dsh）。
//
// 沙箱：配置 policy 含 write-workspace 且系统存在 sandbox-exec（macOS）时，
// 命令被限制为"全盘只读 + 仅工作区/tmp 可写"；检测到拒绝特征时输出
// [sandbox: file access denied under write-workspace mode] 标记。被拒后
// 可按一次性重试协议携带 sandbox_permissions+justification 升级（宿主
// 审批）。policy 不含 write-workspace 时不做 OS 级限制（拒绝标记不会出现，升级请求会
// 被按"无此前拒绝"拒绝）。
package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// BashDeps bash 工具依赖。
type BashDeps struct {
	Workspace string
	Fs        *FsPolicy // 仅读策略开关（Limits；bash 的升级记录在本地 deniedBash，不经它）
	Jobs      *JobManager
	Artifacts *Artifacts
	// 沙箱开关实时读 d.Fs.Limits()（set_policy 可运行时调整 policy）
	Host func() Host // 懒取宿主：注册后 SetHost 注入的真宿主才能收到审批询问
	// Env 懒取附加环境变量（$DSH_*/$GRF_* 事实变量）：每次命令执行时求值，
	// 切换模型后新建 bash 进程的 GRF_MODEL 即时反映。可为 nil。
	Env func() []string
}

// fsLimits 从 FsPolicy 实时读取策略开关；无 FsPolicy 视为不限制（兜底）。
func (d *BashDeps) fsLimits() (observe, readLimit bool) {
	if d.Fs == nil {
		return false, false
	}
	return d.Fs.Limits()
}

// fsLimitsOn 是否启用 write-workspace OS 级沙箱（拒绝检测用）。
func (d *BashDeps) fsLimitsOn() bool {
	observe, _ := d.fsLimits()
	return observe
}

const (
	bashDefaultTimeoutMS = 120000
	bashMaxTimeoutMS     = 600000
	bashOutputLimit      = 64 << 10 // 内联输出上限（截尾）
)

// sandboxExecPath 探测 sandbox-exec（仅 macOS 有意义）。
var sandboxExecPath = func() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	p, err := exec.LookPath("/usr/bin/sandbox-exec")
	if err != nil {
		return ""
	}
	return p
}()

// shellSpec bash 工具的执行载体。契约（spec 描述）是 `bash -c`：unix 与
// 装有 Git Bash 的 windows 都能原生满足；windows 无 bash.exe 时退回
// PowerShell（参数包装见 powerShellArgs），并由 ShellNote 告知模型。
type shellSpec struct {
	name string // "bash" / "powershell"（提示词与诊断用）
	exe  string
	args func(command string) []string
}

var bashShell = shellSpec{
	name: "bash", exe: "bash",
	args: func(c string) []string { return []string{"-c", c} },
}

// powerShellArgs PowerShell 参数包装：-NoProfile 隔离用户配置（保证
// $LASTEXITCODE 初始为 null）；脚本前导统一输出编码为 UTF-8（默认 OEM
// 码页会把中文输出成乱码）；尾部把最后一条原生命令的退出码透传为进程
// 退出码——PowerShell 默认不透传，bash 契约的 [exit code: N] 会错报 0。
// 命令内的 exit N 直接生效（尾部不再执行），语义与 bash 一致。
func powerShellArgs(command string) []string {
	script := "$OutputEncoding=[Console]::OutputEncoding=[System.Text.Encoding]::UTF8\n" +
		command +
		"\nif ($null -ne $LASTEXITCODE -and $LASTEXITCODE -ne 0) { exit $LASTEXITCODE }" +
		"\nif (-not $?) { exit 1 }"
	return []string{"-NoProfile", "-Command", script}
}

// isWSLShim 判断 bash.exe 是否 System32 的 WSL 启动器：它把命令跑在
// Linux 侧（工作区经 /mnt 挂载、$GRF_* 环境变量不透传），与工具契约
// 的本机语义不符，须跳过。
func isWSLShim(p string) bool {
	return strings.Contains(strings.ToLower(p), `\system32\`)
}

// resolveShell 解析本机 shell（纯函数供跨平台测试）：windows 依次探测
// Git Bash 常规安装位、PATH 上非 WSL 的 bash.exe，均无则 PowerShell
// （pwsh 优先于系统自带 powershell）；其余平台恒为 bash。
// Git Bash 安装位先于 PATH：System32 的 WSL shim 常驻 PATH，PATH 顺序
// 不可控。
func resolveShell(goos string, lookPath func(string) (string, error), env func(string) string) shellSpec {
	if goos != "windows" {
		return bashShell
	}
	for _, key := range []string{"ProgramFiles", "ProgramFiles(x86)"} {
		if dir := env(key); dir != "" {
			p := filepath.Join(dir, "Git", "bin", "bash.exe")
			if _, err := os.Stat(p); err == nil {
				return shellSpec{name: "bash", exe: p, args: bashShell.args}
			}
		}
	}
	if p, err := lookPath("bash"); err == nil && !isWSLShim(p) {
		return shellSpec{name: "bash", exe: p, args: bashShell.args}
	}
	if p, err := lookPath("pwsh"); err == nil {
		return shellSpec{name: "powershell", exe: p, args: powerShellArgs}
	}
	return shellSpec{name: "powershell", exe: "powershell", args: powerShellArgs}
}

// activeShell 本机实际使用的 shell（进程级缓存：PATH/安装态在一次运行
// 内视为不变）。
var activeShell = sync.OnceValue(func() shellSpec {
	return resolveShell(runtime.GOOS, exec.LookPath, os.Getenv)
})

// ShellNote 系统提示词的 shell 事实行：bash 时为空（与 spec 契约一致，
// 无需说明）；windows 退回 PowerShell 时告知模型写对应语法，避免按
// bash 语法构造命令（&&、$(...)、2>/dev/null 等在 PowerShell 不通）。
func ShellNote() string {
	if activeShell().name == "bash" {
		return ""
	}
	return "The bash tool finds no bash.exe on this Windows machine and executes commands through PowerShell: compose PowerShell-compatible syntax — single quotes for strings, backtick escapes, $env:NAME for environment variables — not POSIX shell syntax (no $(...), 2>/dev/null, or && chains in Windows PowerShell 5.1).\n"
}

// sandboxProfile 生成工作区沙箱 profile：写侧默认拒绝一切写，只放行工作区
// （含符号链接展开后的真实路径）、临时目录与 /dev/null；不能写成
// "默认允许 + 枚举系统目录拒绝"——那样家目录等其余路径的写/删不受控。
// 读侧（readLimit，policy 的 read-workspace）用黑名单：拒绝 /Users、
// /root、/home 等用户目录后放行工作区（deny-then-allow，后写的 allow
// 覆盖 deny）。不能用"默认拒绝读"——macOS 上进程启动需要探测不全的
// 隐藏读路径，默认拒绝会令任何命令直接 SIGABRT。
func sandboxProfile(workspace string, readLimit bool) string {
	real := workspace
	if r, err := filepath.EvalSymlinks(workspace); err == nil {
		real = r
	}
	p := fmt.Sprintf(`
(version 1)
(allow default)
(deny file-write*)
(allow file-write*
  (subpath %q)
  (subpath %q)
  (subpath "/private/tmp")
  (subpath "/tmp")
  (subpath "/var/folders")
  (literal "/dev/null"))
`, workspace, real)
	if readLimit {
		p += fmt.Sprintf(`
(deny file-read*
  (subpath "/Users")
  (subpath "/root")
  (subpath "/home"))
(allow file-read*
  (subpath %q)
  (subpath %q))
`, workspace, real)
	}
	return p
}

var deniedMarker = "[sandbox: file access denied under write-workspace mode]"

// bashArgs bash 工具参数（具名类型供 spec 交叉校验测试反射 json tag）。
type bashArgs struct {
	Command            string `json:"command"`
	Description        string `json:"description"`
	TimeoutMS          int    `json:"timeoutMs"`
	Workdir            string `json:"workdir"`
	RunInBackground    bool   `json:"run_in_background"`
	SandboxPermissions string `json:"sandbox_permissions"`
	Justification      string `json:"justification"`
}

// RegisterBash 注册 bash 工具。
func RegisterBash(r *Registry, d BashDeps) {
	var deniedBash atomic.Value // string：最近一次被拒的命令（升级重试校验）

	r.Register("bash", func(ctx context.Context, args string) (Result, error) {
		var a bashArgs
		if err := decodeArgs(args, &a); err != nil {
			return Result{}, err
		}
		if strings.TrimSpace(a.Command) == "" {
			return Result{}, fmt.Errorf("command is required")
		}
		if a.Workdir == "" {
			a.Workdir = d.Workspace
		} else if !filepath.IsAbs(a.Workdir) {
			a.Workdir = filepath.Join(d.Workspace, a.Workdir)
		}

		// 升级重试协议
		unconfined := false
		if a.SandboxPermissions != "" {
			if strings.TrimSpace(a.Justification) == "" {
				return Result{}, fmt.Errorf("sandbox_permissions requires a one-sentence justification")
			}
			prev, _ := deniedBash.Load().(string)
			if prev != a.Command {
				return Result{}, fmt.Errorf("sandbox_permissions is only valid as a one-shot retry of a command the sandbox just denied; there is no recorded denial for this exact command — run it without sandbox_permissions first and read the denial marker")
			}
			var host Host
			if d.Host != nil {
				host = d.Host()
			}
			if host == nil || !host.ApproveEscalation(ctx, "bash", a.Command, a.SandboxPermissions, a.Justification).Approved {
				return Result{}, fmt.Errorf("the escalation was rejected (or no host is available to approve it) — this is final for this command: stop and explain; other commands may still be attempted")
			}
			deniedBash.Store("")
			unconfined = true
		}

		build := func(runCtx context.Context, unconfined bool) *exec.Cmd {
			sh := activeShell()
			var cmd *exec.Cmd
			observe, readLimit := d.fsLimits()
			if !unconfined && observe && sandboxExecPath != "" {
				cmd = exec.CommandContext(runCtx, sandboxExecPath, "-p", sandboxProfile(d.Workspace, readLimit), "/bin/bash", "-c", a.Command)
			} else {
				cmd = exec.CommandContext(runCtx, sh.exe, sh.args(a.Command)...)
			}
			cmd.Dir = a.Workdir
			env := os.Environ()
			if d.Env != nil {
				env = append(env, d.Env()...)
			}
			cmd.Env = env
			cmd.WaitDelay = 5 * time.Second
			// 停止语义集中在此（前台/后台共用）：unix 先 SIGINT 给清理机会，
			// 强杀由 WaitDelay 兜底；windows 无跨进程信号，taskkill 终止进程树
			cmd.Cancel = func() error { return stopProcess(cmd) }
			return cmd
		}

		// 后台作业：立即返回 job id
		if a.RunInBackground {
			id := d.Jobs.StartStream("stream", firstNonEmpty(a.Description, a.Command), func(jctx context.Context, emit func(string)) error {
				cmd := build(jctx, unconfined)
				// job_kill 经 ctx 取消 → build 已装 Cancel（停止语义见彼处注
				// 释）；WaitDelay 兜底强杀并关闭管道——置 nil 会让进程永不
				// 停止（ctx 完成后仅等管道，作业僵尸运行）。
				var buf bytes.Buffer
				cmd.Stdout = writerFunc(func(p []byte) (int, error) {
					buf.Write(p)
					emit(string(p))
					return len(p), nil
				})
				cmd.Stderr = cmd.Stdout.(writerFunc)
				err := cmd.Run()
				if ee, ok := err.(*exec.ExitError); ok {
					emit(fmt.Sprintf("\n[exit code: %d]\n", ee.ExitCode()))
				}
				return err
			})
			return TextResult(fmt.Sprintf("background job started: %s (read with job_output, stop with job_kill)", id)), nil
		}

		// 前台执行（promoteOnTimeout 语义，对齐 dsh）：命令超时仍未结束
		// 时不杀进程——转入后台 job 继续跑，本次调用一次性带回已产出
		// 输出与 job id，job_output 从读取游标接续、job_kill 停止。无
		// JobManager 可晋升时退回旧语义（到期 SIGINT，WaitDelay 兜底
		// SIGKILL）。
		timeout := time.Duration(clamp(a.TimeoutMS, bashDefaultTimeoutMS, bashMaxTimeoutMS)) * time.Millisecond
		// 命令运行在独立 ctx（非回合 ctx 派生）：晋升为后台 job 后命令归
		// job 所有，须存活到 job_kill/自然结束，不随本回合取消而死；晋升
		// 之前的回合取消仍要终止命令（看护 goroutine），未晋升时随调用
		// 返回回收。
		runCtx, cancel := context.WithCancel(context.Background())
		promoted := make(chan struct{})
		var promoteOnce sync.Once
		markPromoted := func() { promoteOnce.Do(func() { close(promoted) }) }
		defer func() {
			select {
			case <-promoted:
				// 已晋升：命令归 job 所有，不随调用返回终止
			default:
				cancel()
				markPromoted() // 看护 goroutine 退出
			}
		}()
		watchDone := make(chan struct{})
		defer close(watchDone)
		go func() {
			select {
			case <-ctx.Done(): // 晋升前回合取消：终止命令
				cancel()
			case <-promoted: // 晋升后归 job 所有：不再随回合取消
			case <-watchDone:
			}
		}()

		// 交接锁：晋升时把 job 的 emit 安装到进程输出写路径上——既有
		// 缓冲内容一次性转发（前缀恰好一次），此后增量由写路径直发 job。
		var mu sync.Mutex
		var buf bytes.Buffer
		var jobEmit func(string)
		w := writerFunc(func(p []byte) (int, error) {
			mu.Lock()
			buf.Write(p)
			emit := jobEmit
			mu.Unlock()
			if emit != nil {
				emit(string(p))
			}
			return len(p), nil
		})

		runOnce := func(unconf bool) (out, handoff string, timedOut bool, err error) {
			mu.Lock()
			buf.Reset()
			jobEmit = nil
			mu.Unlock()
			cmd := build(runCtx, unconf)
			cmd.Stdout = w
			cmd.Stderr = w
			done := make(chan error, 1)
			go func() { done <- cmd.Run() }()
			timer := time.NewTimer(timeout)
			defer timer.Stop()
			select {
			case err = <-done:
				return buf.String(), "", false, err
			case <-timer.C:
			}
			if d.Jobs == nil {
				cancel() // 无 job 注册表可晋升：到期终止（旧超时语义）
				err = <-done
				return "", "", true, err
			}
			ready := make(chan struct{})
			markPromoted() // 命令自此归 job 所有，看护不再随回合取消终止它
			id := d.Jobs.StartStream("stream", firstNonEmpty(a.Description, a.Command), func(jctx context.Context, emit func(string)) error {
				go func() {
					<-jctx.Done() // job_kill → 终止真进程（SIGINT，WaitDelay 兜底）
					cancel()
				}()
				mu.Lock()
				prefix := buf.String()
				jobEmit = emit
				mu.Unlock()
				emit(prefix)
				close(ready) // 前缀已入 job 缓冲，交接读取可消费
				rerr := <-done
				if ee, ok := rerr.(*exec.ExitError); ok {
					emit(fmt.Sprintf("\n[exit code: %d]\n", ee.ExitCode()))
				}
				return rerr
			})
			select {
			case <-ready:
			case <-time.After(2 * time.Second): // 兜底：交接信号异常时不卡死调用
			}
			// 一次性消费读取：把目前为止的输出带回（job 读取游标随之前
			// 移，job_output 只返回其后的新增，与交接文本不重复）。
			preview, perr := d.Jobs.Output(ctx, id, false, 0)
			preview = strings.TrimSuffix(strings.TrimRight(preview, "\n"), "[status: running]")
			if perr != nil {
				preview = ""
			}
			return "", fmt.Sprintf("[still running after %s; moved to background job %s]\n%s\nfollow with job_output(job_id=%q) — set wait: true to block until it settles; job_kill stops it",
				timeout, id, preview, id), false, nil
		}

		out, handoff, timedOut, err := runOnce(unconfined)
		if handoff != "" {
			return TextResult(handoff), nil
		}

		// 沙箱拒绝检测：非零退出 + 权限拒绝特征。越权即问：挂起征求宿主
		// 批准（UI 交互，不随工具调用取消），批准则脱沙箱重跑同一命令，
		// 拒绝（或无宿主）则终局返回——不依赖模型配合 sandbox_permissions
		// 重试协议。
		if err != nil && d.fsLimitsOn() && !unconfined &&
			(strings.Contains(out, "Operation not permitted") || isSignalTrapped(err)) {
			deniedBash.Store(a.Command)
			approved := false
			var host Host
			if d.Host != nil {
				host = d.Host()
			}
			if host != nil {
				just := firstNonEmpty(a.Description, a.Command)
				approved = host.ApproveEscalation(context.Background(), "bash", a.Command, "danger-full-access", just).Approved
			}
			if approved {
				deniedBash.Store("")
				out, handoff, timedOut, err = runOnce(true)
				if handoff != "" {
					return TextResult(handoff), nil
				}
			} else {
				deniedBash.Store("") // 拒绝即终局：消费记录，重试不再询问
				trunc := tailBytes(out, bashOutputLimit)
				return TextResult(fmt.Sprintf("%s\n%s\n\nthe command was blocked by the file sandbox and the user declined to grant it wider access — this is final for this command: stop and explain; do not try other ways around it (other commands may still be attempted)",
					trunc, deniedMarker)), nil
			}
		}

		if timedOut {
			return Result{}, fmt.Errorf("command timed out after %s (kill after SIGINT grace)", timeout)
		}
		if len(out) > bashOutputLimit {
			full := ""
			if path, serr := d.Artifacts.Save("bash", out); serr == nil {
				full = fmt.Sprintf("full output saved to %s\n", path)
			}
			out = full + "…(truncated to tail)\n" + tailBytes(out, bashOutputLimit)
		}
		if err != nil {
			if ctx.Err() != nil {
				return Result{}, ctx.Err()
			}
			if ee, ok := err.(*exec.ExitError); ok {
				out += fmt.Sprintf("\n[exit code: %d]", ee.ExitCode())
				return TextResult(out), nil
			}
			return TextResult(fmt.Sprintf("[exit code: -1] %v\n%s", err, out)), nil
		}
		if strings.TrimSpace(out) == "" {
			return TextResult("(no output) [exit code: 0]"), nil
		}
		return TextResult(out + "\n[exit code: 0]"), nil
	})
}

// isSignalTrapped sandbox-exec 拒绝常以 SIGTRAP 终止。
func isSignalTrapped(err error) bool {
	ee, ok := err.(*exec.ExitError)
	if !ok {
		return false
	}
	ws, ok := ee.Sys().(interface{ Signal() int })
	return ok && ws.Signal() == 5 // SIGTRAP
}

type writerFunc func(p []byte) (int, error)

func (w writerFunc) Write(p []byte) (int, error) { return w(p) }

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func clamp(v, def, max int) int {
	if v <= 0 {
		return def
	}
	if v > max {
		return max
	}
	return v
}

func tailBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
