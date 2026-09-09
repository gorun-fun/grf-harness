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

// platform 平台操作接口（各 GOOS 文件实现）。
type platform interface {
	Screenshot(ctx context.Context) ([]byte, error) // PNG 字节
	ScreenSize(ctx context.Context) (w, h int, err error)
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
