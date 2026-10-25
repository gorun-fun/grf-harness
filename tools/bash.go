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
	t := &bashTool{deps: d}
	r.Register("bash", t.handle)
}

// bashTool bash 工具实例：deps 之外只多一个跨调用共享的升级记录。
type bashTool struct {
	deps   BashDeps
	denied atomic.Value // string：最近一次被拒的命令（升级重试校验）
}

// handle 工具入口：参数解析 → 一次性升级协议 → 前台/后台分发。
func (t *bashTool) handle(ctx context.Context, args string) (Result, error) {
	a, err := t.parseArgs(args)
	if err != nil {
		return Result{}, err
	}
	unconfined, err := t.authorizeEscalation(ctx, a)
	if err != nil {
		return Result{}, err
	}
	if a.RunInBackground {
		return t.runBackground(a, unconfined), nil
	}
	return t.runForeground(ctx, a, unconfined)
}

// parseArgs 解码参数并归一 workdir（相对路径相对工作区）。
func (t *bashTool) parseArgs(args string) (bashArgs, error) {
	var a bashArgs
	if err := decodeArgs(args, &a); err != nil {
		return a, err
	}
	if strings.TrimSpace(a.Command) == "" {
		return a, fmt.Errorf("command is required")
	}
	if a.Workdir == "" {
		a.Workdir = t.deps.Workspace
	} else if !filepath.IsAbs(a.Workdir) {
		a.Workdir = filepath.Join(t.deps.Workspace, a.Workdir)
	}
	return a, nil
}

// authorizeEscalation sandbox_permissions 一次性升级重试协议：须携带
// 理由、精确匹配最近被拒的命令、宿主批准后本次脱沙箱执行。
func (t *bashTool) authorizeEscalation(ctx context.Context, a bashArgs) (bool, error) {
	if a.SandboxPermissions == "" {
		return false, nil
	}
	if strings.TrimSpace(a.Justification) == "" {
		return false, fmt.Errorf("sandbox_permissions requires a one-sentence justification")
	}
	prev, _ := t.denied.Load().(string)
	if prev != a.Command {
		return false, fmt.Errorf("sandbox_permissions is only valid as a one-shot retry of a command the sandbox just denied; there is no recorded denial for this exact command — run it without sandbox_permissions first and read the denial marker")
	}
	var host Host
	if t.deps.Host != nil {
		host = t.deps.Host()
	}
	if host == nil || !host.ApproveEscalation(ctx, "bash", a.Command, a.SandboxPermissions, a.Justification).Approved {
		return false, fmt.Errorf("the escalation was rejected (or no host is available to approve it) — this is final for this command: stop and explain; other commands may still be attempted")
	}
	t.denied.Store("")
	return true, nil
}

// build 构造执行命令（沙箱 profile 或裸 shell；前台/后台共用）。
func (t *bashTool) build(runCtx context.Context, a bashArgs, unconfined bool) *exec.Cmd {
	sh := activeShell()
	var cmd *exec.Cmd
	observe, readLimit := t.deps.fsLimits()
	if !unconfined && observe && sandboxExecPath != "" {
		cmd = exec.CommandContext(runCtx, sandboxExecPath, "-p", sandboxProfile(t.deps.Workspace, readLimit), "/bin/bash", "-c", a.Command)
	} else {
		cmd = exec.CommandContext(runCtx, sh.exe, sh.args(a.Command)...)
	}
	cmd.Dir = a.Workdir
	// 子进程环境剔除疑似密钥变量（env.go：模型可令命令打印环境，供应商
	// 密钥与工具链秘密不应顺带暴露），再叠加 harness 事实变量
	var extra []string
	if t.deps.Env != nil {
		extra = t.deps.Env()
	}
	cmd.Env = ChildEnv(extra)
	cmd.WaitDelay = 5 * time.Second
	// 停止语义集中在此（前台/后台共用）：unix 先 SIGINT 给清理机会，
	// 强杀由 WaitDelay 兜底；windows 无跨进程信号，taskkill 终止进程树
	cmd.Cancel = func() error { return stopProcess(cmd) }
	return cmd
}

