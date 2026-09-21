//go:build windows

package term

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

// Windows 控制台实现：输入句柄开 ENABLE_VIRTUAL_TERMINAL_INPUT（方向键等
// 按键经 conhost 翻译为 ANSI 序列，直接喂给 input.go 的既有解析器，Ctrl+C
// 变为字节 \x03 而非信号，行为对齐 unix raw 模式）、行/回显/QuickEdit 全关；
// 输出句柄开 ENABLE_VIRTUAL_TERMINAL_PROCESSING（备用屏幕/清行/光标定位等
// 转义序列生效）。读取仍走 os.Stdin——Go 运行时对控制台句柄经 ReadConsoleW
// 做 UTF-16→UTF-8 转换，字符模式下增量返回，term.ReadInput 无需改动。

// SetConsoleMode 与 GetConsoleScreenBufferInfo 不在 stdlib syscall 导出面，
// 经 LazyDLL 取用（仍为零第三方依赖）。
var (
	kernel32                   = syscall.NewLazyDLL("kernel32.dll")
	procSetConsoleMode         = kernel32.NewProc("SetConsoleMode")
	procGetConsoleScreenBuffer = kernel32.NewProc("GetConsoleScreenBufferInfo")
)

// 控制台模式位（wincon.h）。仅列本文件用到的，不与 syscall 导出重复。
const (
	enableProcessedInput         = 0x0001
	enableLineInput              = 0x0002
	enableEchoInput              = 0x0004
	enableWindowInput            = 0x0008
	enableMouseInput             = 0x0010
	enableInsertMode             = 0x0020
	enableQuickEditMode          = 0x0040
	enableExtendedFlags          = 0x0080
	enableVirtualTerminalInput   = 0x0200
	enableVirtualTerminalProcess = 0x0004
)

type coord struct{ X, Y int16 }

type smallRect struct{ Left, Top, Right, Bottom int16 }

type consoleScreenBufferInfo struct {
	Size              coord
	CursorPosition    coord
	Attributes        uint16
	Window            smallRect
	MaximumWindowSize coord
}

// IsTTY 判断 fd 对应句柄是否控制台（GetConsoleMode 仅对控制台成功）。
func IsTTY(fd int) bool {
	var mode uint32
	return syscall.GetConsoleMode(syscall.Handle(fd), &mode) == nil
}

// sizeHandle 查询控制台句柄的可见窗口 (列, 行)；失败返回 (0,0)。
func sizeHandle(h syscall.Handle) (int, int) {
	var info consoleScreenBufferInfo
	r, _, _ := procGetConsoleScreenBuffer.Call(uintptr(h), uintptr(unsafe.Pointer(&info)))
	if r == 0 {
		return 0, 0
	}
	return int(info.Window.Right - info.Window.Left + 1),
		int(info.Window.Bottom - info.Window.Top + 1)
}

// Size 返回终端 (列, 行)；失败返回 (0,0)。
func Size(fd int) (int, int) { return sizeHandle(syscall.Handle(fd)) }

// StdoutSize 便捷封装。
func StdoutSize() (int, int) { return Size(int(os.Stdout.Fd())) }

// Raw 控制台 raw 模式句柄：输入/输出双句柄模式改写，Close 恢复原值。
// fd 参数忽略——Screen 传入的是 stdout，而 raw 化必须作用到 stdin/stdout
// 两个句柄（Windows 上它们是独立句柄，不像 unix 指向同一 tty 设备）。
type Raw struct {
	in, out syscall.Handle
	oldIn   uint32
	oldOut  uint32
}

// OpenRaw 把控制台切到 raw + VT 模式。任一句柄不是控制台、或不支持 VT
// 转义（老 conhost）时返回错误，上层回退纯文本行模式。
func OpenRaw(fd int) (*Raw, error) {
	_ = fd
	in, err := syscall.GetStdHandle(syscall.STD_INPUT_HANDLE)
	if err != nil || in == 0 {
		return nil, errors.New("stdin is not a console")
	}
	out, err := syscall.GetStdHandle(syscall.STD_OUTPUT_HANDLE)
	if err != nil || out == 0 {
		return nil, errors.New("stdout is not a console")
	}
	var oldIn, oldOut uint32
	if syscall.GetConsoleMode(in, &oldIn) != nil || syscall.GetConsoleMode(out, &oldOut) != nil {
		return nil, errors.New("stdin/stdout is not a console")
	}
	// 输入：关行缓冲/回显/信号处理/QuickEdit（鼠标框选会冻结输出）等熟模式
	// 能力，开 VT 输入翻译。EXTENDED_FLAGS 是改动 QuickEdit 的前置位。
	newIn := oldIn &^ (enableProcessedInput | enableLineInput | enableEchoInput |
		enableWindowInput | enableMouseInput | enableInsertMode | enableQuickEditMode)
	newIn |= enableExtendedFlags | enableVirtualTerminalInput
	if r, _, e := procSetConsoleMode.Call(uintptr(in), uintptr(newIn)); r == 0 {
		return nil, errors.New("console does not support VT input: " + e.Error())
	}
	// 输出：开 VT 处理，备用屏幕/整帧重画的转义序列才能生效。
	newOut := oldOut | enableVirtualTerminalProcess
	if r, _, e := procSetConsoleMode.Call(uintptr(out), uintptr(newOut)); r == 0 {
		procSetConsoleMode.Call(uintptr(in), uintptr(oldIn)) // 回滚输入侧
		return nil, errors.New("console does not support VT output: " + e.Error())
	}
	return &Raw{in: in, out: out, oldIn: oldIn, oldOut: oldOut}, nil
}

// Close 恢复两侧控制台模式（幂等安全由调用方保证只调一次）。
func (r *Raw) Close() {
	if r == nil || r.in == 0 {
		return
	}
	procSetConsoleMode.Call(uintptr(r.in), uintptr(r.oldIn))
	procSetConsoleMode.Call(uintptr(r.out), uintptr(r.oldOut))
	r.in, r.out = 0, 0
}
