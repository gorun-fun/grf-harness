//go:build linux

package term

import (
	"syscall"
	"unsafe"
)

const (
	ioctlRead  = syscall.TCGETS
	ioctlWrite = syscall.TCSETS
)

func ioctlTermios(fd int, req uintptr, t *syscall.Termios) error {
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), req,
		uintptr(unsafe.Pointer(t)), 0, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
