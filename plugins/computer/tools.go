// tools.go grf-computer 工具面：截屏回传多模态图像，其余操作返回文本
// 结果。坐标均为全局虚拟桌面坐标（原点=主屏左上，多屏时副屏坐标可为负，
// 各屏边界见 computer_list_displays）。
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
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
		Name:        "computer_screenshot",
		Description: "截屏，PNG 图像直接回传（多模态），并附文字说明图像覆盖的全局坐标范围与像素倍率 scale。只截单个显示器：指定 display 编号截该屏，省略则截主屏（Retina 下图像为 scale 倍像素）。点击等坐标工具用全局坐标：图上目标像素位置 ÷ scale + 覆盖范围起点 即为全局坐标。操作前先截图确认现状。多屏注意：本工具一次只截一屏，要找的窗口/应用可能不在当前所截屏幕上——若图上找不到目标，先用 computer_list_displays 枚举各屏，再用 display 参数逐屏截图定位目标。",
		Schema: cobj(map[string]any{
			"display": cnum("显示器编号（computer_list_displays 的 index，可省略=主屏）"),
		}),
		ReadOnly: true,
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			var a struct {
				Display *int `json:"display"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			// 截图永远只截单屏（省略 display=主屏），截到的内容与该屏的
			// 全局坐标一致；连同实际图像尺寸/倍率一起回传，供模型把图上
			// 像素换算成全局坐标
			ds, err := p.Displays(ctx)
			if err != nil {
				return nil, err
			}
			d, err := pickDisplay(ds, a.Display)
			if err != nil {
				return nil, err
			}
			data, err := p.Screenshot(ctx, &d.Index)
			if err != nil {
				return nil, err
			}
			scale := 0
			if pw, ph, ok := pngSize(data); ok {
				if d.W > 0 && pw%d.W == 0 {
					scale = pw / d.W
				}
				meta := fmt.Sprintf("图像 %dx%d 像素；覆盖 display#%d（全局坐标 x=%d..%d, y=%d..%d）", pw, ph, d.Index, d.X, d.X+d.W, d.Y, d.Y+d.H)
				if scale > 1 {
					meta += fmt.Sprintf("；scale=%d，图上像素坐标 ÷ %d 再加起点 (%d,%d) 才是点击用的全局坐标", scale, scale, d.X, d.Y)
				} else {
					meta += fmt.Sprintf("；scale=1，图上像素坐标加起点 (%d,%d) 即点击用的全局坐标", d.X, d.Y)
				}
				return []mcpserver.Content{mcpserver.Text(meta), mcpserver.ImagePNG(base64.StdEncoding.EncodeToString(data))}, nil
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
		Name:        "computer_click",
		Description: "在全局坐标 (x,y) 点击鼠标（原点=主屏左上，多屏时副屏坐标可为负；可指定按键与连击次数）。",
		Schema: cobj(map[string]any{
			"x":      cnum("横坐标（像素，原点左上）"),
			"y":      cnum("纵坐标"),
			"button": map[string]any{"type": "string", "enum": []string{"left", "right", "middle"}, "description": "鼠标键（默认 left）"},
			"clicks": cnum("连击次数（2=双击，默认 1）"),
		}, "x", "y"),
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			var a struct {
				X      int    `json:"x"`
				Y      int    `json:"y"`
				Button string `json:"button"`
				Clicks int    `json:"clicks"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			if a.Clicks == 0 {
				a.Clicks = 1
			}
			if err := p.Click(ctx, a.X, a.Y, a.Button, a.Clicks); err != nil {
				return nil, err
			}
			return ctext(fmt.Sprintf("已在 (%d,%d) 点击 %s ×%d", a.X, a.Y, a.Button, a.Clicks)), nil
		},
	})

	s.Add(mcpserver.Tool{
		Name:        "computer_move",
		Description: "移动鼠标指针到全局坐标（不点击；多屏坐标约定见 computer_list_displays）。",
		Schema:      cobj(map[string]any{"x": cnum("横坐标"), "y": cnum("纵坐标")}, "x", "y"),
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			var a struct {
				X int `json:"x"`
				Y int `json:"y"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			if err := p.Move(ctx, a.X, a.Y); err != nil {
				return nil, err
			}
			return ctext(fmt.Sprintf("指针已移至 (%d,%d)", a.X, a.Y)), nil
		},
	})

	s.Add(mcpserver.Tool{
		Name:        "computer_drag",
		Description: "按住左键从 (from_x,from_y) 平滑拖拽到 (to_x,to_y) 后松开。",
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
		Description: "在 (x,y)（可省略=当前位置）滚动滚轮：direction up/down，amount 行数。",
		Schema: cobj(map[string]any{
			"x":         cnum("横坐标（可省略）"),
			"y":         cnum("纵坐标（可省略）"),
			"direction": map[string]any{"type": "string", "enum": []string{"up", "down"}, "description": "滚动方向（默认 down）"},
			"amount":    cnum("滚动行数（默认 3）"),
		}),
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			var a struct {
				X         *int   `json:"x"`
				Y         *int   `json:"y"`
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
			// 指针区分"显式给了坐标（含 0）"与"省略"，(0,0) 不再被误当省略
			hasPos := a.X != nil || a.Y != nil
			x, y := 0, 0
			if a.X != nil {
				x = *a.X
			}
			if a.Y != nil {
				y = *a.Y
			}
			if err := p.Scroll(ctx, x, y, hasPos, a.Direction, a.Amount); err != nil {
				return nil, err
			}
			pos := "当前位置"
			if hasPos {
				pos = fmt.Sprintf("(%d,%d)", x, y)
			}
			return ctext(fmt.Sprintf("已在 %s 向 %s 滚动 %d 行", pos, a.Direction, a.Amount)), nil
		},
	})

	s.Add(mcpserver.Tool{
		Name:        "computer_type",
		Description: "向当前焦点窗口输入文本（纯 ASCII 走按键模拟；含中文等非 ASCII 时自动改经剪贴板粘贴并恢复剪贴板，不受输入法影响；换行用 \\n 在多数输入框等效回车，可靠起见用 computer_key 发送）。",
		Schema:      cobj(map[string]any{"text": cstr("要输入的文本")}, "text"),
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			var a struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			if err := p.Type(ctx, a.Text); err != nil {
				return nil, err
			}
			return ctext(fmt.Sprintf("已输入 %d 字符", len([]rune(a.Text)))), nil
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
