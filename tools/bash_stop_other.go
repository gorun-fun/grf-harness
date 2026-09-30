//go:build !windows

package tools

import (
	"os"
	"os/exec"
)

// stopProcess 停止命令进程：先 SIGINT 给清理机会，强杀由 exec 的
// WaitDelay 兜底。
func stopProcess(cmd *exec.Cmd) error { return cmd.Process.Signal(os.Interrupt) }
