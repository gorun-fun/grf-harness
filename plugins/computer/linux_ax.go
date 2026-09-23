// linux_ax.go Linux 可达性实现：元素树/动作用尽力路径——python3 + GObject
// Atspi（多数 GNOME 系发行版自带 python3-gobject；缺失时明确降级提示改用
// 截图+坐标模式）。应用/窗口枚举用 wmctrl（回退 xdotool 逐窗口查询），
// 激活用 xdotool windowactivate，窗口截图用 ImageMagick import。TSV 输出
// 格式与 macOS/Windows 一致（第 12 列窗口层=X window id）。
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// pyDump Atspi 元素树遍历（argv: pid max_depth max_elems）。
const pyDump = `import sys
try:
    import gi
    gi.require_version('Atspi', '2.0')
    from gi.repository import Atspi
except Exception:
    print('ERR_NOATSPI'); sys.exit(3)
pid, max_depth, max_elems = int(sys.argv[1]), int(sys.argv[2]), int(sys.argv[3])
desktop = Atspi.get_desktop(0)
targets = []
for i in range(desktop.get_child_count()):
    a = desktop.get_child_at_index(i)
    if a and a.get_process_id() == pid:
        targets.append(a)
if not targets:
    print('ERR_NOPROC'); sys.exit(4)
count = 0
def clean(v):
    if v is None: return ''
    s = str(v).replace('|', '/').replace('\n', ' ').replace('\r', ' ').replace('\t', ' ')
    return s[:90]
def props(el):
    role = clean(el.get_role_name()); name = clean(el.get_name())
    desc = clean(el.get_description())
    val = ''
    try:
        t = el.query_text(); val = clean(t.get_text(0, -1))
    except Exception:
        pass
    x = y = w = h = 0
    try:
        e = el.get_extents(Atspi.CoordType.SCREEN)
        x, y, w, h = e.x, e.y, e.width, e.height
    except Exception:
        pass
    st = el.get_state_set()
    en = 1 if (st.contains(Atspi.StateType.ENABLED) or st.contains(Atspi.StateType.SENSITIVE)) else 0
    return role, name, val, desc, x, y, w, h, en
def dump(el, depth, path):
    global count
    if count >= max_elems: return
    role, name, val, desc, x, y, w, h, en = props(el)
    print('%d|%s|%s|%s|%s|%s|%d|%d|%d|%d|%d' % (depth, path, role, name, val, desc, x, y, w, h, en), flush=True)
    count += 1
    if depth >= max_depth: return
    for i in range(el.get_child_count()):
        if count >= max_elems: return
        c = el.get_child_at_index(i)
        if c: dump(c, depth + 1, path + '.' + str(i + 1))
wi = 0
for a in targets:
    for j in range(a.get_child_count()):
        w = a.get_child_at_index(j)
        if not w: continue
        wi += 1
        role, name, val, desc, x, y, ww, hh, en = props(w)
        print('%d|%d|%s|%s|%s|%s|%d|%d|%d|%d|%d|%d' % (0, wi, role, name, val, desc, x, y, ww, hh, en, 0), flush=True)
        count += 1
        dump(w, 1, str(wi))
if wi == 0:
    print('ERR_NOWIN'); sys.exit(5)`

