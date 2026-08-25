// bash.go bash 工具：bash -c 执行，每次独立 shell（无状态跨调用），
// workdir 定位；非零退出以 [exit code: N] 报告；长输出截尾、全文落盘；
// run_in_background 立即返回 job id（job_output/job_kill 管理）。
//
// 沙箱：配置 sandbox=workspace-write 且系统存在 sandbox-exec（macOS）时，
// 命令被限制为"全盘只读 + 仅工作区/tmp 可写"；检测到拒绝特征时输出
// [sandbox: file access denied under workspace-write mode] 标记。被拒后
// 可按一次性重试协议携带 sandbox_permissions+justification 升级（宿主
// 审批）。sandbox=off 时不做 OS 级限制（拒绝标记不会出现，升级请求会
// 被按"无此前拒绝"拒绝）。
package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

// BashDeps bash 工具依赖。
type BashDeps struct {
	Workspace string
	Host      Host
	Fs        *FsPolicy // 升级记录复用其 denied 表
	Jobs      *JobManager
	Artifacts *Artifacts
	SandboxOn bool     // 是否启用 workspace-write OS 级沙箱
	Env       []string // 附加环境变量（$DSH_*/$GRF_* 事实变量）
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

// sandboxProfile 生成限制写入的工作区沙箱 profile。
func sandboxProfile(workspace string) string {
	return fmt.Sprintf(`
(version 1)
(allow default)
(deny file-write*
  (subpath "/System")
  (subpath "/usr")
  (subpath "/Library")
  (subpath "/private/etc"))
(allow file-write*
  (subpath "%s")
  (subpath "/private/tmp")
  (subpath "/tmp")
  (subpath "/var/folders"))
`, workspace)
}

var deniedMarker = "[sandbox: file access denied under workspace-write mode]"

// RegisterBash 注册 bash 工具。
func RegisterBash(r *Registry, d BashDeps) {
	var deniedBash atomic.Value // string：最近一次被拒的命令（升级重试校验）

	r.Register("bash", func(ctx context.Context, args string) (Result, error) {
		var a struct {
			Command            string `json:"command"`
			Description        string `json:"description"`
			TimeoutMS          int    `json:"timeoutMs"`
			Workdir            string `json:"workdir"`
			RunInBackground    bool   `json:"run_in_background"`
			SandboxPermissions string `json:"sandbox_permissions"`
			Justification      string `json:"justification"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return Result{}, err
		}
		if strings.TrimSpace(a.Command) == "" {
			return Result{}, fmt.Errorf("command is required")
		}
		if a.Workdir == "" {
			a.Workdir = d.Workspace
		} else if !filepathIsAbs(a.Workdir) {
			a.Workdir = joinPath(d.Workspace, a.Workdir)
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
			if d.Host == nil || !d.Host.ApproveEscalation(ctx, "bash", a.Command, a.SandboxPermissions, a.Justification) {
				return Result{}, fmt.Errorf("the escalation was rejected (or no host is available to approve it) — this is final for this command: stop and explain; other commands may still be attempted")
			}
			deniedBash.Store("")
			unconfined = true
		}

		build := func(runCtx context.Context) *exec.Cmd {
			var cmd *exec.Cmd
			if !unconfined && d.SandboxOn && sandboxExecPath != "" {
				cmd = exec.CommandContext(runCtx, sandboxExecPath, "-p", sandboxProfile(d.Workspace), "/bin/bash", "-c", a.Command)
			} else {
				cmd = exec.CommandContext(runCtx, "bash", "-c", a.Command)
			}
			cmd.Dir = a.Workdir
			env := os.Environ()
			env = append(env, d.Env...)
			cmd.Env = env
			cmd.WaitDelay = 5 * time.Second
			return cmd
		}

		// 后台作业：立即返回 job id
		if a.RunInBackground {
			id := d.Jobs.StartStream("stream", firstNonEmpty(a.Description, a.Command), func(jctx context.Context, emit func(string)) error {
				cmd := build(jctx)
				cmd.Cancel = nil
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

		// 前台执行
		timeout := time.Duration(clamp(a.TimeoutMS, bashDefaultTimeoutMS, bashMaxTimeoutMS)) * time.Millisecond
		runCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		cmd := build(runCtx)
		cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) } // 超时先 SIGINT，给清理机会
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		err := cmd.Run()
		out := buf.String()

		// 沙箱拒绝检测：非零退出 + 权限拒绝特征
		if err != nil && d.SandboxOn && !unconfined &&
			(strings.Contains(out, "Operation not permitted") || isSignalTrapped(err)) {
			deniedBash.Store(a.Command)
			trunc := tailBytes(out, bashOutputLimit)
			return TextResult(fmt.Sprintf("%s\n%s\n\nthe command was blocked by the file sandbox. If a wider mode would let it succeed, retry the exact same command once with sandbox_permissions plus a one-sentence justification; do not try other ways around it",
				trunc, deniedMarker)), nil
		}

		if len(out) > bashOutputLimit {
			full := ""
			if path, serr := d.Artifacts.Save("bash", out); serr == nil {
				full = fmt.Sprintf("full output saved to %s\n", path)
			}
			out = full + "…(truncated to tail)\n" + tailBytes(out, bashOutputLimit)
		}
		if err != nil {
			if runCtx.Err() == context.DeadlineExceeded {
				return Result{}, fmt.Errorf("command timed out after %s (kill after SIGINT grace)", timeout)
			}
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

func filepathIsAbs(p string) bool { return len(p) > 0 && p[0] == '/' }

func joinPath(base, rel string) string { return base + "/" + rel }
