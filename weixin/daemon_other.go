//go:build !(darwin || linux)

package weixin

import (
	"os"
	"syscall"
)

// 非 unix 平台（windows 等）的回退：无 setsid 会话概念，进程探测走
// OpenProcess（os.FindProcess 内部实现）。

// detachProcAttr windows 无 setsid，普通启动即可。
func detachProcAttr() *syscall.SysProcAttr { return nil }

// procAlive windows 的 os.FindProcess 打开进程句柄，进程不存在即报错。
func procAlive(pid int) bool {
	_, err := os.FindProcess(pid)
	return err == nil
}