// runBackground 后台作业路径：立即返回 job id。
func (t *bashTool) runBackground(a bashArgs, unconfined bool) Result {
	id := t.deps.Jobs.StartStream("stream", firstNonEmpty(a.Description, a.Command), func(jctx context.Context, emit func(string)) error {
		cmd := t.build(jctx, a, unconfined)
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
	return TextResult(fmt.Sprintf("background job started: %s (read with job_output, stop with job_kill)", id))
}

// runForeground 前台执行（promoteOnTimeout 语义，对齐 dsh）：命令超时
// 仍未结束时不杀进程——转入后台 job 继续跑，本次调用一次性带回已产出
// 输出与 job id，job_output 从读取游标接续、job_kill 停止。无 JobManager
// 可晋升时退回旧语义（到期 SIGINT，WaitDelay 兜底 SIGKILL）。
func (t *bashTool) runForeground(ctx context.Context, a bashArgs, unconfined bool) (Result, error) {
	f := newForeState(t, a)
	defer f.release()          // 已晋升则命令归 job 所有，未晋升则终止回收
	watchDone := make(chan struct{})
	defer close(watchDone)     // 先于 release 关闭：看护 goroutine 退出
	go f.watch(ctx, watchDone) // 晋升前的回合取消仍要终止命令

	out, handoff, timedOut, err := f.runOnce(ctx, unconfined)
	if handoff != "" {
		return TextResult(handoff), nil
	}

	res, out, timedOut, err, handled := t.handleDenial(ctx, f, out, timedOut, err, unconfined)
	if handled {
		return res, nil
	}
	return t.formatResult(ctx, a, out, timedOut, err)
}

// handleDenial 沙箱拒绝检测与越权即问：非零退出 + 权限拒绝特征
// （"Operation not permitted" 或 sandbox-exec 的 SIGTRAP）时挂起征求
// 宿主批准（UI 交互，不随工具调用取消），批准则脱沙箱重跑同一命令
// （结果经 out/timedOut/err 带回、handled=false 继续走格式化），拒绝
// （或无宿主）则终局返回（handled=true）——不依赖模型配合
// sandbox_permissions 重试协议。
func (t *bashTool) handleDenial(ctx context.Context, f *foreState, out string, timedOut bool, err error, unconfined bool) (res Result, out2 string, timedOut2 bool, err2 error, handled bool) {
	if err == nil || !t.deps.fsLimitsOn() || unconfined ||
		(!strings.Contains(out, "Operation not permitted") && !isSignalTrapped(err)) {
		return Result{}, out, timedOut, err, false
	}
	t.denied.Store(f.a.Command)
	approved := false
	var host Host
	if t.deps.Host != nil {
		host = t.deps.Host()
	}
	if host != nil {
		just := firstNonEmpty(f.a.Description, f.a.Command)
		approved = host.ApproveEscalation(context.Background(), "bash", f.a.Command, "danger-full-access", just).Approved
	}
	if !approved {
		t.denied.Store("") // 拒绝即终局：消费记录，重试不再询问
		trunc := tailBytes(out, bashOutputLimit)
		return TextResult(fmt.Sprintf("%s\n%s\n\nthe command was blocked by the file sandbox and the user declined to grant it wider access — this is final for this command: stop and explain; do not try other ways around it (other commands may still be attempted)",
			trunc, deniedMarker)), out, timedOut, err, true
	}
	t.denied.Store("")
	var handoff string
	out, handoff, timedOut, err = f.runOnce(ctx, true)
	if handoff != "" {
		return TextResult(handoff), out, timedOut, err, true
	}
	return Result{}, out, timedOut, err, false
}

// formatResult 前台结果的统一收尾：超时报错、超长截尾 + 全文落盘、
// 退出码格式化。
func (t *bashTool) formatResult(ctx context.Context, a bashArgs, out string, timedOut bool, err error) (Result, error) {
	timeout := time.Duration(clamp(a.TimeoutMS, bashDefaultTimeoutMS, bashMaxTimeoutMS)) * time.Millisecond
	if timedOut {
		return Result{}, fmt.Errorf("command timed out after %s (kill after SIGINT grace)", timeout)
	}
	if len(out) > bashOutputLimit {
		full := ""
		if path, serr := t.deps.Artifacts.Save("bash", out); serr == nil {
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
}

// foreState 一次前台执行的运行态：命令的独立 ctx（晋升后归 job 所有，
// 不随本回合取消而死）、晋升标记与交接锁——promoteOnTimeout 的并发
// 细节全部集中于此。
type foreState struct {
	t       *bashTool
	a       bashArgs
	timeout time.Duration

	runCtx   context.Context
	cancel   context.CancelFunc
	promoted chan struct{}
	promote  sync.Once

	// 交接锁：晋升时把 job 的 emit 安装到进程输出写路径上——既有缓冲
	// 内容一次性转发（前缀恰好一次），此后增量由写路径直发 job。
	mu      sync.Mutex
	buf     bytes.Buffer
	jobEmit func(string)
}

func newForeState(t *bashTool, a bashArgs) *foreState {
	runCtx, cancel := context.WithCancel(context.Background())
	return &foreState{
		t: t, a: a,
		timeout: time.Duration(clamp(a.TimeoutMS, bashDefaultTimeoutMS, bashMaxTimeoutMS)) * time.Millisecond,
		runCtx: runCtx, cancel: cancel,
		promoted: make(chan struct{}),
	}
}

// markPromoted 标记命令已归 job 所有（恰好一次）。
func (f *foreState) markPromoted() { f.promote.Do(func() { close(f.promoted) }) }

// watch 看护：晋升前的回合取消要终止命令；晋升后归 job 所有、不再随
// 回合取消；调用返回（watchDone）即退场。
func (f *foreState) watch(ctx context.Context, done <-chan struct{}) {
	select {
	case <-ctx.Done(): // 晋升前回合取消：终止命令
		f.cancel()
	case <-f.promoted: // 晋升后归 job 所有：不再随回合取消
	case <-done:
	}
}

// release 调用返回时的回收：未晋升则终止命令并放看护退出。
func (f *foreState) release() {
	select {
	case <-f.promoted:
		// 已晋升：命令归 job 所有，不随调用返回终止
	default:
		f.cancel()
		f.markPromoted() // 看护 goroutine 退出
	}
}

// write 进程输出写路径：先入缓冲；晋升后增量直发 job。
func (f *foreState) write(p []byte) (int, error) {
	f.mu.Lock()
	f.buf.Write(p)
	emit := f.jobEmit
	f.mu.Unlock()
	if emit != nil {
		emit(string(p))
	}
	return len(p), nil
}

// runOnce 跑一次命令直到完成或超时。超时且可晋升时交接后台 job 并返回
// 交接回执（handoff 非空即终局）；无 JobManager 时到期终止（旧超时
// 语义，timedOut=true）。
func (f *foreState) runOnce(ctx context.Context, unconfined bool) (out, handoff string, timedOut bool, err error) {
	f.mu.Lock()
	f.buf.Reset()
	f.jobEmit = nil
	f.mu.Unlock()
	cmd := f.t.build(f.runCtx, f.a, unconfined)
	w := writerFunc(f.write)
	cmd.Stdout = w
	cmd.Stderr = w
	done := make(chan error, 1)
	go func() { done <- cmd.Run() }()
	timer := time.NewTimer(f.timeout)
	defer timer.Stop()
	select {
	case err = <-done:
		// cmd.Run 返回即输出管道已排干，缓冲无并发写，直接读
		return f.buf.String(), "", false, err
	case <-timer.C:
	}
	if f.t.deps.Jobs == nil {
		f.cancel() // 无 job 注册表可晋升：到期终止（旧超时语义）
		err = <-done
		return "", "", true, err
	}
	return "", f.promoteOnTimeout(ctx, done), false, nil
}

// promoteOnTimeout 超时晋升：把仍在运行的命令交接给后台 job——job 的
// emit 安装到写路径（既有缓冲一次性转发，前缀恰好一次），此后 job_kill
// 经 ctx 取消终止真进程；交接回执带一次性读取的已有输出（job 读取游标
// 随之前移，job_output 只返回其后的新增，与交接文本不重复）。
func (f *foreState) promoteOnTimeout(ctx context.Context, done <-chan error) string {
	ready := make(chan struct{})
	f.markPromoted() // 命令自此归 job 所有，看护不再随回合取消终止它
	id := f.t.deps.Jobs.StartStream("stream", firstNonEmpty(f.a.Description, f.a.Command), func(jctx context.Context, emit func(string)) error {
		go func() {
			<-jctx.Done() // job_kill → 终止真进程（SIGINT，WaitDelay 兜底）
			f.cancel()
		}()
		f.mu.Lock()
		prefix := f.buf.String()
		f.jobEmit = emit
		f.mu.Unlock()
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
	// 一次性消费读取：把目前为止的输出带回。
	preview, perr := f.t.deps.Jobs.Output(ctx, id, false, 0)
	preview = strings.TrimSuffix(strings.TrimRight(preview, "\n"), "[status: running]")
	if perr != nil {
		preview = ""
	}
	return fmt.Sprintf("[still running after %s; moved to background job %s]\n%s\nfollow with job_output(job_id=%q) — set wait: true to block until it settles; job_kill stops it",
		f.timeout, id, preview, id)
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
