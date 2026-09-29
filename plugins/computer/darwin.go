// darwin.go macOS 实现：鼠标/滚轮用 osascript JXA 调 CoreGraphics
// CGEvent（常量直接用数值，规避 ObjC bridge 枚举暴露差异），键盘用
// AppleScript System Events，截屏用 screencapture，剪贴板用 pbcopy/pbpaste。
package main

import (
	"bytes"
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
// kCGMouseEventClickState=1；kCGScrollEventUnitPixel=0（正 delta 向上）。
const jxaHeader = `ObjC.import('CoreGraphics');
ObjC.import('Foundation');
// 事件源必须用 kCGEventSourceStateCombinedSessionState(0)：实测用
// kCGEventSourceStateHIDSystem(1) 时合成的 mouse-up 不回写按钮状态——
// CGEventSourceButtonState 点击后仍报按住，后续一切指针移动都被系统判成
// 按住拖动（"点击变拖动"的根因）。Combined 源合成 down/up 状态正确收放。
const msrc = $.CGEventSourceCreate(0);
function postMouse(type, x, y, btn, clicks) {
  const e = $.CGEventCreateMouseEvent(msrc, type, $.CGPointMake(x, y), btn);
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
	// down/up 之间与连击对之间必须有间隔：零间隔连发的事件批会被部分应用
	// （Electron/Qt/Finder）把批间的指针位移判成按住拖动——"点击变拖动"
	// 的根因；双击/三击还需落在系统双击判定窗口（<500ms）内
	b.WriteString("sleep(0.03);\n")
	for i := 1; i <= clicks; i++ {
		fmt.Fprintf(&b, "postMouse(%d, %d, %d, %d, %d);\n", down, x, y, btn, i)
		b.WriteString("sleep(0.05);\n")
		fmt.Fprintf(&b, "postMouse(%d, %d, %d, %d, %d);\n", up, x, y, btn, i)
		if i < clicks {
			b.WriteString("sleep(0.08);\n")
		}
	}
	// 兜底再补一次 mouse-up（同状态重复 up 幂等）：确保任何应用视角下按键
	// 已释放，后续指针移动不会被当成拖动延续
	fmt.Fprintf(&b, "sleep(0.02);\npostMouse(%d, %d, %d, %d, 1);\n", up, x, y, btn)
	_, err := jxa(ctx, b.String())
	return d.wrap(err)
}

func (d darwinPlatform) Drag(ctx context.Context, fromX, fromY, toX, toY int) error {
	steps := 25
	var b strings.Builder
	b.WriteString(fmt.Sprintf(jxaHeader+"postMouse(5, %d, %d, 0, 1);\n", fromX, fromY))
	b.WriteString("sleep(0.03);\n")
	b.WriteString(fmt.Sprintf("postMouse(1, %d, %d, 0, 1);\n", fromX, fromY))
	for i := 1; i <= steps; i++ {
		x := fromX + (toX-fromX)*i/steps
		y := fromY + (toY-fromY)*i/steps
		b.WriteString("sleep(0.005);\n")
		fmt.Fprintf(&b, "postMouse(6, %d, %d, 0, 1);\n", x, y)
	}
	// 到达终点稍作停留再松开：立即 up 时部分应用仍按上一插值点结算拖放
	b.WriteString("sleep(0.05);\n")
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
	// kCGScrollEventUnitLine(1) 的合成滚轮事件在 macOS 新系统（SwiftUI 应用
	// 如天气面板、备忘录）会被直接忽略（实测无效果），改用像素单位
	// kCGScrollEventUnitPixel(0)：1"行"按 40px 折算（≈标准滚轮一格 3 行），
	// 且必须拆成多个带 20ms 间隔的连续事件——单个像素滚动事件同样会被忽略。
	pixels := delta * 40
	events := amount
	if events < 3 {
		events = 3
	}
	per := pixels / events
	if per == 0 {
		per = pixels
		events = 1
	}
	fmt.Fprintf(&b, "for (let i = 0; i < %d; i++) { const e = $.CGEventCreateScrollWheelEvent($(), 0, 1, %d);\n$.CGEventPost(0, e); $.NSThread.sleepForTimeInterval(0.02); }", events, per)
	_, err := jxa(ctx, b.String())
	return d.wrap(err)
}

func (d darwinPlatform) Type(ctx context.Context, text string) error {
	// 输入源是拼音等 IME 时，合成键击会被输入法拦截重组——ASCII 也逃不掉
	// （实测 "echo D2_OK" 被搜狗转成中文、enter 变成空提交）。检测到 IME
	// 输入源即整体改走剪贴板粘贴（IME 不拦粘贴），事后恢复原剪贴板；
	// 非 IME 输入源的纯 ASCII 仍走 keystroke（更快且不动剪贴板）。
	// 含非 ASCII（如中文）时同样必须粘贴：IME 会把拉丁键进候选、空格变选词。
	if isASCII(text) && !d.imeActive(ctx) {
		// 分块键入：单次 keystroke 打长串，应用事件队列忙时会出现丢字/错序
		// （实测 "echo" 落成 "ec ho"）。按 30 字符一块、块间 40ms 间隔放慢
		// 节奏；收尾再停 150ms 让应用排空队列——紧随其后的 computer_key
		// enter 才不会抢在字符前面。
		script := `on run argv
	set t to item 1 of argv
	set n to count of t
	set i to 1
	repeat while i < n + 1
		set j to i + 29
		if j > n then set j to n
		tell application "System Events" to keystroke (text i thru j of t)
		delay 0.04
		set i to j + 1
	end repeat
	delay 0.15
end run`
		_, err := run(ctx, "osascript", "-e", script, text)
		return d.wrap(err)
	}
	old, err := pbpasteRaw(ctx)
	if err != nil {
		return err
	}
	// ClipboardSet 自带落板确认（轮询 pbpaste 与目标一致），返回即可粘贴
	if err := d.ClipboardSet(ctx, text); err != nil {
		return err
	}
	if err := d.Key(ctx, "cmd+v"); err != nil {
		_ = d.ClipboardSet(ctx, old)
		return err
	}
	// 恢复剪贴板前多等一会：焦点应用处理 cmd+v 有快有慢（浏览器/Electron
	// 常超 300ms），过早恢复会让目标粘到旧内容——那正是"输入不起作用"的
	// 常见表现
	time.Sleep(500 * time.Millisecond)
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

// imeActive 当前输入源是否为 IME（拼音/注音等组合输入法）。IME 输入源的
// bundle id 形如 com.sogou.inputmethod.sogou.pinyin / com.apple.inputmethod.
// SCIM.ITABC（含 ".inputmethod"）。读 HIToolbox 的当前输入源 id，失败按
// 非 IME 处理（退回键击路径，不比旧行为差）。注意：输入法状态按应用记忆，
// 此键未必实时反映目标应用的输入源——尽力检测。
func (d darwinPlatform) imeActive(ctx context.Context) bool {
	out, err := runT(ctx, 3*time.Second, "defaults", "read",
		"com.apple.HIToolbox", "AppleCurrentKeyboardLayoutInputSourceID")
	if err != nil {
		return false
	}
	return looksLikeIME(out)
}

// looksLikeIME 输入源 id 是否 IME 形态（纯字符串判定，可单测）。
func looksLikeIME(sourceID string) bool {
	return strings.Contains(sourceID, ".inputmethod")
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

func (d darwinPlatform) ScreenSize(ctx context.Context, display *int) (int, int, error) {
	ds, err := d.Displays(ctx)
	if err != nil {
		return 0, 0, err
	}
	disp, err := pickDisplay(ds, display)
	if err != nil {
		return 0, 0, err
	}
	return disp.W, disp.H, nil
}

// displaysJXA 枚举显示器：NSScreen 的 frame 是 AppKit 坐标（原点=主屏左下、
// y 向上），这里翻转为全局显示坐标（原点=主屏左上、y 向下，与 CGEvent/
// screencapture -R 一致，单位为 point）。
const displaysJXA = `ObjC.import('AppKit');
const screens = $.NSScreen.screens;
let ph = 0;
for (let i = 0; i < screens.count; i++) {
  const f = screens.objectAtIndex(i).frame;
  if (f.origin.x === 0 && f.origin.y === 0) { ph = f.size.height; }
}
const res = [];
for (let i = 0; i < screens.count; i++) {
  const s = screens.objectAtIndex(i);
  const f = s.frame;
  res.push({x: f.origin.x, y: ph - (f.origin.y + f.size.height),
            w: f.size.width, h: f.size.height, scale: s.backingScaleFactor});
}
JSON.stringify(res);`

func (d darwinPlatform) Displays(ctx context.Context) ([]Display, error) {
	out, err := jxa(ctx, displaysJXA)
	if err != nil {
		return nil, d.wrap(err)
	}
	var arr []struct {
		X, Y, W, H, Scale float64
	}
	if err := parseJSONLoose(out, &arr); err != nil {
		return nil, fmt.Errorf("解析显示器列表 %q: %w", out, err)
	}
	if len(arr) == 0 {
		return nil, fmt.Errorf("未检测到显示器")
	}
	ds := make([]Display, 0, len(arr))
	for i, a := range arr {
		// 全局坐标系里主屏即原点 (0,0)
		ds = append(ds, Display{
			Index: i, X: int(a.X), Y: int(a.Y), W: int(a.W), H: int(a.H),
			Main: a.X == 0 && a.Y == 0, Scale: int(a.Scale),
		})
	}
	return ds, nil
}

func (d darwinPlatform) Screenshot(ctx context.Context, display *int) ([]byte, error) {
	// 单屏截图：screencapture 不带 -R 时默认只截主屏，显式给该屏的
	// bounds，保证截到的内容与该屏的全局坐标系一致（省略编号=主屏）
	ds, err := d.Displays(ctx)
	if err != nil {
		return nil, err
	}
	disp, err := pickDisplay(ds, display)
	if err != nil {
		return nil, err
	}
	return screenshotToFile(ctx, []string{"-x", "-t", "png",
		fmt.Sprintf("-R%d,%d,%d,%d", disp.X, disp.Y, disp.W, disp.H)})
}

// screenshotToFile 以给定 screencapture 参数截图到临时文件并读回。
func screenshotToFile(ctx context.Context, args []string) ([]byte, error) {
	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("grf-computer-%d.png", time.Now().UnixNano()))
	defer os.Remove(tmp)
	args = append(append([]string{}, args...), tmp)
	if _, err := run(ctx, "screencapture", args...); err != nil {
		return nil, err
	}
	return os.ReadFile(tmp)
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
	// pbcopy 落板是异步的：立即粘贴/读取可能仍拿到旧内容（"写入不生效"
	// 的表现）。轮询确认剪贴板真的变成目标文本，超时才报错——失败给明确
	// 错误而非静默
	for i := 0; i < 20; i++ {
		if cur, err := pbpasteRaw(ctx); err == nil && cur == text {
			return nil
		}
		select {
		case <-time.After(50 * time.Millisecond):
		case <-cctx.Done():
			return cctx.Err()
		}
	}
	return fmt.Errorf("剪贴板写入未生效（pbcopy 落板超时；若 gh 跑在无 GUI 会话的守护进程里，pbcopy 无法访问粘贴板服务）")
}

// pbpasteRaw 读取剪贴板原文（不 TrimSpace）：恢复剪贴板时必须逐字节原样
// 写回，否则被 run() 截掉尾部的换行/空白，用户剪贴板被无声篡改。
func pbpasteRaw(ctx context.Context) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	var buf bytes.Buffer
	cmd := exec.CommandContext(cctx, "pbpaste")
	cmd.Stdout = &buf
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// parseJSONLoose 解析 JSON。
func parseJSONLoose(s string, v any) error { return json.Unmarshal([]byte(s), v) }
