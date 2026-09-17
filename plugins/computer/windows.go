// windows.go Windows 实现：截屏/鼠标/键盘/剪贴板全部经 PowerShell
// （System.Drawing CopyFromScreen、user32 P/Invoke、SendKeys、
// Get/Set-Clipboard），无 cgo。
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type windowsPlatform struct{}

func ps(ctx context.Context, script string) (string, error) {
	return run(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)
}

// user32PS 鼠标操作脚本前缀（每次注入 P/Invoke 定义，简单可靠）。
const user32PS = `Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
public class GrfU {
  [DllImport("user32.dll")] public static extern bool SetCursorPos(int x, int y);
  [DllImport("user32.dll")] public static extern void mouse_event(uint f, uint dx, uint dy, int d, UIntPtr e);
}
'@;
`

// mouseEventPS 一次 mouse_event 调用（flags: LEFTDOWN=2 LEFTUP=4
// RIGHTDOWN=8 RIGHTUP=16 MIDDLEDOWN=32 MIDDLEUP=64 WHEEL=0x0800）。
func mouseEventPS(flag string) string {
	return fmt.Sprintf("[GrfU]::mouse_event(%s,0,0,0,[UIntPtr]::Zero);", flag)
}

func (w windowsPlatform) Move(ctx context.Context, x, y int) error {
	_, err := ps(ctx, fmt.Sprintf(user32PS+"[GrfU]::SetCursorPos(%d,%d);", x, y))
	return err
}

func (w windowsPlatform) Click(ctx context.Context, x, y int, button string, clicks int) error {
	btn, ok := map[string]struct{ d, u string }{
		"left": {"2", "4"}, "": {"2", "4"}, "right": {"8", "16"}, "middle": {"32", "64"},
	}[button]
	if !ok {
		return fmt.Errorf("未知按键 %q（left/middle/right）", button)
	}
	if clicks < 1 {
		clicks = 1
	}
	var b strings.Builder
	fmt.Fprintf(&b, user32PS+"[GrfU]::SetCursorPos(%d,%d);Start-Sleep -Milliseconds 60;", x, y)
	for i := 0; i < clicks; i++ {
		b.WriteString(mouseEventPS(btn.d) + mouseEventPS(btn.u) + "Start-Sleep -Milliseconds 60;")
	}
	_, err := ps(ctx, b.String())
	return err
}

func (w windowsPlatform) Drag(ctx context.Context, fromX, fromY, toX, toY int) error {
	var b strings.Builder
	fmt.Fprintf(&b, user32PS+"[GrfU]::SetCursorPos(%d,%d);Start-Sleep -Milliseconds 80;", fromX, fromY)
	b.WriteString(mouseEventPS("2"))
	steps := 25
	for i := 1; i <= steps; i++ {
		x := fromX + (toX-fromX)*i/steps
		y := fromY + (toY-fromY)*i/steps
		fmt.Fprintf(&b, "[GrfU]::SetCursorPos(%d,%d);Start-Sleep -Milliseconds 12;", x, y)
	}
	b.WriteString(mouseEventPS("4"))
	_, err := ps(ctx, b.String())
	return err
}

func (w windowsPlatform) Scroll(ctx context.Context, x, y int, hasPos bool, dir string, amount int) error {
	if amount < 1 {
		amount = 1 // 先夹紧再算 delta，避免 amount=0 时静默无滚动
	}
	delta := amount * 120
	if dir == "down" {
		delta = -delta
	} else if dir != "up" {
		return fmt.Errorf("未知滚动方向 %q（up/down）", dir)
	}
	var b strings.Builder
	b.WriteString(user32PS)
	if hasPos {
		fmt.Fprintf(&b, "[GrfU]::SetCursorPos(%d,%d);", x, y)
	}
	fmt.Fprintf(&b, "[GrfU]::mouse_event(0x0800,0,0,%d,[UIntPtr]::Zero);", delta)
	_, err := ps(ctx, b.String())
	return err
}

