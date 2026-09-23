// darwin_ax.go macOS 可达性（Accessibility）实现：经 System Events
// （AppleScript）读写 AX 元素树与元素动作——`click UI element` 本身就是
// "AXPress 优先、坐标兜底"语义；应用/窗口枚举用 JXA 调 AppKit/CoreGraphics
// （CGWindowID 供窗口截图）。全程 osascript，零 cgo。
package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// dumpAS 输出元素树（TSV：depth|path|role|name|value|desc|x|y|w|h|enabled）。
// 深度/数量受限防大树拖垮；value/name/desc 截断并去掉分隔符。
const dumpAS = `property outText : ""
property elemCount : 0
property maxElems : 0

on replaceText(t, f, r)
	set olds to AppleScript's text item delimiters
	set AppleScript's text item delimiters to f
	set items_ to text items of t
	set AppleScript's text item delimiters to r
	set t to items_ as text
	set AppleScript's text item delimiters to olds
	return t
end replaceText

on clean(v)
	try
		set v to v as text
	on error
		return ""
	end try
	if v is "missing value" then return ""
	set v to my replaceText(v, "|", "/")
	set v to my replaceText(v, linefeed, " ")
	set v to my replaceText(v, return, " ")
	set v to my replaceText(v, tab, " ")
	if (count of v) > 90 then set v to text 1 thru 90 of v
	return v
end clean

on dumpEl(e, depth, maxDepth, pathStr)
	tell application "System Events"
		set r to ""
		try
			set r to role of e
		end try
		set nme to ""
		try
			set nme to name of e
		end try
		set val to ""
		try
			set val to value of e
		end try
		set dsc to ""
		try
			set dsc to description of e
		end try
		set enb to "1"
		try
			if enabled of e is false then set enb to "0"
		end try
		set p to {0, 0}
		try
			set p to position of e
		end try
		set s to {0, 0}
		try
			set s to size of e
		end try
		set outText to outText & depth & "|" & pathStr & "|" & my clean(r) & "|" & my clean(nme) & "|" & my clean(val) & "|" & my clean(dsc) & "|" & (item 1 of p) & "|" & (item 2 of p) & "|" & (item 1 of s) & "|" & (item 2 of s) & "|" & enb & linefeed
		set elemCount to elemCount + 1
		if depth < maxDepth and elemCount < maxElems then
			set kids to {}
			try
				set kids to UI elements of e
			end try
			repeat with i from 1 to (count of kids)
				if elemCount < maxElems then my dumpEl(item i of kids, depth + 1, maxDepth, pathStr & "." & i)
			end repeat
		end if
	end tell
end dumpEl

on run argv
	set pidNum to (item 1 of argv) as integer
	set maxDepth to (item 2 of argv) as integer
	set maxElems to (item 3 of argv) as integer
	tell application "System Events"
		set procList to (application processes whose unix id is pidNum)
		if (count of procList) is 0 then error "ERR_NOPROC"
		set theProc to item 1 of procList
		set wins to windows of theProc
		set wi to 0
		repeat with w in wins
			set wi to wi + 1
			set wName to ""
			try
				set wName to name of w
			end try
			set p to {0, 0}
			try
				set p to position of w
			end try
			set s to {0, 0}
			try
				set s to size of w
			end try
			set outText to outText & "0|" & wi & "|AXWindow|" & my clean(wName) & "|||" & (item 1 of p) & "|" & (item 2 of p) & "|" & (item 1 of s) & "|" & (item 2 of s) & "|1" & linefeed
			set elemCount to elemCount + 1
			if elemCount < maxElems then my dumpEl(contents of w, 1, maxDepth, wi as text)
		end repeat
	end tell
	return outText
end run`

// actionAS 元素动作：按 path（窗口序号.子序号...）重走同一遍历序，先校验
// role/name 与快照一致，再 press/set_value/locate。校验失败 ERR_CHANGED、
// 元素不可直接点 ERR_NOACTION（调用方坐标兜底）。
const actionAS = `on splitText(t, d)
	set res to {}
	set olds to AppleScript's text item delimiters
	set AppleScript's text item delimiters to d
	repeat with p in text items of t
		set end of res to contents of p
	end repeat
	set AppleScript's text item delimiters to olds
	return res
end splitText

on run argv
	tell application "System Events"
		set procList to (application processes whose unix id is ((item 1 of argv) as integer))
		if (count of procList) is 0 then error "ERR_NOPROC"
		set theProc to item 1 of procList
		set segs to my splitText(item 2 of argv, ".")
		set r to a reference to window ((item 1 of segs) as integer) of theProc
		repeat with i from 2 to (count of segs)
			set r to a reference to UI element ((item i of segs) as integer) of r
		end repeat
		set aRole to ""
		try
			set aRole to role of r
		end try
		if aRole is not (item 3 of argv) then error "ERR_CHANGED role=" & aRole
		set aName to ""
		try
			set aName to name of r
			if aName is missing value then set aName to ""
			set aName to aName as text
		end try
		if (item 4 of argv) is not "" and aName is not (item 4 of argv) then error "ERR_CHANGED name=" & aName
		set act to item 5 of argv
		if act is "press" then
			try
				click r
				return "pressed"
			on error errMsg
				error "ERR_NOACTION " & errMsg
			end try
		else if act is "set_value" then
			try
				set focused of r to true
			end try
			set value of r to (item 6 of argv)
			return "set"
		else if act is "locate" then
			set p to position of r
			set s to size of r
			return ("" & ((item 1 of p) + ((item 1 of s) div 2))) & "," & ("" & ((item 2 of p) + ((item 2 of s) div 2)))
		end if
		error "ERR_BADACTION"
	end tell
end run`