// pyAction Atspi 元素动作（argv: pid path want_role want_title action value）。
// 窗口 id 用 X window id（wmctrl 查询），Atspi 侧按 pid 首应用的窗口序对齐
// dump 的顺序。press 找 click/press/activate 动作，找不到报 ERR_NOACTION。
const pyAction = `import sys
try:
    import gi
    gi.require_version('Atspi', '2.0')
    from gi.repository import Atspi
except Exception:
    print('ERR_NOATSPI'); sys.exit(3)
pid = int(sys.argv[1]); path = sys.argv[2]
want_role, want_title, action, value = sys.argv[3], sys.argv[4], sys.argv[5], sys.argv[6]
desktop = Atspi.get_desktop(0)
targets = []
for i in range(desktop.get_child_count()):
    a = desktop.get_child_at_index(i)
    if a and a.get_process_id() == pid:
        targets.append(a)
if not targets:
    print('ERR_NOPROC'); sys.exit(4)
wins = []
for a in targets:
    for j in range(a.get_child_count()):
        w = a.get_child_at_index(j)
        if w: wins.append(w)
segs = [int(s) for s in path.split('.')]
if segs[0] < 1 or segs[0] > len(wins):
    print('ERR_CHANGED window gone'); sys.exit(6)
el = wins[segs[0] - 1]
for k in segs[1:]:
    el = el.get_child_at_index(k - 1) if k - 1 < el.get_child_count() else None
    if el is None:
        print('ERR_CHANGED element gone'); sys.exit(6)
if el.get_role_name() != want_role:
    print('ERR_CHANGED role=%s' % el.get_role_name()); sys.exit(6)
if want_title and el.get_name() != want_title:
    print('ERR_CHANGED name=%s' % el.get_name()); sys.exit(6)
if action == 'press':
    names = [el.get_action_name(i) for i in range(el.get_n_actions())]
    for i, n in enumerate(names):
        if n in ('click', 'press', 'activate', 'toggle'):
            el.do_action(i); print('pressed'); sys.exit(0)
    print('ERR_NOACTION %s' % names); sys.exit(5)
elif action == 'set_value':
    try:
        el.query_value().set_current_value(float(value) if value else 0.0)
        try:
            t = el.query_text()
            t.set_text_contents(value)
        except Exception:
            pass
        print('set'); sys.exit(0)
    except Exception as e:
        try:
            el.query_text().set_text_contents(value); print('set'); sys.exit(0)
        except Exception:
            print('ERR_NOACTION %s' % e); sys.exit(5)
elif action == 'locate':
    e = el.get_extents(Atspi.CoordType.SCREEN)
    print('%d,%d' % (e.x + e.width // 2, e.y + e.height // 2)); sys.exit(0)
print('ERR_BADACTION'); sys.exit(7)`

func py3(ctx context.Context, script string, args ...string) (string, error) {
	return runScript(ctx, 60e9, []string{"python3"}, ".py", script, args...)
}

// wmctrlWindows 解析 wmctrl -lp：winid(0x..) desktop pid host title。
func (l linuxPlatform) wmctrlWindows(ctx context.Context) ([]WinInfo, bool) {
	out, err := run(ctx, "wmctrl", "-lp")
	if err != nil {
		return nil, false
	}
	var wins []WinInfo
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		id, err := strconv.ParseInt(f[0], 0, 64)
		if err != nil {
			continue
		}
		pid, _ := strconv.Atoi(f[2])
		title := strings.Join(f[4:], " ")
		wins = append(wins, WinInfo{ID: id, PID: pid, Title: title})
	}
	return wins, true
}