// psSingleQuote PowerShell 单引号字符串字面量（内部 ' 加倍转义）。
// Go 的 %q 产出 \"、\n、\uXXXX 这类 Go 转义，PowerShell 双引号串不认，
// 不能用来把文本拼进脚本。
func psSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// sendKeysEsc 转义 SendKeys 特殊字符：+^%~(){}。
func sendKeysEsc(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune("+^%~(){}", r) {
			b.WriteByte('{')
			b.WriteRune(r)
			b.WriteByte('}')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (w windowsPlatform) Type(ctx context.Context, text string) error {
	_, err := ps(ctx, "Add-Type -AssemblyName System.Windows.Forms;[System.Windows.Forms.SendKeys]::SendWait("+psSingleQuote(sendKeysEsc(text))+");")
	return err
}

// sendKeysKey 具名键 → SendKeys 序列。
var sendKeysKey = map[string]string{
	"enter": "{ENTER}", "return": "{ENTER}", "tab": "{TAB}", "escape": "{ESC}", "esc": "{ESC}",
	"backspace": "{BS}", "delete": "{DEL}", "space": " ",
	"up": "{UP}", "down": "{DOWN}", "left": "{LEFT}", "right": "{RIGHT}",
	"pageup": "{PGUP}", "pagedown": "{PGDN}", "home": "{HOME}", "end": "{END}",
}

func (w windowsPlatform) Key(ctx context.Context, combo string) error {
	mods, key, err := parseCombo(combo)
	if err != nil {
		return err
	}
	if mods.meta {
		return fmt.Errorf("Windows 下 SendKeys 不支持 Win 键组合（%q）", combo)
	}
	mapped, ok := sendKeysKey[key]
	if !ok {
		if len([]rune(key)) != 1 {
			return fmt.Errorf("未知按键 %q", key)
		}
		mapped = key
	}
	// SendKeys 修饰前缀：ctrl=^ alt=% shift=+
	seq := ""
	if mods.ctrl {
		seq += "^"
	}
	if mods.alt {
		seq += "%"
	}
	if mods.shift {
		seq += "+"
	}
	_, err = ps(ctx, "Add-Type -AssemblyName System.Windows.Forms;[System.Windows.Forms.SendKeys]::SendWait("+psSingleQuote(seq+mapped)+");")
	return err
}

func (w windowsPlatform) ScreenSize(ctx context.Context, display *int) (int, int, error) {
	ds, err := w.Displays(ctx)
	if err != nil {
		return 0, 0, err
	}
	d, err := pickDisplay(ds, display)
	if err != nil {
		return 0, 0, err
	}
	return d.W, d.H, nil
}

// Displays 枚举显示器（AllScreens，bounds 即虚拟桌面像素坐标，Windows
// 鼠标坐标同坐标系，Scale=1）。
func (w windowsPlatform) Displays(ctx context.Context) ([]Display, error) {
	out, err := ps(ctx, `Add-Type -AssemblyName System.Windows.Forms;
[System.Windows.Forms.Screen]::AllScreens | ForEach-Object {
  $b = $_.Bounds
  "$([int]$_.Primary) $($b.X) $($b.Y) $($b.Width) $($b.Height) $($_.DeviceName)"
}`)
	if err != nil {
		return nil, err
	}
	var ds []Display
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var primary int
		var x, y, wid, ht int
		var name string
		if _, err := fmt.Sscanf(strings.TrimSpace(line), "%d %d %d %d %d %s",
			&primary, &x, &y, &wid, &ht, &name); err != nil {
			return nil, fmt.Errorf("解析显示器列表 %q: %w", line, err)
		}
		ds = append(ds, Display{
			Index: len(ds), Name: name, X: x, Y: y, W: wid, H: ht,
			Main: primary == 1, Scale: 1,
		})
	}
	if len(ds) == 0 {
		return nil, fmt.Errorf("未检测到显示器")
	}
	return ds, nil
}

func (w windowsPlatform) Screenshot(ctx context.Context, display *int) ([]byte, error) {
	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("grf-computer-%d.png", time.Now().UnixNano()))
	defer os.Remove(tmp)
	// 省略编号=整桌面（VirtualScreen 含副屏，可为负原点）；指定编号=该屏 bounds
	rect := "$b=[System.Windows.Forms.SystemInformation]::VirtualScreen;"
	if display != nil {
		ds, err := w.Displays(ctx)
		if err != nil {
			return nil, err
		}
		d, err := pickDisplay(ds, display)
		if err != nil {
			return nil, err
		}
		rect = fmt.Sprintf("$b=New-Object System.Drawing.Rectangle(%d,%d,%d,%d);", d.X, d.Y, d.W, d.H)
	}
	script := fmt.Sprintf(`Add-Type -AssemblyName System.Windows.Forms,System.Drawing;
`+rect+`
$bmp=New-Object System.Drawing.Bitmap($b.Width,$b.Height);
$g=[System.Drawing.Graphics]::FromImage($bmp);
$g.CopyFromScreen($b.Location,[System.Drawing.Point]::Empty,$b.Size);
$bmp.Save('%s',[System.Drawing.Imaging.ImageFormat]::Png);`, strings.ReplaceAll(tmp, "'", "''"))
	if _, err := ps(ctx, script); err != nil {
		return nil, err
	}
	return os.ReadFile(tmp)
}

func (w windowsPlatform) ClipboardGet(ctx context.Context) (string, error) {
	return ps(ctx, "Get-Clipboard")
}

func (w windowsPlatform) ClipboardSet(ctx context.Context, text string) error {
	_, err := ps(ctx, "Set-Clipboard -Value "+psSingleQuote(text))
	return err
}
