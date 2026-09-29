// tools.go grf-computer 工具面（zcode-cua 式"AX 优先 + 坐标兜底"）：
// 观察类（list_apps/get_app_state/screenshot）只读；动作类双模式——元素
// 定位（state_id+index，主路径，经可达性 API 直接操作元素）或坐标（x/y，
// 兜底路径，合成鼠标键盘事件）。操作建议顺序：get_app_state 或 screenshot
// 观察 → 动作 → 再观察确认。
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"git.gorun.fun/share/grf-harness/plugins/internal/mcpserver"
)

func cobj(props map[string]any, required ...string) map[string]any {
	r := []any{}
	for _, n := range required {
		r = append(r, n)
	}
	return map[string]any{"type": "object", "properties": props, "required": r}
}

func cstr(desc string) map[string]any  { return map[string]any{"type": "string", "description": desc} }
func cbool(desc string) map[string]any { return map[string]any{"type": "boolean", "description": desc} }
func cnum(desc string) map[string]any  { return map[string]any{"type": "number", "description": desc} }
func ctext(s string) []mcpserver.Content {
	return []mcpserver.Content{mcpserver.Text(s)}
}

// targetProps 动作工具的元素定位参数（与 x/y 坐标二选一，元素优先）。
var targetProps = map[string]any{
	"state_id": cstr("computer_get_app_state 返回的 state_id（元素定位模式，与 x/y 二选一，优先）"),
	"index":    cnum("state 树中的元素索引（与 state_id 配对使用）"),
}

// targetReq 动作参数结构：state_id+index 为元素模式，缺省走 x/y 坐标。
type targetReq struct {
	StateID string `json:"state_id"`
	Index   *int   `json:"index"`
	X       *int   `json:"x"`
	Y       *int   `json:"y"`
}

// resolveElement 取元素模式的目标（快照 + 节点）；给了 state_id 但没给
// index 时：带 x/y 视为坐标模式（computer_click 的坐标绑定快照校验等），
// 返回快照与 nil 节点；坐标也没给才是参数错误。非元素模式返回双 nil。
func resolveElement(a targetReq) (*State, *Node, error) {
	if a.StateID == "" {
		return nil, nil, nil
	}
	if a.Index == nil {
		if a.X == nil && a.Y == nil {
			return nil, nil, fmt.Errorf("给了 state_id 就必须给 index（元素在 state 树中的索引）")
		}
		st, ok := states.get(a.StateID)
		if !ok {
			return nil, nil, fmt.Errorf("state_id=%s 不存在或已过期，请重新 computer_get_app_state 观察", a.StateID)
		}
		return st, nil, nil
	}
	st, ok := states.get(a.StateID)
	if !ok {
		return nil, nil, fmt.Errorf("state_id=%s 不存在或已过期，请重新 computer_get_app_state 观察", a.StateID)
	}
	if *a.Index < 0 || *a.Index >= len(st.Nodes) {
		return nil, nil, fmt.Errorf("index=%d 超出范围（0..%d），请重新 computer_get_app_state", *a.Index, len(st.Nodes)-1)
	}
	return st, st.Nodes[*a.Index], nil
}

// resolveXY 坐标模式：必须给了 x/y。
func (a targetReq) resolveXY() (int, int, error) {
	if a.X == nil || a.Y == nil {
		return 0, 0, fmt.Errorf("缺少坐标：元素模式需 state_id+index，坐标模式需 x+y")
	}
	return *a.X, *a.Y, nil
}

