//go:build windows

package tools

import (
	"os/exec"
	"strconv"
)

// stopProcess 停止命令进程：Windows 无跨进程信号（Signal(os.Interrupt)
// 直接报错），taskkill /T /F 终止整棵进程树（PowerShell/bash 起的子进程
// 一并回收；console ctrl 事件需要进程组且控制台应用常不响应，不走优雅
// 路径）。目标已退出时 taskkill 非零退出，视为达成——返回 nil 让 exec
// 报 ctx 错误而非把 taskkill 的错误当命令结果，极端失败由 WaitDelay
// 兜底强杀。
func stopProcess(cmd *exec.Cmd) error {
	_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
	return nil
}
