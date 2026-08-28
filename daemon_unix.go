//go:build darwin || linux

package main

import "syscall"

// detachProcAttr 守护进程脱离会话（setsid），Ctrl+C 不影响前台。
func detachProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// procAlive 探测 pid 进程是否存在（信号 0 不实际发送）。
func procAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
