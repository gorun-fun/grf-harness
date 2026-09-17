// platform.go 平台抽象与通用执行辅助：computer 工具的具体鼠标/键盘/
// 截屏实现按 GOOS 分文件（darwin/linux/windows/other），全部通过调用
// 操作系统自带命令行工具完成（osascript+screencapture / xdotool 等 /
// PowerShell），插件本身零 cgo、零第三方依赖。
package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// opTimeout 单次系统命令超时。
const opTimeout = 15 * time.Second

// Display 一块显示器的几何信息。Index 与工具 display 参数对应（0 起）；
// bounds 为全局虚拟桌面坐标系（原点=主屏左上，副屏排在左侧/上方时坐标为
// 负）；Scale 为截图像素与该坐标系的倍率（Retina=2，其余=1）。
type Display struct {
	Index int
	Name  string // 输出名（Linux xrandr / Windows 设备名，macOS 为空）
	X, Y  int
	W, H  int
	Main  bool
	Scale int
}

// platform 平台操作接口（各 GOOS 文件实现）。坐标均为全局虚拟桌面坐标；
// display 参数为 computer_list_displays 的显示器编号，nil 表示缺省行为
// （截图/尺寸=主屏）。
type platform interface {
	Displays(ctx context.Context) ([]Display, error)
	Screenshot(ctx context.Context, display *int) ([]byte, error) // PNG 字节
	ScreenSize(ctx context.Context, display *int) (w, h int, err error)
	Move(ctx context.Context, x, y int) error
	Click(ctx context.Context, x, y int, button string, clicks int) error
	Drag(ctx context.Context, fromX, fromY, toX, toY int) error
	// Scroll 滚动：hasPos 表示 x/y 是调用方显式给的坐标（含合法的 0），
	// false 则在当前位置滚动（三平台语义统一，见 tools.go 的调用侧）。
	Scroll(ctx context.Context, x, y int, hasPos bool, dir string, amount int) error
	Type(ctx context.Context, text string) error
	Key(ctx context.Context, combo string) error
	ClipboardGet(ctx context.Context) (string, error)
	ClipboardSet(ctx context.Context, text string) error
}

// newPlatform 按 GOOS 选实现（在 tools.go 注册期调用一次）。
func newPlatform() platform {
	switch runtime.GOOS {
	case "darwin":
		return darwinPlatform{}
	case "linux":
		return linuxPlatform{}
	case "windows":
		return windowsPlatform{}
	default:
		return unsupportedPlatform{}
	}
}

// pickDisplay 按编号选显示器；nil 取主屏（无主屏标记时取第 0 块）。
func pickDisplay(ds []Display, display *int) (Display, error) {
	if len(ds) == 0 {
		return Display{}, fmt.Errorf("未检测到显示器")
	}
	if display == nil {
		for _, d := range ds {
			if d.Main {
				return d, nil
			}
		}
		return ds[0], nil
	}
	for _, d := range ds {
		if d.Index == *display {
			return d, nil
		}
	}
	return Display{}, fmt.Errorf("显示器编号 %d 不存在（可用 computer_list_displays 查询）", *display)
}

// locateDisplay 找到包含全局坐标 (x,y) 的显示器，返回其 index 与屏内局部
// 坐标（X11 XTest 的坐标相对所在屏，需换算）；不在任何屏内时取中心最近的
// 屏并把坐标夹紧进屏内。
func locateDisplay(ds []Display, x, y int) (int, int, int) {
	best, bi := -1, 0
	for i, d := range ds {
		if x >= d.X && x < d.X+d.W && y >= d.Y && y < d.Y+d.H {
			return i, x - d.X, y - d.Y
		}
		cx, cy := d.X+d.W/2, d.Y+d.H/2
		dist := (x-cx)*(x-cx) + (y-cy)*(y-cy)
		if best < 0 || dist < best {
			best, bi = dist, i
		}
	}
	d := ds[bi]
	lx, ly := x-d.X, y-d.Y
	if lx < 0 {
		lx = 0
	} else if lx >= d.W {
		lx = d.W - 1
	}
	if ly < 0 {
		ly = 0
	} else if ly >= d.H {
		ly = d.H - 1
	}
	return bi, lx, ly
}

// pngSize 从 PNG 字节直接解 IHDR 宽高（避免为量尺寸引入图像解码依赖）。
func pngSize(data []byte) (int, int, bool) {
	// PNG 签名 8 字节 + IHDR 长度/类型 8 字节，宽高各 4 字节大端
	if len(data) < 24 || string(data[12:16]) != "IHDR" {
		return 0, 0, false
	}
	w := int(data[16])<<24 | int(data[17])<<16 | int(data[18])<<8 | int(data[19])
	h := int(data[20])<<24 | int(data[21])<<16 | int(data[22])<<8 | int(data[23])
	return w, h, w > 0 && h > 0
}

// run 执行系统命令（CombinedOutput，超时收敛为 error）。
func run(ctx context.Context, name string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	out := strings.TrimSpace(buf.String())
	if err != nil {
		return out, fmt.Errorf("%s: %w（输出: %s）", name, err, out)
	}
	return out, nil
}

// screenScaleHint 各平台给模型的可附加提示（如 macOS 需辅助功能授权）。
func permissionHint(err error, hint string) error {
	return fmt.Errorf("%w\n提示: %s", err, hint)
}

// comboMods 组合键修饰位（平台各自渲染成脚本片段）。
type comboMods struct {
	ctrl, alt, meta, shift bool
}

// parseCombo 解析 "ctrl+shift+t"：修饰键 + 末位键名（小写）。
func parseCombo(combo string) (comboMods, string, error) {
	tokens := strings.Split(strings.ToLower(strings.TrimSpace(combo)), "+")
	if len(tokens) == 0 || tokens[0] == "" {
		return comboMods{}, "", fmt.Errorf("按键为空")
	}
	var m comboMods
	for _, t := range tokens[:len(tokens)-1] {
		switch t {
		case "ctrl", "control":
			m.ctrl = true
		case "alt", "option":
			m.alt = true
		case "cmd", "meta", "command", "super", "win":
			m.meta = true
		case "shift":
			m.shift = true
		default:
			return m, "", fmt.Errorf("未知修饰键 %q", t)
		}
	}
	return m, tokens[len(tokens)-1], nil
}

// macScript macOS AppleScript 的 using {…} 片段。
func (m comboMods) macScript() string {
	var parts []string
	if m.meta {
		parts = append(parts, "command down")
	}
	if m.ctrl {
		parts = append(parts, "control down")
	}
	if m.alt {
		parts = append(parts, "option down")
	}
	if m.shift {
		parts = append(parts, "shift down")
	}
	if len(parts) == 0 {
		return ""
	}
	return " using {" + strings.Join(parts, ", ") + "}"
}

// xdotoolCombo Linux xdotool 的组合键串（如 ctrl+shift+t）。
func (m comboMods) xdotoolCombo(key string) string {
	var parts []string
	if m.ctrl {
		parts = append(parts, "ctrl")
	}
	if m.alt {
		parts = append(parts, "alt")
	}
	if m.shift {
		parts = append(parts, "shift")
	}
	if m.meta {
		parts = append(parts, "super")
	}
	return strings.Join(append(parts, key), "+")
}
