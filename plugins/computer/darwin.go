// darwin.go macOS 实现：鼠标/滚轮用 osascript JXA 调 CoreGraphics
// CGEvent（常量直接用数值，规避 ObjC bridge 枚举暴露差异），键盘用
// AppleScript System Events，截屏用 screencapture，剪贴板用 pbcopy/pbpaste。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type darwinPlatform struct{}

// mouseButton 按钮名 → (down/up 事件类型, CG 按钮 id, 是否合法)。
func mouseButton(name string) (down, up, id int, ok bool) {
	switch name {
	case "left", "":
		return 1, 2, 0, true
	case "right":
		return 3, 4, 1, true
	case "middle":
		return 25, 26, 2, true
	}
	return 0, 0, 0, false
}

// CGEvent 数值常量：事件类型 LeftMouseDown=1 LeftMouseUp=2 RightMouseDown=3
// RightMouseUp=4 MouseMoved=5 LeftMouseDragged=6 OtherMouseDown=25
// OtherMouseUp=26 OtherMouseDragged=27；鼠标键 Left=0 Right=1 Center=2；
// kCGMouseEventClickState=1；kCGScrollEventUnitLine=1（正 delta 向上）。
const jxaHeader = `ObjC.import('CoreGraphics');
ObjC.import('Foundation');
function postMouse(type, x, y, btn, clicks) {
  const e = $.CGEventCreateMouseEvent($(), type, $.CGPointMake(x, y), btn);
  $.CGEventSetIntegerValueField(e, 1, clicks);
  $.CGEventPost(0, e);
}
function sleep(sec) { $.NSThread.sleepForTimeInterval(sec); }
`

func jxa(ctx context.Context, code string) (string, error) {
	return run(ctx, "osascript", "-l", "JavaScript", "-e", code)
}

// wrap 把系统命令错误补上 macOS 授权提示（辅助功能/屏幕录制）。
func (d darwinPlatform) wrap(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, "assistive") || strings.Contains(msg, "25211") ||
		strings.Contains(msg, "not allowed") || strings.Contains(msg, " UniversalAccess") {
		return permissionHint(err, "macOS 需辅助功能授权：系统设置 → 隐私与安全性 → 辅助功能，勾选运行 gh 的终端应用；截屏还需\"屏幕录制\"授权")
	}
	return err
}

func (d darwinPlatform) Move(ctx context.Context, x, y int) error {
	_, err := jxa(ctx, fmt.Sprintf(jxaHeader+"postMouse(5, %d, %d, 0, 1);", x, y))
	return d.wrap(err)
}

func (d darwinPlatform) Click(ctx context.Context, x, y int, button string, clicks int) error {
	down, up, btn, ok := mouseButton(button)
	if !ok {
		return fmt.Errorf("未知按键 %q（left/right/middle）", button)
	}
	if clicks < 1 {
		clicks = 1
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf(jxaHeader+"postMouse(5, %d, %d, %d, 1);\n", x, y, btn))
	for i := 1; i <= clicks; i++ {
		fmt.Fprintf(&b, "postMouse(%d, %d, %d, %d, %d);\n", down, x, y, btn, i)
		fmt.Fprintf(&b, "postMouse(%d, %d, %d, %d, %d);\n", up, x, y, btn, i)
	}
	_, err := jxa(ctx, b.String())
	return d.wrap(err)
}

func (d darwinPlatform) Drag(ctx context.Context, fromX, fromY, toX, toY int) error {
	steps := 25
	var b strings.Builder
	b.WriteString(fmt.Sprintf(jxaHeader+"postMouse(5, %d, %d, 0, 1);\n", fromX, fromY))
	b.WriteString(fmt.Sprintf("postMouse(1, %d, %d, 0, 1);\n", fromX, fromY))
	for i := 1; i <= steps; i++ {
		x := fromX + (toX-fromX)*i/steps
		y := fromY + (toY-fromY)*i/steps
		b.WriteString("sleep(0.005);\n")
		fmt.Fprintf(&b, "postMouse(6, %d, %d, 0, 1);\n", x, y)
	}
	b.WriteString(fmt.Sprintf("postMouse(2, %d, %d, 0, 1);\n", toX, toY))
	_, err := jxa(ctx, b.String())
	return d.wrap(err)
}

