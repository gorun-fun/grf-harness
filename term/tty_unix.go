//go:build darwin || linux

package term

import (
	"os"
	"syscall"
	"unsafe"
)

// Size 返回终端 (列, 行)；失败返回 (0,0)。
func Size(fd int) (int, int) {
	var ws struct{ row, col, xpixel, ypixel uint16 }
	if _, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd),
		syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws)), 0, 0, 0); errno != 0 {
		return 0, 0
	}
	return int(ws.col), int(ws.row)
}

// resizeSignal 窗口尺寸变化信号（unix: SIGWINCH；Windows 无此信号，
// 由 tty_other.go 提供空实现）。
func resizeSignal() os.Signal { return syscall.SIGWINCH }

// IsTTY 判断 fd 是否字符设备（终端）。
func IsTTY(fd int) bool {
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return false
	}
	return st.Mode&syscall.S_IFMT == syscall.S_IFCHR
}

// Raw 终端 raw 模式句柄：关闭 ICANON/ECHO/ISIG 等，Close 恢复原属性。
type Raw struct {
	fd  int
	old syscall.Termios
}

// OpenRaw 把 fd 切到 raw 模式。失败返回错误（非终端或系统不支持）。
func OpenRaw(fd int) (*Raw, error) {
	var old syscall.Termios
	if err := tcget(fd, &old); err != nil {
		return nil, err
	}
	raw := old
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if err := tcset(fd, &raw); err != nil {
		return nil, err
	}
	return &Raw{fd: fd, old: old}, nil
}

// Close 恢复原终端属性（幂等安全由调用方保证只调一次）。
func (r *Raw) Close() {
	if r == nil || r.fd < 0 {
		return
	}
	_ = tcset(r.fd, &r.old)
	r.fd = -1
}

func tcget(fd int, t *syscall.Termios) error {
	return ioctlTermios(fd, ioctlRead, t)
}

func tcset(fd int, t *syscall.Termios) error {
	return ioctlTermios(fd, ioctlWrite, t)
}

// StdinSize/StdoutSize 便捷封装。
func StdoutSize() (int, int) { return Size(int(os.Stdout.Fd())) }
