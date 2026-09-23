// linux.go Linux 实现（X11；Wayland 需 XWayland 支持）：鼠标/键盘用
// xdotool，截屏依次尝试 scrot / gnome-screenshot / import(ImageMagick) /
// grim(Wayland)，剪贴板用 xclip（回退 xsel）。显示器枚举优先 xrandr
// （多屏时给出各屏全局边界），无 xrandr 时按单屏处理。XTest 的鼠标坐标
// 相对所在屏，全局坐标经 moveMouse 换算。
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// xrandrRe 匹配 xrandr 输出的已连接行，如
// "eDP-1 connected primary 1920x1080+0+0 ..." / "HDMI-0 connected 2560x1440+1920+0 ..."。
var xrandrRe = regexp.MustCompile(`^(\S+) connected (?:primary )?(\d+)x(\d+)\+(-?\d+)\+(-?\d+)`)

// Displays 枚举显示器：xrandr 各输出的几何即全局（root window）坐标；
// primary 标记为主屏，都没有标记时取原点 (0,0) 的（再不行取第 0 块）。
func (l linuxPlatform) Displays(ctx context.Context) ([]Display, error) {
	out, err := run(ctx, "xrandr", "--current")
	if err != nil {
		// 无 xrandr：退回单屏（当前屏尺寸、原点 0,0）
		w, h, err2 := l.screenSizeLegacy(ctx)
		if err2 != nil {
			return nil, err2
		}
		return []Display{{Index: 0, X: 0, Y: 0, W: w, H: h, Main: true, Scale: 1}}, nil
	}
	var ds []Display
	for _, line := range strings.Split(out, "\n") {
		m := xrandrRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		w, _ := strconv.Atoi(m[2])
		h, _ := strconv.Atoi(m[3])
		x, _ := strconv.Atoi(m[4])
		y, _ := strconv.Atoi(m[5])
		ds = append(ds, Display{
			Index: len(ds), Name: m[1], X: x, Y: y, W: w, H: h,
			Main: strings.Contains(line, " primary "), Scale: 1,
		})
	}
	if len(ds) == 0 {
		return nil, fmt.Errorf("xrandr 未报告任何已连接的显示器")
	}
	hasMain := false
	for _, d := range ds {
		if d.Main {
			hasMain = true
			break
		}
	}
	if !hasMain {
		ds[0].Main = true
		if ds[0].X != 0 || ds[0].Y != 0 {
			for i := range ds {
				if ds[i].X == 0 && ds[i].Y == 0 {
					ds[0].Main = false
					ds[i].Main = true
					break
				}
			}
		}
	}
	return ds, nil
}

// screenSizeLegacy 旧路径的当前屏尺寸（xdotool getdisplaygeometry）。
func (l linuxPlatform) screenSizeLegacy(ctx context.Context) (int, int, error) {
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

// moveMouse 移动到全局坐标：XTest 坐标相对所在屏，先换算成屏内局部坐标并
// 用 --screen 指定屏（枚举失败时退回直接移动，单屏语义不变）。
func (l linuxPlatform) moveMouse(ctx context.Context, x, y int) error {
	ds, err := l.Displays(ctx)
	if err != nil || len(ds) == 0 {
		_, err := xdotool(ctx, "mousemove", "--sync", strconv.Itoa(x), strconv.Itoa(y))
		return err
	}
	i, lx, ly := locateDisplay(ds, x, y)
	_, err = xdotool(ctx, "mousemove", "--sync",
		"--screen", strconv.Itoa(ds[i].Index), strconv.Itoa(lx), strconv.Itoa(ly))
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
	if err := l.moveMouse(ctx, x, y); err != nil {
		return err
	}
	_, err := xdotool(ctx, "click", "--repeat", strconv.Itoa(clicks), strconv.Itoa(btn))
	return err
}

func (l linuxPlatform) Drag(ctx context.Context, fromX, fromY, toX, toY int) error {
	if err := l.moveMouse(ctx, fromX, fromY); err != nil {
		return err
	}
	if _, err := xdotool(ctx, "mousedown", "1"); err != nil {
		return err
	}
	steps := 25
	for i := 1; i <= steps; i++ {
		x := fromX + (toX-fromX)*i/steps
		y := fromY + (toY-fromY)*i/steps
		if err := l.moveMouse(ctx, x, y); err != nil {
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
		if err := l.moveMouse(ctx, x, y); err != nil {
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

func (l linuxPlatform) ScreenSize(ctx context.Context, display *int) (int, int, error) {
	ds, err := l.Displays(ctx)
	if err != nil {
		return 0, 0, err
	}
	d, err := pickDisplay(ds, display)
	if err != nil {
		return 0, 0, err
	}
	return d.W, d.H, nil
}

func (l linuxPlatform) Screenshot(ctx context.Context, display *int) ([]byte, error) {
	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("grf-computer-%d.png", time.Now().UnixNano()))
	defer os.Remove(tmp)
	if display == nil {
		// 省略编号=整桌面（root window / grim 全部输出）
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
	ds, err := l.Displays(ctx)
	if err != nil {
		return nil, err
	}
	d, err := pickDisplay(ds, display)
	if err != nil {
		return nil, err
	}
	// 按屏截：scrot 选区 / import 裁剪（负原点不支持，跳过）/ grim 指定输出
	for _, cand := range []struct {
		bin  string
		skip bool
		args []string
	}{
		{"scrot", false, []string{"-z", "-a", fmt.Sprintf("%d,%d,%d,%d", d.X, d.Y, d.W, d.H), tmp}},
		{"import", d.X < 0 || d.Y < 0, []string{"-window", "root", "-crop", fmt.Sprintf("%dx%d+%d+%d", d.W, d.H, d.X, d.Y), tmp}},
		{"grim", d.Name == "", []string{"-o", d.Name, tmp}},
	} {
		if cand.skip {
			continue
		}
		if _, err := exec.LookPath(cand.bin); err != nil {
			continue
		}
		if _, err := run(ctx, cand.bin, cand.args...); err == nil {
			return os.ReadFile(tmp)
		}
	}
	return nil, fmt.Errorf("按屏截屏失败：需要 scrot / ImageMagick(import) / grim 之一")
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