func (d darwinPlatform) Scroll(ctx context.Context, x, y int, hasPos bool, dir string, amount int) error {
	if amount < 1 {
		amount = 1
	}
	delta := amount
	if dir == "down" {
		delta = -amount
	} else if dir != "up" {
		return fmt.Errorf("未知滚动方向 %q（up/down）", dir)
	}
	var b strings.Builder
	if hasPos {
		fmt.Fprintf(&b, jxaHeader+"postMouse(5, %d, %d, 0, 1);\n", x, y)
	} else {
		b.WriteString(jxaHeader)
	}
	fmt.Fprintf(&b, "const e = $.CGEventCreateScrollWheelEvent($(), 1, 1, %d);\n$.CGEventPost(0, e);", delta)
	_, err := jxa(ctx, b.String())
	return d.wrap(err)
}

func (d darwinPlatform) Type(ctx context.Context, text string) error {
	// 纯 ASCII 走 keystroke；含非 ASCII（如中文）时 IME 会拦截按键把内容打乱
	// （拉丁键进候选、空格变选词提交），改走剪贴板粘贴，事后恢复原剪贴板。
	if isASCII(text) {
		script := "on run argv\ntell application \"System Events\" to keystroke (item 1 of argv)\nend run"
		_, err := run(ctx, "osascript", "-e", script, text)
		return d.wrap(err)
	}
	old, err := d.ClipboardGet(ctx)
	if err != nil {
		return err
	}
	if err := d.ClipboardSet(ctx, text); err != nil {
		return err
	}
	time.Sleep(100 * time.Millisecond) // 等 pbcopy 落盘到粘贴板，避免粘到旧内容
	if err := d.Key(ctx, "cmd+v"); err != nil {
		_ = d.ClipboardSet(ctx, old)
		return err
	}
	time.Sleep(100 * time.Millisecond) // 等焦点窗口取走粘贴板内容再恢复
	return d.ClipboardSet(ctx, old)
}

// isASCII 判断是否全部为 ASCII 字符（含空串）。
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// darwinKeyCodes AppleScript key code 表。
var darwinKeyCodes = map[string]int{
	"enter": 36, "return": 36, "tab": 48, "escape": 53, "esc": 53,
	"backspace": 51, "delete": 51, "forwarddelete": 117, "space": 49,
	"up": 126, "down": 125, "left": 123, "right": 124,
	"pageup": 116, "pagedown": 121, "home": 115, "end": 119,
	"f1": 122, "f2": 120, "f3": 99, "f4": 118, "f5": 96, "f6": 97,
	"f7": 98, "f8": 100, "f9": 101, "f10": 109, "f11": 103, "f12": 111,
}

func (d darwinPlatform) Key(ctx context.Context, combo string) error {
	mods, key, err := parseCombo(combo)
	if err != nil {
		return err
	}
	modScript := mods.macScript()
	if code, ok := darwinKeyCodes[key]; ok {
		script := fmt.Sprintf("tell application \"System Events\"\nkey code %d%s\nend tell", code, modScript)
		_, err := run(ctx, "osascript", "-e", script)
		return d.wrap(err)
	}
	if len([]rune(key)) == 1 {
		script := "on run argv\ntell application \"System Events\"\nkeystroke (item 1 of argv)" + modScript + "\nend tell\nend run"
		_, err := run(ctx, "osascript", "-e", script, key)
		return d.wrap(err)
	}
	return fmt.Errorf("未知按键 %q", key)
}

func (d darwinPlatform) ScreenSize(ctx context.Context) (int, int, error) {
	out, err := jxa(ctx, "ObjC.import('CoreGraphics');\nconst b = $.CGDisplayBounds($.CGMainDisplayID());\nJSON.stringify({w: b.size.width, h: b.size.height});")
	if err != nil {
		return 0, 0, d.wrap(err)
	}
	var dim struct {
		W, H float64
	}
	if err := parseJSONLoose(out, &dim); err != nil {
		return 0, 0, fmt.Errorf("解析屏幕尺寸 %q: %w", out, err)
	}
	return int(dim.W), int(dim.H), nil
}

func (d darwinPlatform) Screenshot(ctx context.Context) ([]byte, error) {
	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("grf-computer-%d.png", time.Now().UnixNano()))
	defer os.Remove(tmp)
	if _, err := run(ctx, "screencapture", "-x", "-t", "png", tmp); err != nil {
		return nil, d.wrap(err)
	}
	data, err := os.ReadFile(tmp)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (d darwinPlatform) ClipboardGet(ctx context.Context) (string, error) {
	out, err := run(ctx, "pbpaste")
	return out, d.wrap(err)
}

func (d darwinPlatform) ClipboardSet(ctx context.Context, text string) error {
	cctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "pbcopy")
	cmd.Stdin = strings.NewReader(text)
	if err := cmd.Run(); err != nil {
		return d.wrap(err)
	}
	return nil
}

// parseJSONLoose 解析 JSON。
func parseJSONLoose(s string, v any) error { return json.Unmarshal([]byte(s), v) }
