// Package term 提供零依赖的终端底层能力：TTY 判定与尺寸查询、raw 模式、
// 输入事件解析（按键/粘贴/鼠标）、ANSI 感知的宽度与软换行、备用屏幕
// 全屏渲染（含 SIGWINCH 尺寸变化重绘）。仅使用 Go 标准库。
package term

// Key 按键事件的规范化名称。
const (
	KeyUp        = "up"
	KeyDown      = "down"
	KeyLeft      = "left"
	KeyRight     = "right"
	KeyEnter     = "enter"
	KeyEsc       = "esc"
	KeyTab       = "tab"
	KeyBackspace = "backspace"
	KeyDelete    = "delete"
	KeyHome      = "home"
	KeyEnd       = "end"
	KeyPgUp      = "pgup"
	KeyPgDn      = "pgdn"
	KeySpace     = "space"
)

// CtrlName 返回控制字节对应的按键名（如 ctrl+c）；无映射时返回空。
func CtrlName(b byte) string {
	switch b {
	case 0x01:
		return "ctrl+a"
	case 0x02:
		return "ctrl+b"
	case 0x03:
		return "ctrl+c"
	case 0x04:
		return "ctrl+d"
	case 0x05:
		return "ctrl+e"
	case 0x06:
		return "ctrl+f"
	case 0x07:
		return "ctrl+g"
	case 0x08:
		return "ctrl+h"
	case 0x09:
		return "tab"
	case 0x0a:
		return "ctrl+j"
	case 0x0b:
		return "ctrl+k"
	case 0x0c:
		return "ctrl+l"
	case 0x0d:
		return "enter"
	case 0x0e:
		return "ctrl+n"
	case 0x0f:
		return "ctrl+o"
	case 0x10:
		return "ctrl+p"
	case 0x11:
		return "ctrl+q"
	case 0x12:
		return "ctrl+r"
	case 0x13:
		return "ctrl+s"
	case 0x14:
		return "ctrl+t"
	case 0x15:
		return "ctrl+u"
	case 0x16:
		return "ctrl+v"
	case 0x17:
		return "ctrl+w"
	case 0x18:
		return "ctrl+x"
	case 0x19:
		return "ctrl+y"
	case 0x1a:
		return "ctrl+z"
	case 0x00:
		return "ctrl+@"
	case 0x1c:
		return "ctrl+\\"
	case 0x1d:
		return "ctrl+]"
	case 0x1e:
		return "ctrl+^"
	case 0x1f:
		return "ctrl+_"
	case 0x7f:
		return "backspace"
	}
	return ""
}