// appsJXA NSWorkspace 枚举运行中 GUI 应用（pid/名称/是否前台）。
const appsJXA = `ObjC.import('AppKit');
const apps = $.NSWorkspace.sharedWorkspace.runningApplications;
const res = [];
for (let i = 0; i < apps.count; i++) {
  const a = apps.objectAtIndex(i);
  const pid = a.processIdentifier;
  res.push({pid: pid, name: a.localizedName.js, front: a.isActive});
}
JSON.stringify(res);`

// windowsAS 枚举进程窗口（System Events）。macOS 的 CGWindowID 无法在
// JXA 里可靠取得（CF 类型桥接不稳），改用合成窗口 id = pid*100+窗口序号，
// ScreenshotWindow 按同编码解码回进程+窗口序取边界截图。
const windowsAS = `on clean(v)
	try
		set v to v as text
	on error
		return ""
	end try
	return my rep(v)
end clean

on rep(t)
	set olds to AppleScript's text item delimiters
	set AppleScript's text item delimiters to "|"
	set t to (text items of t) as text
	set AppleScript's text item delimiters to olds
	return t
end rep

on run argv
	set pidNum to (item 1 of argv) as integer
	tell application "System Events"
		set procList to (application processes whose unix id is pidNum)
		if (count of procList) is 0 then return ""
		set theProc to item 1 of procList
		set res to ""
		set wi to 0
		repeat with w in windows of theProc
			set wi to wi + 1
			set wName to ""
			try
				set wName to my clean(name of w)
			end try
			set p to {0, 0}
			try
				set p to position of w
			end try
			set s to {0, 0}
			try
				set s to size of w
			end try
			set res to res & (pidNum * 100 + wi) & "|" & wName & "|" & (item 1 of p) & "|" & (item 2 of p) & "|" & (item 1 of s) & "|" & (item 2 of s) & linefeed
		end repeat
		return res
	end tell
end run`

// winRectAS 取进程第 N 个窗口的边界（供窗口截图）。
const winRectAS = `on run argv
	set pidNum to (item 1 of argv) as integer
	set wi to (item 2 of argv) as integer
	tell application "System Events"
		set procList to (application processes whose unix id is pidNum)
		if (count of procList) is 0 then error "ERR_NOPROC"
		set theProc to item 1 of procList
		set wins to windows of theProc
		if wi < 1 or wi > (count of wins) then error "ERR_NOWIN"
		set w to item wi of wins
		set p to position of w
		set s to size of w
		return ((item 1 of p) as text) & "," & ((item 2 of p) as text) & "," & ((item 1 of s) as text) & "," & ((item 2 of s) as text)
	end tell
end run`

func (d darwinPlatform) Apps(ctx context.Context) ([]AppInfo, error) {
	out, err := jxa(ctx, appsJXA)
	if err != nil {
		return nil, d.wrap(err)
	}
	var arr []struct {
		PID   int    `json:"pid"`
		Name  string `json:"name"`
		Front bool   `json:"front"`
	}
	if err := parseJSONLoose(out, &arr); err != nil {
		return nil, fmt.Errorf("解析应用列表: %w", err)
	}
	apps := make([]AppInfo, 0, len(arr))
	// 按名去重：浏览器/Electron 的渲染子进程在 NSWorkspace 里与主进程同名
	// （如 Safari浏览器 + 其 Networking/网页内容进程），保留首个（主进程）
	seen := map[string]bool{}
	for _, a := range arr {
		if a.Name == "" || seen[a.Name] {
			continue
		}
		seen[a.Name] = true
		apps = append(apps, AppInfo{PID: a.PID, Name: a.Name, Frontmost: a.Front})
	}
	return apps, nil
}

func (d darwinPlatform) AppWindows(ctx context.Context, pid int) ([]WinInfo, error) {
	out, err := runScript(ctx, 30e9, []string{"osascript"}, ".applescript", windowsAS, strconv.Itoa(pid))
	if err != nil {
		return nil, d.wrap(err)
	}
	var wins []WinInfo
	for i, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(line), "|")
		if len(f) != 6 {
			continue
		}
		id, _ := strconv.ParseInt(f[0], 10, 64)
		wins = append(wins, WinInfo{ID: id, PID: pid, Title: f[1],
			X: atoi(f[2]), Y: atoi(f[3]), W: atoi(f[4]), H: atoi(f[5]), Main: i == 0})
	}
	return wins, nil
}

