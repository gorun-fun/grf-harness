// linux.go Linux 实现（X11；Wayland 需 XWayland 支持）：鼠标/键盘用
// xdotool，截屏依次尝试 scrot / gnome-screenshot / import(ImageMagick) /
// grim(Wayland)，剪贴板用 xclip（回退 xsel）。
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type linuxPlatform struct{}

// xdotoolKey 具名键 → X keysym。
var xdotoolKey = map[string]string{
	"enter": "Return", "return": "Return", "tab": "Tab", "escape": "Escape", "esc": "Escape",
	"backspace": "BackSpace", "delete": "Delete", "space": "space",
	"up": "Up", "down": "Down", "left": "Left", "right": "Right",
	"arrowup": "Up", "arrowdown": "Down", "arrowleft": "Left", "arrowright": "Right",
	"pageup": "Page_Up", "pagedown": "Page_Down", "home": "Home", "end": "End",
	"f1": "F1", "f2": "F2", "f3": "F3", "f4": "F4", "f5": "F5", "f6": "F6",
	"f7": "F7", "f8": "F8", "f9": "F9", "f10": "F10", "f11": "F11", "f12": "F12",
}

func xdotool(ctx context.Context, args ...string) (string, error) {
	return run(ctx, "xdotool", args...)
}

func (l linuxPlatform) Move(ctx context.Context, x, y int) error {
	_, err := xdotool(ctx, "mousemove", strconv.Itoa(x), strconv.Itoa(y))
	return err
}

func (l linuxPlatform) Click(ctx context.Context, x, y int, button string, clicks int) error {
	btn, ok := map[string]int{"left": 1, "": 1, "middle": 2, "right": 3}[button]
	if !ok {
		return fmt.Errorf("未知按键 %q（left/middle/right）", button)
	}
	if clicks < 1 {
		clicks = 1
	}
	if x >= 0 && y >= 0 {
		if _, err := xdotool(ctx, "mousemove", strconv.Itoa(x), strconv.Itoa(y)); err != nil {
			return err
		}
	}
	_, err := xdotool(ctx, "click", "--repeat", strconv.Itoa(clicks), strconv.Itoa(btn))
	return err
}

func (l linuxPlatform) Drag(ctx context.Context, fromX, fromY, toX, toY int) error {
	if _, err := xdotool(ctx, "mousemove", strconv.Itoa(fromX), strconv.Itoa(fromY)); err != nil {
		return err
	}
	if _, err := xdotool(ctx, "mousedown", "1"); err != nil {
		return err
	}
	steps := 25
	for i := 1; i <= steps; i++ {
		x := fromX + (toX-fromX)*i/steps
		y := fromY + (toY-fromY)*i/steps
		if _, err := xdotool(ctx, "mousemove", "--sync", strconv.Itoa(x), strconv.Itoa(y)); err != nil {
			return err
		}
	}
	_, err := xdotool(ctx, "mouseup", "1")
	return err
}

func (l linuxPlatform) Scroll(ctx context.Context, x, y int, hasPos bool, dir string, amount int) error {
	var btn string
	switch dir {
	case "up":
		btn = "4"
	case "down":
		btn = "5"
	default:
		return fmt.Errorf("未知滚动方向 %q（up/down）", dir)
	}
	if amount < 1 {
		amount = 1
	}
	if hasPos {
		if _, err := xdotool(ctx, "mousemove", strconv.Itoa(x), strconv.Itoa(y)); err != nil {
			return err
		}
	}
	_, err := xdotool(ctx, "click", "--repeat", strconv.Itoa(amount), btn)
	return err
}

func (l linuxPlatform) Type(ctx context.Context, text string) error {
	_, err := xdotool(ctx, "type", "--delay", "15", "--", text)
	return err
}

func (l linuxPlatform) Key(ctx context.Context, combo string) error {
	mods, key, err := parseCombo(combo)
	if err != nil {
		return err
	}
	if mapped, ok := xdotoolKey[key]; ok {
		key = mapped
	} else if len([]rune(key)) != 1 {
		return fmt.Errorf("未知按键 %q", key)
	}
	_, err = xdotool(ctx, "key", "--", mods.xdotoolCombo(key))
	return err
}

func (l linuxPlatform) ScreenSize(ctx context.Context) (int, int, error) {
	out, err := xdotool(ctx, "getdisplaygeometry")
	if err != nil {
		return 0, 0, fmt.Errorf("需要 X11 与 xdotool（Wayland 请经 XWayland）: %w", err)
	}
	parts := strings.Fields(out)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("getdisplaygeometry 输出异常: %q", out)
	}
	w, e1 := strconv.Atoi(parts[0])
	h, e2 := strconv.Atoi(parts[1])
	if e1 != nil || e2 != nil {
		return 0, 0, fmt.Errorf("getdisplaygeometry 输出异常: %q", out)
	}
	return w, h, nil
}

func (l linuxPlatform) Screenshot(ctx context.Context) ([]byte, error) {
	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("grf-computer-%d.png", time.Now().UnixNano()))
	defer os.Remove(tmp)
	for _, cand := range []struct {
		bin  string
		args []string
	}{
		{"scrot", []string{"-z", tmp}},
		{"gnome-screenshot", []string{"-f", tmp}},
		{"import", []string{"-window", "root", tmp}},
		{"grim", []string{tmp}},
	} {
		if _, err := exec.LookPath(cand.bin); err != nil {
			continue
		}
		if _, err := run(ctx, cand.bin, cand.args...); err == nil {
			return os.ReadFile(tmp)
		}
	}
	return nil, fmt.Errorf("截屏失败：需要 scrot / gnome-screenshot / ImageMagick(import) / grim 之一")
}

func (l linuxPlatform) ClipboardGet(ctx context.Context) (string, error) {
	if _, err := exec.LookPath("xclip"); err == nil {
		return run(ctx, "xclip", "-selection", "clipboard", "-o")
	}
	return run(ctx, "xsel", "-b", "-o")
}

func (l linuxPlatform) ClipboardSet(ctx context.Context, text string) error {
	cctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	var cmd *exec.Cmd
	if _, err := exec.LookPath("xclip"); err == nil {
		cmd = exec.CommandContext(cctx, "xclip", "-selection", "clipboard", "-i")
	} else if _, err := exec.LookPath("xsel"); err == nil {
		cmd = exec.CommandContext(cctx, "xsel", "-b", "-i")
	} else {
		return fmt.Errorf("剪贴板需要 xclip 或 xsel")
	}
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run()
}
