// tools.go grf-computer 工具面：截屏回传多模态图像，其余操作返回文本
// 结果。坐标均为屏幕绝对坐标（原点左上）。
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
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
		Name:        "computer_screenshot",
		Description: "截取整块屏幕，PNG 图像直接回传（多模态）。操作前先截图确认现状。",
		Schema:      cobj(map[string]any{}),
		ReadOnly:    true,
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			data, err := p.Screenshot(ctx)
			if err != nil {
				return nil, err
			}
			return []mcpserver.Content{mcpserver.ImagePNG(base64.StdEncoding.EncodeToString(data))}, nil
		},
	})

	s.Add(mcpserver.Tool{
		Name:        "computer_screen_size",
		Description: "查询主屏分辨率（宽高像素）。截图/点击前可用来换算坐标。",
		Schema:      cobj(map[string]any{}),
		ReadOnly:    true,
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			w, h, err := p.ScreenSize(ctx)
			if err != nil {
				return nil, err
			}
			return ctext(fmt.Sprintf("%d x %d", w, h)), nil
		},
	})

	s.Add(mcpserver.Tool{
		Name:        "computer_click",
		Description: "在屏幕绝对坐标 (x,y) 点击鼠标（可指定按键与连击次数）。",
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
		Description: "移动鼠标指针到屏幕绝对坐标（不点击）。",
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