func (d darwinPlatform) DumpElements(ctx context.Context, pid int, maxDepth int) ([]*Node, error) {
	if maxDepth <= 0 {
		maxDepth = 10
	}
	if maxDepth > 15 {
		maxDepth = 15
	}
	out, err := runScript(ctx, 60e9, []string{"osascript"}, ".applescript", dumpAS,
		strconv.Itoa(pid), strconv.Itoa(maxDepth), "300")
	if err != nil {
		return nil, d.wrap(err)
	}
	nodes := parseTSVNodes(out)
	if len(nodes) == 0 {
		return nil, fmt.Errorf("进程 %d 无可读窗口（可能无窗口或在后台菜单栏运行）", pid)
	}
	// 窗口层补合成窗口 id（pid*100+序号，与 windowsAS/AppWindows 编码一致，
	// 供 computer_screenshot 的 window_id）
	for i := range nodes {
		if nodes[i].Depth != 0 {
			break
		}
		nodes[i].WindowID = int64(pid)*100 + int64(i+1)
	}
	return nodes, nil
}

// parseTSVNodes 解析 dumpAS 的 TSV 输出为节点列表（跨文件复用：Windows
// PowerShell / Linux atspi 输出同格式）。
func parseTSVNodes(out string) []*Node {
	var nodes []*Node
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		f := strings.Split(line, "|")
		if len(f) < 11 {
			continue
		}
		n := Node{
			Depth: atoi(f[0]), Path: f[1], Role: f[2], Name: f[3],
			Value: f[4], Desc: f[5],
			X: atoi(f[6]), Y: atoi(f[7]), W: atoi(f[8]), H: atoi(f[9]),
			Enabled: f[10] != "0",
		}
		if len(f) >= 12 { // 第 12 列为窗口层节点的平台窗口 ID（Windows=HWND）
			n.WindowID, _ = strconv.ParseInt(strings.TrimSpace(f[11]), 10, 64)
		}
		nodes = append(nodes, &n)
	}
	return nodes
}

// atoi 宽松整数解析（失败给 0）。
func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

func (d darwinPlatform) ElementAction(ctx context.Context, pid int, path, wantRole, wantTitle, action, value string) (ElemActionResult, error) {
	out, err := runScript(ctx, 30e9, []string{"osascript"}, ".applescript", actionAS,
		strconv.Itoa(pid), path, wantRole, wantTitle, action, value)
	if err != nil {
		msg := err.Error()
		switch {
		case strings.Contains(msg, "ERR_CHANGED"):
			// 脚本输出 "ERR_CHANGED role=XXX"/"name=XXX"，带实际值回报
			var got Node
			if i := strings.Index(msg, "role="); i >= 0 {
				got.Role = msg[i+6:]
			}
			if i := strings.Index(msg, "name="); i >= 0 {
				got.Name = msg[i+5:]
			}
			return ElemActionResult{}, &errChanged{detail: "元素签名不符（" + msg[strings.Index(msg, "ERR_CHANGED"):] + "）"}
		case strings.Contains(msg, "ERR_NOACTION"):
			// 元素无直接点按动作：转 locate 拿中心坐标，调用方坐标兜底
			return d.ElementAction(ctx, pid, path, wantRole, wantTitle, "locate", "")
		case strings.Contains(msg, "ERR_NOPROC"):
			return ElemActionResult{}, fmt.Errorf("进程 %d 已退出", pid)
		}
		return ElemActionResult{}, d.wrap(err)
	}
	out = strings.TrimSpace(out)
	if action == "locate" {
		xy := strings.SplitN(out, ",", 2)
		if len(xy) == 2 {
			return ElemActionResult{HasPos: true, X: atoi(xy[0]), Y: atoi(xy[1])}, nil
		}
		return ElemActionResult{}, fmt.Errorf("定位输出异常 %q", out)
	}
	return ElemActionResult{Text: out}, nil
}

func (d darwinPlatform) ActivateApp(ctx context.Context, name string) error {
	// open -a 对运行中应用等效激活、未运行则拉起；应用名单参数传递无注入
	if _, err := run(ctx, "open", "-a", name); err != nil {
		return fmt.Errorf("无法启动/聚焦应用 %q（检查名称，可用 computer_list_apps 查询）: %w", name, err)
	}
	return nil
}

func (d darwinPlatform) ScreenshotWindow(ctx context.Context, winID int64) ([]byte, error) {
	// 合成 id 解码：pid*100+窗口序号；取窗口边界后按区域截屏
	pid := int(winID / 100)
	idx := int(winID % 100)
	if pid <= 0 || idx <= 0 {
		return nil, fmt.Errorf("window_id 无效（应取 computer_get_app_state 窗口行的 window_id）")
	}
	out, err := runScript(ctx, 30e9, []string{"osascript"}, ".applescript", winRectAS,
		strconv.Itoa(pid), strconv.Itoa(idx))
	if err != nil {
		return nil, d.wrap(err)
	}
	f := strings.Split(strings.TrimSpace(out), ",")
	if len(f) != 4 {
		return nil, fmt.Errorf("窗口边界输出异常 %q", out)
	}
	return screenshotToFile(ctx, []string{"-x", "-t", "png",
		fmt.Sprintf("-R%s,%s,%s,%s", f[0], f[1], f[2], f[3])})
}