// procName 从 /proc 读进程名。
func procName(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (l linuxPlatform) Apps(ctx context.Context) ([]AppInfo, error) {
	wins, ok := l.wmctrlWindows(ctx)
	if !ok {
		return nil, fmt.Errorf("需要 wmctrl（或安装后重试）")
	}
	front := 0
	if out, err := xdotool(ctx, "getactivewindow", "getwindowpid"); err == nil {
		front, _ = strconv.Atoi(strings.TrimSpace(out))
	}
	seen := map[int]bool{}
	var apps []AppInfo
	for _, w := range wins {
		if w.PID <= 0 || seen[w.PID] {
			continue
		}
		seen[w.PID] = true
		if n := procName(w.PID); n != "" {
			apps = append(apps, AppInfo{PID: w.PID, Name: n, Frontmost: w.PID == front})
		}
	}
	return apps, nil
}

func (l linuxPlatform) AppWindows(ctx context.Context, pid int) ([]WinInfo, error) {
	wins, ok := l.wmctrlWindows(ctx)
	if !ok {
		return nil, fmt.Errorf("需要 wmctrl")
	}
	var res []WinInfo
	for i, w := range wins {
		if w.PID != pid {
			continue
		}
		// 几何经 xdotool 补全
		if out, err := xdotool(ctx, "getwindowgeometry", "--shell", strconv.FormatInt(w.ID, 10)); err == nil {
			var g struct{ x, y, w, h int }
			for _, kv := range strings.Fields(strings.ReplaceAll(out, "\n", " ")) {
				p := strings.SplitN(kv, "=", 2)
				if len(p) != 2 {
					continue
				}
				switch p[0] {
				case "X":
					g.x = atoi(p[1])
				case "Y":
					g.y = atoi(p[1])
				case "WIDTH":
					g.w = atoi(p[1])
				case "HEIGHT":
					g.h = atoi(p[1])
				}
			}
			w.X, w.Y, w.W, w.H = g.x, g.y, g.w, g.h
		}
		w.Main = len(res) == 0 && i >= 0
		res = append(res, w)
	}
	return res, nil
}

func (l linuxPlatform) DumpElements(ctx context.Context, pid int, maxDepth int) ([]*Node, error) {
	if maxDepth <= 0 {
		maxDepth = 10
	}
	if maxDepth > 15 {
		maxDepth = 15
	}
	out, err := py3(ctx, pyDump, strconv.Itoa(pid), strconv.Itoa(maxDepth), "300")
	if err != nil {
		msg := err.Error()
		switch {
		case strings.Contains(msg, "ERR_NOATSPI"):
			return nil, fmt.Errorf("本机缺少 python3-gobject（Atspi 绑定），无法读元素树；请改用 computer_screenshot + 坐标模式操作，或安装 python3-gobject")
		case strings.Contains(msg, "ERR_NOPROC"):
			return nil, fmt.Errorf("进程 %d 已退出或无可达性接口", pid)
		case strings.Contains(msg, "ERR_NOWIN"):
			return nil, fmt.Errorf("进程 %d 无可读窗口", pid)
		}
		return nil, err
	}
	nodes := parseTSVNodes(out)
	if len(nodes) == 0 {
		return nil, fmt.Errorf("进程 %d 无可读窗口", pid)
	}
	// 窗口层补 X window id（与 wmctrl 的窗口序对齐，供窗口截图）
	if wins, ok := l.wmctrlWindows(ctx); ok {
		wi := 0
		for i := range nodes {
			if nodes[i].Depth != 0 {
				break
			}
			wi++
			for _, w := range wins {
				if w.PID == pid {
					nodes[i].WindowID = w.ID
					break
				}
			}
		}
	}
	return nodes, nil
}

func (l linuxPlatform) ElementAction(ctx context.Context, pid int, path, wantRole, wantTitle, action, value string) (ElemActionResult, error) {
	out, err := py3(ctx, pyAction, strconv.Itoa(pid), path, wantRole, wantTitle, action, value)
	if err != nil {
		msg := err.Error()
		switch {
		case strings.Contains(msg, "ERR_CHANGED"):
			return ElemActionResult{}, &errChanged{detail: "元素签名不符（" + msg + "）"}
		case strings.Contains(msg, "ERR_NOACTION") && action == "press":
			return l.ElementAction(ctx, pid, path, wantRole, wantTitle, "locate", "")
		case strings.Contains(msg, "ERR_NOATSPI"):
			return ElemActionResult{}, fmt.Errorf("本机缺少 python3-gobject（Atspi 绑定）；请改用坐标模式")
		case strings.Contains(msg, "ERR_NOPROC"):
			return ElemActionResult{}, fmt.Errorf("进程 %d 已退出", pid)
		}
		return ElemActionResult{}, err
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

func (l linuxPlatform) ActivateApp(ctx context.Context, name string) error {
	wins, ok := l.wmctrlWindows(ctx)
	if ok {
		for _, w := range wins {
			if w.PID > 0 && procName(w.PID) == name {
				_, err := xdotool(ctx, "windowactivate", strconv.FormatInt(w.ID, 10))
				return err
			}
		}
	}
	// 未运行：无 shell 直接拉起（名称即命令行）
	cmd := exec.Command(name)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("无法启动/聚焦应用 %q（computer_list_apps 查询名称；Linux 下应用名=进程名/命令名）: %w", name, err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func (l linuxPlatform) ScreenshotWindow(ctx context.Context, winID int64) ([]byte, error) {
	tmp := tempFile("png")
	defer removeFile(tmp)
	if _, err := run(ctx, "import", "-window", strconv.FormatInt(winID, 10), tmp); err != nil {
		return nil, fmt.Errorf("窗口截图需要 ImageMagick(import): %w", err)
	}
	return readFile(tmp)
}
