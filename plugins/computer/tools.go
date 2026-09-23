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

// resolveElement 取元素模式的目标（快照 + 节点）；非元素模式返回 nil。
func resolveElement(a targetReq) (*State, *Node, error) {
	if a.StateID == "" {
		return nil, nil, nil
	}
	if a.Index == nil {
		return nil, nil, fmt.Errorf("给了 state_id 就必须给 index（元素在 state 树中的索引）")
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
		Description: "列出运行中的 GUI 应用（pid、名称、是否前台）。用于选定目标应用后 computer_get_app_state / computer_activate_app。",
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
				fmt.Fprintf(&b, "pid=%d %q%s\n", a.PID, a.Name, mark)
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
			"app 省略=前台应用；可传应用名或 pid（见 computer_list_apps）。界面变化后旧 state_id 会失效，需重新观察。",
		Schema: cobj(map[string]any{
			"app":       cstr("应用名或 pid（省略=前台应用）"),
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
			pid, name, err := resolveApp(ctx, p, a.App)
			if err != nil {
				return nil, err
			}
			nodes, err := p.DumpElements(ctx, pid, a.MaxDepth)
			if err != nil {
				return nil, err
			}
			st := states.put(pid, name, nodes)
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
			"元素签名不符会报「界面已变化」，此时重新 computer_get_app_state。button=right/middle 或双击自动退化为元素中心坐标点击。",
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
		Description: "输入文本。优先元素模式（state_id+index）：先尝试可达性 API 直设值（不依赖焦点与输入法）；元素不支持下直设（Electron/浏览器等）时自动兜底——激活应用→点击输入框聚焦→三击全选→键入（整体替换原内容）。省略元素则向当前焦点输入（注意：键盘输入只落进前台应用，务必先点击目标输入框）。",
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
				if _, err := p.ElementAction(ctx, st.PID, n.Path, n.Role, n.Name, "set_value", a.Text); err == nil {
					return ctext(fmt.Sprintf("已向元素 [%d] %s 设值 %d 字符", *a.Index, n.Role, len([]rune(a.Text)))), nil
				} else if _, changed := err.(*errChanged); changed {
					return nil, err
				}
				// 元素不支持 AXSetValue（Electron/浏览器等常见）：聚焦输入框后
				// 走真实键盘输入兜底。不聚焦就打字是"输入不了"的头号根因——
				// keystroke 只落进前台应用，而 agent 跑在终端里，焦点常常就
				// 在终端自己身上。
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
				// 三击=全选输入框原内容，随后的输入整体替换（与 set_value 语义一致）
				if err := p.Click(ctx, res.X, res.Y, "left", 3); err != nil {
					return nil, err
				}
				time.Sleep(200 * time.Millisecond)
				if err := p.Type(ctx, a.Text); err != nil {
					return nil, err
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
		Description: "设置元素的值（state_id+index 定位，可达性 API 直设：文本框内容、勾选框开关、滑杆数值等；对不可设值元素会报错，改用 computer_click）。",
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
			if _, err := p.ElementAction(ctx, st.PID, n.Path, n.Role, n.Name, "set_value", a.Value); err != nil {
				return nil, err
			}
			return ctext(fmt.Sprintf("已设置元素 [%d] %s 的值为 %q", *a.Index, n.Role, trunc(a.Value, 40))), nil
		},
	})

	s.Add(mcpserver.Tool{
		Name:        "computer_key",
		Description: "向当前焦点窗口发送按键/组合键：enter、esc、tab、pageup、f1 等，或 ctrl+c、cmd+shift+t（Win 平台不支持 Win 键）。",
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
		Description: "启动/聚焦应用（未运行则拉起，已运行则置前）。操作某应用前如发现它不在前台且元素点击无效，先激活它。name 见 computer_list_apps。",
		Schema:      cobj(map[string]any{"name": cstr("应用名，如 Safari、访达")}, "name"),
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

// resolveApp 把 computer_get_app_state 的 app 参数（名称/pid/空）解析成
// pid 与应用名。空=前台应用；数字按 pid 匹配；否则按名称模糊匹配（大小写
// 不敏感），多命中要求更精确。
func resolveApp(ctx context.Context, p platform, app string) (int, string, error) {
	apps, err := p.Apps(ctx)
	if err != nil {
		return 0, "", err
	}
	if app == "" {
		for _, a := range apps {
			if a.Frontmost {
				return a.PID, a.Name, nil
			}
		}
		return 0, "", fmt.Errorf("未找到前台应用，请用 computer_list_apps 查询后传 app 参数")
	}
	if pid, err := strconv.Atoi(app); err == nil {
		for _, a := range apps {
			if a.PID == pid {
				return a.PID, a.Name, nil
			}
		}
		return 0, "", fmt.Errorf("pid=%d 的应用未在运行（computer_list_apps 查询）", pid)
	}
	lower := strings.ToLower(app)
	var hits []AppInfo
	for _, a := range apps {
		if strings.ToLower(a.Name) == lower {
			return a.PID, a.Name, nil
		}
		if strings.Contains(strings.ToLower(a.Name), lower) {
			hits = append(hits, a)
		}
	}
	if len(hits) == 1 {
		return hits[0].PID, hits[0].Name, nil
	}
	if len(hits) > 1 {
		names := make([]string, len(hits))
		for i, h := range hits {
			names[i] = fmt.Sprintf("%s(pid=%d)", h.Name, h.PID)
		}
		return 0, "", fmt.Errorf("应用名 %q 匹配多个：%s；请传更精确的名称或 pid", app, strings.Join(names, ", "))
	}
	return 0, "", fmt.Errorf("未找到应用 %q（computer_list_apps 查询确切名称，或先 computer_activate_app 启动）", app)
}