// registerComputerTools 注册全部 computer_* 工具。
func registerComputerTools(s *mcpserver.Server, p platform) {
	s.Add(mcpserver.Tool{
		Name:        "computer_list_displays",
		Description: "枚举显示器：每屏编号(index)/名称/全局边界 x,y,w,h/是否主屏/缩放 scale（截图像素与坐标的倍率，Retina=2）。鼠标/坐标工具一律用全局坐标系（原点=主屏左上，副屏排左侧/上方时坐标为负）。",
		Schema:      cobj(map[string]any{}),
		ReadOnly:    true,
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			ds, err := p.Displays(ctx)
			if err != nil {
				return nil, err
			}
			var b strings.Builder
			for _, d := range ds {
				main := ""
				if d.Main {
					main = " [主屏]"
				}
				name := d.Name
				if name != "" {
					name = " name=" + name
				}
				fmt.Fprintf(&b, "#%d x=%d y=%d w=%d h=%d scale=%d%s%s\n", d.Index, d.X, d.Y, d.W, d.H, d.Scale, main, name)
			}
			return ctext(strings.TrimRight(b.String(), "\n")), nil
		},
	})

	s.Add(mcpserver.Tool{
		Name:        "computer_list_apps",
		Description: "列出运行中的 GUI 应用（pid、名称、bundle id、是否前台）。名称是本地化名（中文系统的 Finder 显示为\"访达\"）；跨语言稳定引用用 bundle id（computer_activate_app / computer_get_app_state 均接受）。",
		Schema:      cobj(map[string]any{}),
		ReadOnly:    true,
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			apps, err := p.Apps(ctx)
			if err != nil {
				return nil, err
			}
			var b strings.Builder
			for _, a := range apps {
				mark := ""
				if a.Frontmost {
					mark = " [前台]"
				}
				bundle := ""
				if a.BundleID != "" {
					bundle = " bundle=" + a.BundleID
				}
				fmt.Fprintf(&b, "pid=%d %q%s%s\n", a.PID, a.Name, bundle, mark)
			}
			if b.Len() == 0 {
				return ctext("(无 GUI 应用)"), nil
			}
			return ctext(strings.TrimRight(b.String(), "\n")), nil
		},
	})

	s.Add(mcpserver.Tool{
		Name: "computer_get_app_state",
		Description: "读取应用的界面元素树（可达性 API，最可靠的观察手段，优先于截图）：每个元素带索引/角色/名称/值/坐标，返回 state_id。" +
			"后续 computer_click/type/set_value/scroll 用 state_id+index 精确定位元素（比截图猜坐标可靠得多，且不依赖窗口在前台）。" +
			"app 省略=前台应用；可传应用名、bundle id 或 pid（见 computer_list_apps）。界面变化后旧 state_id 会失效，需重新观察。",
		Schema: cobj(map[string]any{
			"app":       cstr("应用名、bundle id 或 pid（省略=前台应用）"),
			"max_depth": cnum("元素树遍历深度上限（默认 10，最大 15；界面无响应/元素过多时调小）"),
		}),
		ReadOnly: true,
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			var a struct {
				App      string `json:"app"`
				MaxDepth int    `json:"max_depth"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			pid, name, bundle, err := resolveApp(ctx, p, a.App)
			if err != nil {
				return nil, err
			}
			nodes, err := p.DumpElements(ctx, pid, a.MaxDepth)
			if err != nil {
				return nil, err
			}
			st := states.put(pid, name, bundle, nodes)
			return ctext(renderState(st)), nil
		},
	})

	s.Add(mcpserver.Tool{
		Name: "computer_screenshot",
		Description: "截图回传 PNG（多模态）。窗口截图传 window_id（computer_get_app_state 窗口行的 window_id，坐标原点=该窗口左上）；整屏截图传 display 编号（省略=主屏，坐标为全局坐标系）。" +
			"图上像素坐标 ÷ scale（Retina=2）后，整屏加全局起点/窗口加窗口左上（见元素树窗口行的 (x,y)）才是点击用的全局坐标。优先用 computer_get_app_state 读元素树，截图用于定位纯图形内容。",
		Schema: cobj(map[string]any{
			"display":   cnum("显示器编号（computer_list_displays 的 index，可省略=主屏）"),
			"window_id": cnum("窗口 ID（computer_get_app_state 输出的 window_id；优先于 display）"),
		}),
		ReadOnly: true,
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			var a struct {
				Display  *int   `json:"display"`
				WindowID *int64 `json:"window_id"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			var data []byte
			var meta string
			if a.WindowID != nil {
				var err error
				data, err = p.ScreenshotWindow(ctx, *a.WindowID)
				if err != nil {
					return nil, err
				}
				meta = fmt.Sprintf("窗口截图 window_id=%d；坐标原点=窗口左上（全局位置见元素树窗口行）", *a.WindowID)
			} else {
				ds, err := p.Displays(ctx)
				if err != nil {
					return nil, err
				}
				d, err := pickDisplay(ds, a.Display)
				if err != nil {
					return nil, err
				}
				data, err = p.Screenshot(ctx, &d.Index)
				if err != nil {
					return nil, err
				}
				meta = fmt.Sprintf("整屏截图 display#%d（全局坐标 x=%d..%d, y=%d..%d）", d.Index, d.X, d.X+d.W, d.Y, d.Y+d.H)
			}
			if pw, ph, ok := pngSize(data); ok {
				return []mcpserver.Content{
					mcpserver.Text(meta + fmt.Sprintf("；图像 %dx%d 像素，像素坐标按 scale 换算（÷scale 后加对应原点）", pw, ph)),
					mcpserver.ImagePNG(base64.StdEncoding.EncodeToString(data)),
				}, nil
			}
			return []mcpserver.Content{mcpserver.ImagePNG(base64.StdEncoding.EncodeToString(data))}, nil
		},
	})

	s.Add(mcpserver.Tool{
		Name:        "computer_screen_size",
		Description: "查询屏幕分辨率（宽高；省略 display=主屏）。截图/点击前可用来换算坐标。",
		Schema: cobj(map[string]any{
			"display": cnum("显示器编号（可省略=主屏）"),
		}),
		ReadOnly: true,
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			var a struct {
				Display *int `json:"display"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			w, h, err := p.ScreenSize(ctx, a.Display)
			if err != nil {
				return nil, err
			}
			return ctext(fmt.Sprintf("%d x %d", w, h)), nil
		},
	})

	s.Add(mcpserver.Tool{
		Name: "computer_click",
		Description: "点击鼠标。优先元素模式（state_id+index，经可达性 API 直接点元素，不依赖坐标换算与窗口前台）；坐标模式传全局 x/y（原点=主屏左上，配合 computer_screenshot 使用）。" +
			"坐标模式强烈建议与 state_id（不带 index）联用：坐标取自该快照时，点击前校验快照窗口未移动，窗口漂移即报错——旧坐标会静默点进其他应用。button=right/middle 或双击自动退化为元素中心坐标点击。",
		Schema: cobj(mergeProps(map[string]any{
			"x": cnum("横坐标（坐标模式）"), "y": cnum("纵坐标（坐标模式）"),
			"button": map[string]any{"type": "string", "enum": []string{"left", "right", "middle"}, "description": "鼠标键（默认 left）"},
			"clicks": cnum("连击次数（2=双击，默认 1）"),
		})),
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			var a struct {
				targetReq
				Button string `json:"button"`
				Clicks int    `json:"clicks"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			if a.Clicks == 0 {
				a.Clicks = 1
			}
			if a.Button == "" {
				a.Button = "left"
			}
			// 坐标绑定快照（state_id 无 index + x/y）：点击前校验快照窗口现帧，
			// 漂移超阈值即报错——窗口挪动后旧坐标会落进别的应用（静默误击，
			// 输入进无关窗口可能触发其快捷键）
			if a.StateID != "" && a.Index == nil {
				if err := validateCoordSnapshot(ctx, p, a.targetReq); err != nil {
					return nil, err
				}
			}
			st, n, err := resolveElement(a.targetReq)
			if err != nil {
				return nil, err
			}
			var x, y int
			how := ""
			if n != nil {
				// 纯左键单击走 AXPress；其余（右键/双击）取元素中心走坐标事件
				if a.Button == "left" && a.Clicks == 1 {
					res, err := p.ElementAction(ctx, st.PID, n.Path, n.Role, n.Name, "press", "")
					if err != nil {
						return nil, err
					}
					if !res.HasPos {
						return ctext(fmt.Sprintf("已点击元素 [%d] %s %q（可达性 API 直点）", *a.Index, n.Role, trunc(n.Name, 30))), nil
					}
					x, y = res.X, res.Y
					how = "元素中心"
				} else {
					res, err := p.ElementAction(ctx, st.PID, n.Path, n.Role, n.Name, "locate", "")
					if err != nil {
						return nil, err
					}
					if !res.HasPos {
						return nil, fmt.Errorf("无法定位元素 [%d] 的坐标", *a.Index)
					}
					x, y = res.X, res.Y
					how = "元素中心"
				}
			} else {
				var err error
				x, y, err = a.resolveXY()
				if err != nil {
					return nil, err
				}
			}
			if err := p.Click(ctx, x, y, a.Button, a.Clicks); err != nil {
				return nil, err
			}
			if how != "" {
				return ctext(fmt.Sprintf("已在 %s (%d,%d) 点击 %s ×%d（元素 [%d]）", how, x, y, a.Button, a.Clicks, *a.Index)), nil
			}
			return ctext(fmt.Sprintf("已在 (%d,%d) 点击 %s ×%d", x, y, a.Button, a.Clicks)), nil
		},
	})

	s.Add(mcpserver.Tool{
		Name: "computer_type",
		Description: "输入文本。优先元素模式（state_id+index）：先尝试可达性 API 直设值并回读校验（不依赖焦点与输入法）；元素不支持下直设（Electron/浏览器等）时自动兜底——激活应用→点击输入框聚焦→三击全选→键入（整体替换原内容）。" +
			"终端类应用（Terminal/iTerm2/WezTerm 等）自动跳过直设值、直接走聚焦+键入——终端文本区是回显缓冲不是输入框，直设值只改屏幕显示、不进输入队列。省略元素则向当前焦点输入（注意：键盘输入只落进前台应用，务必先点击目标输入框）。",
		Schema: cobj(mergeProps(map[string]any{
			"text": cstr("要输入的文本"),
		}), "text"),
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			var a struct {
				targetReq
				Text string `json:"text"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			st, n, err := resolveElement(a.targetReq)
			if err != nil {
				return nil, err
			}
			if n != nil {
				// 终端类应用的文本区是回显缓冲：AX 设值"成功"也只是改了屏幕
				// 回显，不进 PTY 输入——必须走真实键盘路径
				terminal := isTerminalApp(st.App, st.BundleID)
				if !terminal {
					if res, err := p.ElementAction(ctx, st.PID, n.Path, n.Role, n.Name, "set_value", a.Text); err == nil {
						verified, verr := verifySetValue(a.Text, res.Text)
						if verr != nil {
							return nil, verr // 回读证明没写进去：如实报错，不假成功
						}
						if verified {
							return ctext(fmt.Sprintf("已向元素 [%d] %s 设值 %d 字符（回读一致）", *a.Index, n.Role, len([]rune(a.Text)))), nil
						}
						return ctext(fmt.Sprintf("已向元素 [%d] 提交设值 %d 字符，但该元素不回读值（密码框等仅写元素），无法确认是否生效", *a.Index, len([]rune(a.Text)))), nil
					} else if _, changed := err.(*errChanged); changed {
						return nil, err
					}
				}
				// 元素不支持 AXSetValue（Electron/浏览器等常见）或目标在终端里：
				// 聚焦输入框后走真实键盘输入兜底。不聚焦就打字是"输入不了"的
				// 头号根因——keystroke 只落进前台应用，而 agent 跑在终端里，
				// 焦点常常就在终端自己身上。
				res, lerr := p.ElementAction(ctx, st.PID, n.Path, n.Role, n.Name, "locate", "")
				if lerr != nil {
					return nil, lerr
				}
				if !res.HasPos {
					return nil, err
				}
				if err := p.ActivateApp(ctx, st.App); err != nil {
					return nil, err
				}
				time.Sleep(250 * time.Millisecond)
				// 三击=全选输入框原内容，随后的输入整体替换（与 set_value 语义
				// 一致）；终端没有可替换的字段内容且三击会选行，单击聚焦即可
				clicks := 3
				if terminal {
					clicks = 1
				}
				if err := p.Click(ctx, res.X, res.Y, "left", clicks); err != nil {
					return nil, err
				}
				time.Sleep(200 * time.Millisecond)
				if err := p.Type(ctx, a.Text); err != nil {
					return nil, err
				}
				if terminal {
					return ctext(fmt.Sprintf("已聚焦终端 [%d] 并键入 %d 字符（终端走键盘路径，不直设回显缓冲）；重要输入建议回读校验", *a.Index, len([]rune(a.Text)))), nil
				}
				return ctext(fmt.Sprintf("元素 [%d] 不支持直设值，已聚焦输入框键入 %d 字符（替换原内容）", *a.Index, len([]rune(a.Text)))), nil
			}
			if err := p.Type(ctx, a.Text); err != nil {
				return nil, err
			}
			return ctext(fmt.Sprintf("已向焦点输入 %d 字符", len([]rune(a.Text)))), nil
		},
	})

	s.Add(mcpserver.Tool{
		Name:        "computer_set_value",
		Description: "设置元素的值（state_id+index 定位，可达性 API 直设：文本框内容、勾选框开关、滑杆数值等），设置后回读校验——回读不一致（应用侧拒绝/钳制，如滑杆被夹回 0）会如实报错而非假报成功；元素不回读值（密码框等）会注明无法确认。对不可设值元素会报错，改用 computer_click。",
		Schema: cobj(mergeProps(map[string]any{
			"value": cstr("目标值"),
		}), "value", "state_id", "index"),
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			var a struct {
				targetReq
				Value string `json:"value"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			st, n, err := resolveElement(a.targetReq)
			if err != nil {
				return nil, err
			}
			if n == nil {
				return nil, fmt.Errorf("computer_set_value 只支持元素模式（state_id+index）")
			}
			res, err := p.ElementAction(ctx, st.PID, n.Path, n.Role, n.Name, "set_value", a.Value)
			if err != nil {
				return nil, err
			}
			verified, verr := verifySetValue(a.Value, res.Text)
			if verr != nil {
				return nil, verr
			}
			if verified {
				return ctext(fmt.Sprintf("已设置元素 [%d] %s 的值为 %q（回读一致）", *a.Index, n.Role, trunc(a.Value, 40))), nil
			}
			return ctext(fmt.Sprintf("已提交设置元素 [%d] %s 的值为 %q，但该元素不回读值，无法确认是否生效", *a.Index, n.Role, trunc(a.Value, 40))), nil
		},
	})

	s.Add(mcpserver.Tool{
		Name:        "computer_key",
		Description: "向当前焦点窗口发送按键/组合键：enter、esc、tab、pageup、f1 等，或 ctrl+c、cmd+shift+t（Win 平台不支持 Win 键）。注意：经合成键击切换输入法（如 ctrl+space）异步且按应用记忆、不保证生效，需要文本输入请用 computer_type（检测到拼音等 IME 输入源时自动改走剪贴板粘贴，不受输入法影响）。",
		Schema:      cobj(map[string]any{"key": cstr("按键或组合，如 ctrl+c、alt+f4、enter")}, "key"),
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			var a struct {
				Key string `json:"key"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			if err := p.Key(ctx, a.Key); err != nil {
				return nil, err
			}
			return ctext("已按键 " + a.Key), nil
		},
	})

	s.Add(mcpserver.Tool{
		Name:        "computer_drag",
		Description: "按住左键从 (from_x,from_y) 平滑拖拽到 (to_x,to_y) 后松开（全局坐标）。",
		Schema: cobj(map[string]any{
			"from_x": cnum("起点横坐标"), "from_y": cnum("起点纵坐标"),
			"to_x": cnum("终点横坐标"), "to_y": cnum("终点纵坐标"),
		}, "from_x", "from_y", "to_x", "to_y"),
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			var a struct {
				Fx int `json:"from_x"`
				Fy int `json:"from_y"`
				Tx int `json:"to_x"`
				Ty int `json:"to_y"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			if err := p.Drag(ctx, a.Fx, a.Fy, a.Tx, a.Ty); err != nil {
				return nil, err
			}
			return ctext(fmt.Sprintf("已拖拽 (%d,%d) → (%d,%d)", a.Fx, a.Fy, a.Tx, a.Ty)), nil
		},
	})

	s.Add(mcpserver.Tool{
		Name:        "computer_scroll",
		Description: "滚动滚轮：direction up/down，amount 行数（默认 3）。元素模式（state_id+index）在该元素中心滚动；坐标模式传 x/y；都省略=当前位置。",
		Schema: cobj(mergeProps(map[string]any{
			"direction": map[string]any{"type": "string", "enum": []string{"up", "down"}, "description": "滚动方向（默认 down）"},
			"amount":    cnum("滚动行数（默认 3）"),
		})),
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			var a struct {
				targetReq
				Direction string `json:"direction"`
				Amount    int    `json:"amount"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			if a.Direction == "" {
				a.Direction = "down"
			}
			if a.Amount == 0 {
				a.Amount = 3
			}
			st, n, err := resolveElement(a.targetReq)
			if err != nil {
				return nil, err
			}
			x, y, hasPos, how := 0, 0, false, "当前位置"
			if n != nil {
				res, err := p.ElementAction(ctx, st.PID, n.Path, n.Role, n.Name, "locate", "")
				if err != nil {
					return nil, err
				}
				if !res.HasPos {
					return nil, fmt.Errorf("无法定位元素 [%d] 的坐标", *a.Index)
				}
				x, y, hasPos = res.X, res.Y, true
				how = fmt.Sprintf("元素 [%d] 中心 (%d,%d)", *a.Index, x, y)
			} else if a.X != nil || a.Y != nil {
				if a.X != nil {
					x = *a.X
				}
				if a.Y != nil {
					y = *a.Y
				}
				hasPos = true
				how = fmt.Sprintf("(%d,%d)", x, y)
			}
			if err := p.Scroll(ctx, x, y, hasPos, a.Direction, a.Amount); err != nil {
				return nil, err
			}
			return ctext(fmt.Sprintf("已在 %s 向 %s 滚动 %d 行", how, a.Direction, a.Amount)), nil
		},
	})

	s.Add(mcpserver.Tool{
		Name:        "computer_activate_app",
		Description: "启动/聚焦应用（未运行则拉起，已运行则置前）。操作某应用前如发现它不在前台且元素点击无效，先激活它。name 接受应用名或 bundle id（如 com.apple.Safari，跨语言最稳——本地化名\"访达\"也能命中，见 computer_list_apps）。",
		Schema:      cobj(map[string]any{"name": cstr("应用名或 bundle id，如 Safari、访达、com.apple.Safari")}, "name"),
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			var a struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			if err := p.ActivateApp(ctx, a.Name); err != nil {
				return nil, err
			}
			return ctext("已启动/聚焦 " + a.Name), nil
		},
	})

	s.Add(mcpserver.Tool{
		Name:        "computer_clipboard",
		Description: "读取（action=get）或写入（action=set，需 text）系统剪贴板。",
		Schema: cobj(map[string]any{
			"action": map[string]any{"type": "string", "enum": []string{"get", "set"}, "description": "读或写"},
			"text":   cstr("set 时的写入内容"),
		}, "action"),
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			var a struct {
				Action string `json:"action"`
				Text   string `json:"text"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			switch a.Action {
			case "get":
				out, err := p.ClipboardGet(ctx)
				if err != nil {
					return nil, err
				}
				if out == "" {
					return ctext("(剪贴板为空)"), nil
				}
				return ctext(out), nil
			case "set":
				if err := p.ClipboardSet(ctx, a.Text); err != nil {
					return nil, err
				}
				return ctext("已写入剪贴板"), nil
			default:
				return nil, fmt.Errorf("未知 action %q", a.Action)
			}
		},
	})

	s.Add(mcpserver.Tool{
		Name:        "computer_wait",
		Description: "等待指定秒数（≤10，用于动画/加载）。只读。",
		Schema:      cobj(map[string]any{"seconds": cnum("等待秒数（0.5~10）")}, "seconds"),
		ReadOnly:    true,
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			var a struct {
				Seconds float64 `json:"seconds"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			if a.Seconds <= 0 {
				a.Seconds = 1
			}
			if a.Seconds > 10 {
				a.Seconds = 10
			}
			select {
			case <-time.After(time.Duration(a.Seconds * float64(time.Second))):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return ctext(fmt.Sprintf("已等待 %.1f 秒", a.Seconds)), nil
		},
	})
}

// mergeProps 把 targetProps 的元素定位参数并进动作参数表（不覆盖同名键）。
func mergeProps(a map[string]any) map[string]any {
	for k, v := range targetProps {
		if _, ok := a[k]; !ok {
			a[k] = v
		}
	}
	return a
}

// resolveApp 把 computer_get_app_state 的 app 参数（名称/bundle id/pid/空）
// 解析成 pid 与应用名。空=前台应用；数字按 pid 匹配；否则按名称与 bundle
// id 模糊匹配（大小写不敏感），多命中要求更精确。
func resolveApp(ctx context.Context, p platform, app string) (int, string, string, error) {
	apps, err := p.Apps(ctx)
	if err != nil {
		return 0, "", "", err
	}
	if app == "" {
		for _, a := range apps {
			if a.Frontmost {
				return a.PID, a.Name, a.BundleID, nil
			}
		}
		return 0, "", "", fmt.Errorf("未找到前台应用，请用 computer_list_apps 查询后传 app 参数")
	}
	if pid, err := strconv.Atoi(app); err == nil {
		for _, a := range apps {
			if a.PID == pid {
				return a.PID, a.Name, a.BundleID, nil
			}
		}
		return 0, "", "", fmt.Errorf("pid=%d 的应用未在运行（computer_list_apps 查询）", pid)
	}
	lower := strings.ToLower(app)
	var hits []AppInfo
	for _, a := range apps {
		al := strings.ToLower(a.Name)
		bl := strings.ToLower(a.BundleID)
		if al == lower || (a.BundleID != "" && bl == lower) {
			return a.PID, a.Name, a.BundleID, nil
		}
		if strings.Contains(al, lower) || (a.BundleID != "" && strings.Contains(bl, lower)) {
			hits = append(hits, a)
		}
	}
	if len(hits) == 1 {
		return hits[0].PID, hits[0].Name, hits[0].BundleID, nil
	}
	if len(hits) > 1 {
		names := make([]string, len(hits))
		for i, h := range hits {
			names[i] = fmt.Sprintf("%s(pid=%d)", h.Name, h.PID)
		}
		return 0, "", "", fmt.Errorf("应用名 %q 匹配多个：%s；请传更精确的名称、bundle id 或 pid", app, strings.Join(names, ", "))
	}
	return 0, "", "", fmt.Errorf("未找到应用 %q（computer_list_apps 查询确切名称/bundle id，或先 computer_activate_app 启动）", app)
}

// verifySetValue 校验 set_value 的回读（darwin 平台层返回 "set:<回读值>"；
// Linux/Windows 暂无回读，返回裸 "set"）。一致（字符串或数值等价，容忍
// 首尾空白）→ verified=true；无回读或回读为空 → verified=false 不报错
// （密码框/仅写属性/平台未实现，无法确认只能如实告知）；非空且不符 →
// 报错：AX 接受但应用侧拒绝（滑杆钳制、终端回显缓冲）正是"假成功"的
// 实锤，必须让调用方知道。
func verifySetValue(want, tag string) (verified bool, err error) {
	got, hasReadback := strings.CutPrefix(tag, "set:")
	if !hasReadback {
		return false, nil // 平台未实现回读：不校验，仅如实标注无法确认
	}
	if got == want || strings.TrimSpace(got) == strings.TrimSpace(want) {
		return true, nil
	}
	if wf, e1 := strconv.ParseFloat(strings.TrimSpace(want), 64); e1 == nil {
		if gf, e2 := strconv.ParseFloat(strings.TrimSpace(got), 64); e2 == nil && wf == gf {
			return true, nil
		}
	}
	if got == "" {
		return false, nil
	}
	return false, fmt.Errorf("设置未生效：目标值 %q，回读仍为 %q（应用侧拒绝或钳制了该值）；终端类界面请改用 computer_type 聚焦+键入",
		trunc(want, 40), trunc(got, 40))
}

// terminalBundleIDs / terminalAppNames 终端类应用识别：这类应用的文本区
// 是回显缓冲（AX 设值只改显示、不进 PTY 输入），type 必须走键盘路径。
// 名称表含常见本地化变体（小写精确匹配）。
var terminalBundleIDs = map[string]bool{ // 键须全小写（查找前 ToLower）
	"com.apple.terminal": true, "com.googlecode.iterm2": true,
	"com.github.wez.wezterm": true, "net.kovidgoyal.kitty": true,
	"com.mitchellh.ghostty": true, "org.alacritty": true,
}

var terminalAppNames = map[string]bool{
	"terminal": true, "终端": true, "iterm2": true, "wezterm": true,
	"kitty": true, "ghostty": true, "alacritty": true, "xterm": true,
	"gnome-terminal": true, "gnome-terminal-server": true, "konsole": true,
	"windowsterminal": true, "windowsterminal.exe": true,
	"openconsole": true, "openconsole.exe": true, "conhost": true, "mintty": true,
}

// isTerminalApp 目标应用是否终端类（bundle id 优先，名称兜底）。
func isTerminalApp(name, bundle string) bool {
	if bundle != "" && terminalBundleIDs[strings.ToLower(bundle)] {
		return true
	}
	return terminalAppNames[strings.ToLower(strings.TrimSpace(name))]
}

// validateCoordSnapshot 坐标绑定快照校验（state_id 无 index + x/y）：坐标
// 应取自该快照的窗口。找到快照中包含该坐标的窗口层节点，与该应用当前
// 窗口帧比对——原点漂移超阈值（窗口被挪动/挤动）即报错，防旧坐标静默
// 落进其他应用（输入进无关窗口还可能触发其快捷键）。
func validateCoordSnapshot(ctx context.Context, p platform, a targetReq) error {
	x, y, err := a.resolveXY()
	if err != nil {
		return err
	}
	st, ok := states.get(a.StateID)
	if !ok {
		return fmt.Errorf("state_id=%s 不存在或已过期，请重新 computer_get_app_state 观察", a.StateID)
	}
	snap, ordinal, hit := snapWindowContaining(st.Nodes, x, y)
	if !hit {
		return fmt.Errorf("坐标 (%d,%d) 不在快照 %s 的任何窗口内；坐标须取自该快照，或改用元素模式 state_id+index", x, y, a.StateID)
	}
	wins, werr := p.AppWindows(ctx, st.PID)
	if werr != nil {
		return werr
	}
	if ordinal > len(wins) {
		return fmt.Errorf("快照对应的窗口已关闭（快照第 %d 个窗口，现仅 %d 个）；请重新观察取新坐标", ordinal, len(wins))
	}
	cur := wins[ordinal-1]
	if frameStale(snap, cur.X, cur.Y, 30) {
		return fmt.Errorf("窗口已移动：快照记录原点 (%d,%d)，现帧原点 (%d,%d)——旧坐标会点错目标（可能落进其他应用）；请重新 computer_get_app_state / computer_screenshot 取新坐标",
			snap.X, snap.Y, cur.X, cur.Y)
	}
	return nil
}

// snapWindowContaining 找快照中包含坐标 (x,y) 的窗口层节点（8px 容差），
// 返回节点与其窗口序号（1 起，仅数窗口层节点，对应平台窗口枚举序，
// AppWindows 按同序取现帧；无几何的窗口行同样占序号但不参与命中）。
func snapWindowContaining(nodes []*Node, x, y int) (*Node, int, bool) {
	win := 0
	for _, n := range nodes {
		if n.Depth != 0 {
			continue
		}
		win++
		if n.W <= 0 || n.H <= 0 {
			continue
		}
		if x >= n.X-8 && x <= n.X+n.W+8 && y >= n.Y-8 && y <= n.Y+n.H+8 {
			return n, win, true
		}
	}
	return nil, 0, false
}

// frameStale 快照窗口原点与现帧原点是否漂移超容差（尺寸变化不拦——
// 窗口右下缩放不改变左上角坐标命中语义；只防"整窗挪位"）。
func frameStale(snap *Node, curX, curY, tol int) bool {
	return absI(snap.X-curX) > tol || absI(snap.Y-curY) > tol
}

func absI(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
