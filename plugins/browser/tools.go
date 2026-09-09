// tools.go grf-browser 工具面（对齐 playwright-mcp 的工作流：先
// browser_snapshot 拿 ref，再按 ref 点击/输入）。所有处理器经
// b.lock() 串行化——同一浏览器实例的动作不允许并发执行。
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp/kb"
	"git.gorun.fun/share/grf-harness/plugins/internal/mcpserver"
)

// ---- JSON Schema 小构造器 ----

func obj(props map[string]any, required ...string) map[string]any {
	r := []any{}
	for _, n := range required {
		r = append(r, n)
	}
	return map[string]any{"type": "object", "properties": props, "required": r}
}

func pstr(desc string) map[string]any      { return map[string]any{"type": "string", "description": desc} }
func pbool(desc string) map[string]any     { return map[string]any{"type": "boolean", "description": desc} }
func pnum(desc string) map[string]any      { return map[string]any{"type": "number", "description": desc} }
func parr(item any) map[string]any         { return map[string]any{"type": "array", "items": item} }
func content(s string) []mcpserver.Content { return []mcpserver.Content{mcpserver.Text(s)} }

const refHelp = "browser_snapshot 输出里的元素引用（如 e12）"

// registerBrowserTools 注册全部 browser_* 工具。
func registerBrowserTools(s *mcpserver.Server, b *browser) {
	s.Add(mcpserver.Tool{
		Name: "browser_navigate",
		Description: "浏览器打开指定 URL（当前标签页导航）。返回页面标题与最终 URL。" +
			"后续操作建议先 browser_snapshot 获取可交互元素引用。" +
			"visible=true 时以可见窗口运行浏览器（用户明确要求看到浏览器界面时使用；" +
			"仅在浏览器尚未启动时生效，已启动需先 browser_close）。",
		Schema: obj(map[string]any{
			"url":     pstr("目标 URL（含 http:// 或 https://）"),
			"visible": pbool("以可见窗口打开浏览器（默认无头；仅首次启动前有效）"),
		}, "url"),
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			var a struct {
				URL     string `json:"url"`
				Visible bool   `json:"visible"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			u, err := url.Parse(strings.TrimSpace(a.URL))
			if err != nil || u.Scheme == "" {
				return nil, fmt.Errorf("非法 URL %q", a.URL)
			}
			switch u.Scheme {
			case "http", "https", "file", "about", "data":
			default:
				return nil, fmt.Errorf("不支持的协议 %q（允许 http/https/file/about/data）", u.Scheme)
			}
			if a.Visible {
				b.setVisible() // 须在 lock() 懒启动浏览器之前生效
			}
			unlock, err := b.lock()
			if err != nil {
				return nil, err
			}
			defer unlock()
			tab := b.activeTab()
			if err := chromedp.Run(tab.ctx, chromedp.Navigate(strings.TrimSpace(a.URL))); err != nil {
				return nil, err
			}
			var title, loc string
			_ = chromedp.Run(tab.ctx, chromedp.Title(&title), chromedp.Location(&loc))
			return content(fmt.Sprintf("已打开: %s\n%s", title, loc)), nil
		},
	})

	for _, t := range []struct {
		name, desc string
		action     func(ctx context.Context, tab *btab) error
	}{
		{"browser_back", "浏览器后退（当前标签页历史返回一页）。", func(ctx context.Context, tab *btab) error {
			return chromedp.Run(tab.ctx, chromedp.NavigateBack())
		}},
		{"browser_forward", "浏览器前进。", func(ctx context.Context, tab *btab) error {
			return chromedp.Run(tab.ctx, chromedp.NavigateForward())
		}},
	} {
		tt := t
		s.Add(mcpserver.Tool{
			Name: tt.name, Description: tt.desc, ReadOnly: true,
			Schema: obj(map[string]any{}),
			Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
				unlock, err := b.lock()
				if err != nil {
					return nil, err
				}
				defer unlock()
				if err := tt.action(ctx, b.activeTab()); err != nil {
					return nil, err
				}
				return content("完成。建议 browser_snapshot 查看新页面"), nil
			},
		})
	}

	s.Add(mcpserver.Tool{
		Name:        "browser_reload",
		Description: "刷新当前页面。",
		Schema:      obj(map[string]any{}),
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			unlock, err := b.lock()
			if err != nil {
				return nil, err
			}
			defer unlock()
			if err := b.runEval("location.reload()", nil); err != nil {
				return nil, err
			}
			return content("已刷新"), nil
		},
	})

	s.Add(mcpserver.Tool{
		Name: "browser_snapshot",
		Description: "抓取页面大纲（角色/名称/[ref=…] 与主要文本块），交互元素打上快照引用，" +
			"遍历穿透 shadow DOM 与同源 iframe。browser_click/type/hover/select_option/scroll " +
			"都以这里的 ref 定位；页面变化后需重拍。纯文本阅读用 browser_get_text 更省。",
		Schema:   obj(map[string]any{}),
		ReadOnly: true,
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			unlock, err := b.lock()
			if err != nil {
				return nil, err
			}
			defer unlock()
			var snap string
			if err := b.runEval(snapshotJS, &snap); err != nil {
				return nil, err
			}
			var loc, title string
			tab := b.activeTab()
			_ = chromedp.Run(tab.ctx, chromedp.Title(&title), chromedp.Location(&loc))
			return content("页面: " + title + "\nURL: " + loc + "\n\n" + snap), nil
		},
	})

	s.Add(mcpserver.Tool{
		Name: "browser_click",
		Description: "点击快照引用的元素（自动滚动到可见，发送真实鼠标事件 mousedown/mouseup，" +
			"isTrusted 为真，反爬与依赖真实事件序列的页面可用）。页面随后可能变化，建议重新 snapshot。",
		Schema: obj(map[string]any{
			"ref":          pstr(refHelp),
			"selector":     pstr("备用：直接 CSS 选择器（ref 缺失或失效时用）"),
			"double_click": pbool("双击（默认单击）"),
		}),
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			var a struct {
				Ref         string `json:"ref"`
				Selector    string `json:"selector"`
				DoubleClick bool   `json:"double_click"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			if a.Ref == "" && a.Selector == "" {
				return nil, fmt.Errorf("ref 与 selector 至少提供一个")
			}
			sel := a.Selector
			if a.Ref != "" {
				sel = refSelector(a.Ref)
			}
			unlock, err := b.lock()
			if err != nil {
				return nil, err
			}
			defer unlock()
			if err := b.mouseClick(sel, a.DoubleClick); err != nil {
				return nil, err
			}
			return content("已点击 " + sel), nil
		},
	})

	s.Add(mcpserver.Tool{
		Name: "browser_type",
		Description: "向快照引用的输入框填入文本（替换原值并触发 input/change 事件）；" +
			"submit=true 时随后按回车提交。",
		Schema: obj(map[string]any{
			"ref":      pstr(refHelp),
			"selector": pstr("备用：直接 CSS 选择器"),
			"text":     pstr("要输入的文本"),
			"submit":   pbool("输入后按回车"),
		}, "text"),
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			var a struct {
				Ref      string `json:"ref"`
				Selector string `json:"selector"`
				Text     string `json:"text"`
				Submit   bool   `json:"submit"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			sel := a.Selector
			if a.Ref != "" {
				sel = refSelector(a.Ref)
			}
			if sel == "" {
				return nil, fmt.Errorf("ref 与 selector 至少提供一个")
			}
			unlock, err := b.lock()
			if err != nil {
				return nil, err
			}
			defer unlock()
			expr := fmt.Sprintf(`((sel, text) => {
				%s
				const el = dq(sel);
				if (!el) return {ok: false, error: '元素不存在: ' + sel};
				el.scrollIntoView({block: 'center'});
				el.focus();
				const proto = el.tagName === 'TEXTAREA' ? window.HTMLTextAreaElement.prototype : window.HTMLInputElement.prototype;
				const desc = Object.getOwnPropertyDescriptor(proto, 'value');
				if (desc && desc.set) desc.set.call(el, text); else el.value = text;
				el.dispatchEvent(new Event('input', {bubbles: true}));
				el.dispatchEvent(new Event('change', {bubbles: true}));
					return {ok: true};
				})(%s, %s)`, dqHelper, jsString(sel), jsString(a.Text))
			var res map[string]any
			if err := b.runEval(expr, &res); err != nil {
				return nil, err
			}
			if res["ok"] != true {
				msg, _ := res["error"].(string)
				return content(msg + "；可重新 browser_snapshot 获取新引用"), nil
			}
			if a.Submit {
				if err := chromedp.Run(b.activeTab().ctx, chromedp.KeyEvent(kb.Enter)); err != nil {
					return nil, err
				}
			}
			return content("已输入到 " + sel), nil
		},
	})

	s.Add(mcpserver.Tool{
		Name:        "browser_press_key",
		Description: "向当前聚焦元素发送按键。支持单键（enter/esc/tab/arrowdown/pageup 等）与组合（ctrl+c、cmd+t、shift+arrowdown）。",
		Schema:      obj(map[string]any{"key": pstr("按键或组合，如 Enter、Escape、ctrl+a、cmd+shift+t")}, "key"),
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			var a struct {
				Key string `json:"key"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			unlock, err := b.lock()
			if err != nil {
				return nil, err
			}
			defer unlock()
			if err := b.pressCombo(a.Key); err != nil {
				return nil, err
			}
			return content("已按键 " + a.Key), nil
		},
	})

	s.Add(mcpserver.Tool{
		Name:        "browser_hover",
		Description: "把鼠标悬停到快照引用的元素上（先移出再移入，可靠触发依赖 mouseleave/mouseenter 边沿的 hover 菜单/提示）。",
		Schema:      obj(map[string]any{"ref": pstr(refHelp)}, "ref"),
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			var a struct {
				Ref string `json:"ref"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			unlock, err := b.lock()
			if err != nil {
				return nil, err
			}
			defer unlock()
			sel := refSelector(a.Ref)
			// 先滚到可见再取中心，否则坐标可能在视口外
			_ = b.runEval(fmt.Sprintf(`(%s; dq(%s) && dq(%s).scrollIntoView({block: 'center'}))`, dqHelper, jsString(sel), jsString(sel)), nil)
			x, y, err := b.deepCenter(sel)
			if err != nil {
				return nil, err
			}
			if err := chromedp.Run(b.activeTab().ctx,
				chromedp.MouseEvent(input.MouseMoved, 1, 1),
				chromedp.MouseEvent(input.MouseMoved, x, y)); err != nil {
				return nil, err
			}
			return content(fmt.Sprintf("已悬停 %s (%.0f,%.0f)", a.Ref, x, y)), nil
		},
	})

	s.Add(mcpserver.Tool{
		Name:        "browser_select_option",
		Description: "在快照引用的下拉框中按 value 或可见文本选中选项。",
		Schema: obj(map[string]any{
			"ref":    pstr(refHelp),
			"values": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "选项 value 或文本列表"},
		}, "ref", "values"),
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			var a struct {
				Ref    string   `json:"ref"`
				Values []string `json:"values"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			unlock, err := b.lock()
			if err != nil {
				return nil, err
			}
			defer unlock()
			vals, _ := json.Marshal(a.Values)
			expr := fmt.Sprintf(`((sel, values) => {
				%s
				const el = dq(sel);
				if (!el || el.tagName !== 'SELECT') return {ok: false, matched: []};
				const matched = [];
				for (const v of values) {
					const opt = [...el.options].find(o => o.value === v || o.text.trim() === v);
					if (opt) { opt.selected = true; matched.push(v); }
				}
				el.dispatchEvent(new Event('input', {bubbles: true}));
				el.dispatchEvent(new Event('change', {bubbles: true}));
				return {ok: matched.length > 0, matched};
			})(%s, %s)`, dqHelper, jsString(refSelector(a.Ref)), string(vals))
			var res map[string]any
			if err := b.runEval(expr, &res); err != nil {
				return nil, err
			}
			matched, _ := res["matched"].([]any)
			if res["ok"] != true {
				return nil, fmt.Errorf("选中失败：元素不是 SELECT 或无匹配选项（已选: %v）", matched)
			}
			if len(matched) < len(a.Values) {
				return content(fmt.Sprintf("部分选中（%d/%d）: %v；未匹配项请检查取值", len(matched), len(a.Values), matched)), nil
			}
			return content("已选中: " + fmt.Sprint(matched)), nil
		},
	})

	s.Add(mcpserver.Tool{
		Name:        "browser_evaluate",
		Description: "在当前页面执行任意 JavaScript 并返回结果（JSON 序列化）。用于精查 DOM、读取变量等。",
		Schema:      obj(map[string]any{"expression": pstr("JS 表达式（在页面上下文求值）")}, "expression"),
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			var a struct {
				Expression string `json:"expression"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			unlock, err := b.lock()
			if err != nil {
				return nil, err
			}
			defer unlock()
			var res any
			if err := b.runEval(a.Expression, &res); err != nil {
				return nil, err
			}
			if res == nil {
				return content("undefined"), nil
			}
			out, err := json.MarshalIndent(res, "", "  ")
			if err != nil {
				return content(fmt.Sprint(res)), nil
			}
			return content(string(out)), nil
		},
	})

	s.Add(mcpserver.Tool{
		Name: "browser_take_screenshot",
		Description: "截取当前页面视口（或整页）PNG。缺省把图片直接回传（多模态）；" +
			"给 file_path 则保存为文件并返回路径。",
		Schema: obj(map[string]any{
			"full_page": pbool("整页截图（默认仅视口）"),
			"file_path": pstr("保存路径（相对路径相对工作目录；缺省直接回传图片）"),
		}),
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			var a struct {
				FullPage bool   `json:"full_page"`
				FilePath string `json:"file_path"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			unlock, err := b.lock()
			if err != nil {
				return nil, err
			}
			defer unlock()
			var buf []byte
			if a.FullPage {
				err = chromedp.Run(b.activeTab().ctx, chromedp.FullScreenshot(&buf, 90))
			} else {
				err = chromedp.Run(b.activeTab().ctx, chromedp.CaptureScreenshot(&buf))
			}
			if err != nil {
				return nil, err
			}
			if a.FilePath != "" {
				path := a.FilePath
				if !filepath.IsAbs(path) {
					wd, _ := os.Getwd()
					path = filepath.Join(wd, path)
				}
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					return nil, err
				}
				if err := os.WriteFile(path, buf, 0o644); err != nil {
					return nil, err
				}
				return content(fmt.Sprintf("截图已保存: %s（%d KB）", path, len(buf)/1024)), nil
			}
			return []mcpserver.Content{mcpserver.ImagePNG(base64.StdEncoding.EncodeToString(buf))}, nil
		},
	})

	s.Add(mcpserver.Tool{
		Name:        "browser_get_text",
		Description: "读取当前页面可见文本（innerText，截断到 2 万字符）。只读、比快照更省。",
		Schema:      obj(map[string]any{}),
		ReadOnly:    true,
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			unlock, err := b.lock()
			if err != nil {
				return nil, err
			}
			defer unlock()
			var text string
			// 点击触发的导航没有统一的 load 等待点：读到的文本为空或
			// 仍在变化（渐进渲染的中间态）时稍候重试，取稳定值
			prev := ""
			for i := 0; i < 4; i++ {
				if err := b.runEval("document.body ? document.body.innerText : ''", &text); err != nil {
					return nil, err
				}
				if strings.TrimSpace(text) != "" && text == prev {
					break
				}
				prev = text
				select {
				case <-time.After(400 * time.Millisecond):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			text = prev
			limit := 20000
			if len(text) > limit {
				for limit > 0 && !utf8.ValidString(text[:limit]) {
					limit-- // 回退到 rune 边界，避免截断多字节字符产生乱码
				}
				text = text[:limit] + "\n…（截断，可用 browser_evaluate 精查）"
			}
			return content(text), nil
		},
	})

	s.Add(mcpserver.Tool{
		Name: "browser_console_messages",
		Description: "回放当前标签页累计的控制台输出（console.* 与页面异常，含自动应答的 [dialog] 对话框与 [download] 下载事件）。",
		Schema:      obj(map[string]any{}),
		ReadOnly:    true,
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			unlock, err := b.lock()
			if err != nil {
				return nil, err
			}
			defer unlock()
			tab := b.activeTab()
			tab.mu.Lock()
			msgs := append([]string(nil), tab.console...)
			tab.mu.Unlock()
			if len(msgs) == 0 {
				return content("(控制台无输出)"), nil
			}
			return content(strings.Join(msgs, "\n")), nil
		},
	})

	s.Add(mcpserver.Tool{
		Name:        "browser_network_requests",
		Description: "回放当前标签页捕获的网络请求（方法 URL -> 状态码，含资源类型/耗时/大小/Content-Type），limit 从最新往回取。",
		Schema:      obj(map[string]any{"limit": pnum("最多返回条数（默认 50）")}),
		ReadOnly:    true,
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			var a struct {
				Limit int `json:"limit"`
			}
			_ = json.Unmarshal(args, &a)
			if a.Limit <= 0 {
				a.Limit = 50
			}
			unlock, err := b.lock()
			if err != nil {
				return nil, err
			}
			defer unlock()
			tab := b.activeTab()
			tab.mu.Lock()
			nets := append([]string(nil), tab.nets...)
			tab.mu.Unlock()
			if len(nets) > a.Limit {
				nets = nets[len(nets)-a.Limit:]
			}
			if len(nets) == 0 {
				return content("(无网络请求记录)"), nil
			}
			return content(strings.Join(nets, "\n")), nil
		},
	})

	s.Add(mcpserver.Tool{
		Name:        "browser_tabs",
		Description: "标签页管理：list 列出全部 / new 新开（可带 url 并置为活跃）/ select 切换 / close 关闭。",
		Schema: obj(map[string]any{
			"action": map[string]any{"type": "string", "enum": []string{"list", "new", "select", "close"}, "description": "操作"},
			"index":  pnum("select/close 的标签页序号（list 输出的序号；close 缺省关活跃页）"),
			"url":    pstr("new 时的初始 URL（缺省 about:blank）"),
		}, "action"),
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			var a struct {
				Action string   `json:"action"`
				Index  *float64 `json:"index"`
				URL    string   `json:"url"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			unlock, err := b.lock()
			if err != nil {
				return nil, err
			}
			defer unlock()
			switch a.Action {
			case "list":
				var lines []string
				for i, tab := range b.tabs {
					var title, loc string
					_ = chromedp.Run(tab.ctx, chromedp.Title(&title), chromedp.Location(&loc))
					mark := " "
					if i == b.active {
						mark = "*"
					}
					lines = append(lines, fmt.Sprintf("%s[%d] %s  %s", mark, i, title, loc))
				}
				return content(strings.Join(lines, "\n")), nil
			case "new":
				u := a.URL
				if u == "" {
					u = "about:blank"
				}
				if err := b.openTabLocked(u); err != nil {
					return nil, err
				}
				b.activateLocked(b.active) // 新标签页置前（与真实浏览器行为一致）
				return content(fmt.Sprintf("已新开标签页 #%d", b.active)), nil
			case "select":
				if a.Index == nil {
					return nil, fmt.Errorf("select 需要 index")
				}
				idx := int(*a.Index)
				if idx < 0 || idx >= len(b.tabs) {
					return nil, fmt.Errorf("标签页序号 %d 越界（共 %d 个）", idx, len(b.tabs))
				}
				b.active = idx
				b.activateLocked(idx) // 只改指针不激活 target，页面不会真正切换
				return content(fmt.Sprintf("已切换到标签页 #%d", idx)), nil
			case "close":
				idx := b.active
				if a.Index != nil {
					idx = int(*a.Index)
				}
				if idx < 0 || idx >= len(b.tabs) {
					return nil, fmt.Errorf("标签页序号 %d 越界（共 %d 个）", idx, len(b.tabs))
				}
				b.closeTabLocked(idx) // 墓碑 + 带超时的同步关闭，防 syncTabs 误接管卡死
				switch {
				case len(b.tabs) == 0:
					if err := b.openTabLocked("about:blank"); err != nil {
						return nil, err
					}
				case idx < b.active:
					b.active-- // 关闭的是活跃页左侧的标签，剩余左移一位须同步收紧指针
				case b.active >= len(b.tabs):
					b.active = len(b.tabs) - 1
				}
				return content(fmt.Sprintf("已关闭标签页 #%d", idx)), nil
			default:
				return nil, fmt.Errorf("未知 action %q", a.Action)
			}
		},
	})

	s.Add(mcpserver.Tool{
		Name:        "browser_close",
		Description: "关闭整个浏览器（所有标签页与浏览器进程）。下次调用任一 browser_* 工具会自动重启。",
		Schema:      obj(map[string]any{}),
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			unlock, err := b.lock()
			if err != nil {
				return nil, err
			}
			defer unlock()
			b.closeLocked()
			return content("浏览器已关闭"), nil
		},
	})

	s.Add(mcpserver.Tool{
		Name: "browser_scroll",
		Description: "滚动页面（up/down/left/right，按像素）或把快照引用的元素滚到视口中央。" +
			"方向滚动派发真实滚轮事件到视口中心——内层滚动容器（body 不可滚的应用布局）" +
			"由浏览器自行路由到实际可滚的元素，懒加载监听正常触发。",
		Schema: obj(map[string]any{
			"direction": map[string]any{"type": "string", "enum": []string{"up", "down", "left", "right"}, "description": "滚动方向（缺省 down）"},
			"amount":    pnum("滚动像素（缺省 600）"),
			"ref":       pstr("给出则改为把该元素滚到视口中央（优先于方向/像素）"),
		}),
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			var a struct {
				Direction string  `json:"direction"`
				Amount    float64 `json:"amount"`
				Ref       string  `json:"ref"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			if a.Amount <= 0 {
				a.Amount = 600
			}
			unlock, err := b.lock()
			if err != nil {
				return nil, err
			}
			defer unlock()
			if a.Ref != "" {
				expr := fmt.Sprintf(`(() => {
					%s
					const el = dq(%s);
					if (!el) return {ok: false};
					el.scrollIntoView({block: 'center'});
					return {ok: true};
				})()`, dqHelper, jsString(refSelector(a.Ref)))
				var res map[string]any
				if err := b.runEval(expr, &res); err != nil {
					return nil, err
				}
				if res["ok"] != true {
					return content("元素不在当前快照中；可重新 browser_snapshot 获取新引用"), nil
				}
				return content("已把 " + a.Ref + " 滚动到视口中央"), nil
			}
			if a.Direction == "" {
				a.Direction = "down"
			}
			var dx, dy float64
			switch a.Direction {
			case "up":
				dy = -a.Amount
			case "down":
				dy = a.Amount
			case "left":
				dx = -a.Amount
			case "right":
				dx = a.Amount
			default:
				return nil, fmt.Errorf("未知方向 %q", a.Direction)
			}
			// 真实滚轮事件派发到视口中心：window.scrollBy 只滚主文档，
			// body 固定 100vh 的应用布局（内层容器滚动）会完全不动。
			var vp struct {
				W, H float64
			}
			if err := b.runEval(`({W: window.innerWidth, H: window.innerHeight})`, &vp); err != nil {
				return nil, err
			}
			if err := chromedp.Run(b.activeTab().ctx, chromedp.ActionFunc(func(c context.Context) error {
				return input.DispatchMouseEvent(input.MouseWheel, vp.W/2, vp.H/2).
					WithDeltaX(dx).WithDeltaY(dy).Do(c)
			})); err != nil {
				return nil, err
			}
			// 滚动落点可能在主文档也可能在内层容器，两者都回报
			// （wheel 的滚动应用是异步的，稍候再读，否则读到旧值）
			select {
			case <-time.After(150 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			var pos map[string]any
			_ = b.runEval(`(() => {
				let inner = 0;
				for (const el of document.querySelectorAll('*')) {
					const cs = getComputedStyle(el);
					if (/auto|scroll/.test(cs.overflowY) && el.scrollTop > inner) inner = el.scrollTop;
				}
				return {win: window.scrollY, inner};
			})()`, &pos)
			return content(fmt.Sprintf("已滚动 %s %.0fpx；窗口滚动位置 %v，内层容器滚动位置 %v",
				a.Direction, a.Amount, pos["win"], pos["inner"])), nil
		},
	})

	s.Add(mcpserver.Tool{
		Name: "browser_wait_for",
		Description: "等待页面出现指定文本（body.innerText 包含）或 URL 变为包含指定子串，" +
			"用于点击后的异步跳转/渲染。轮询间隔 250ms，超时报错。",
		Schema: obj(map[string]any{
			"text":       pstr("等待出现的文本（与 url 至少给一个）"),
			"url":        pstr("等待 URL 包含的子串"),
			"timeout_ms": pnum("超时毫秒（缺省 10000）"),
		}),
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			var a struct {
				Text      string  `json:"text"`
				URL       string  `json:"url"`
				TimeoutMs float64 `json:"timeout_ms"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			if a.Text == "" && a.URL == "" {
				return nil, fmt.Errorf("text 与 url 至少提供一个")
			}
			if a.TimeoutMs <= 0 {
				a.TimeoutMs = 10000
			}
			unlock, err := b.lock()
			if err != nil {
				return nil, err
			}
			defer unlock()
			deadline := time.Now().Add(time.Duration(a.TimeoutMs * float64(time.Millisecond)))
			expr := fmt.Sprintf(`(() => {
				const okText = %s === '' || (document.body && document.body.innerText.includes(%s));
				const okUrl = %s === '' || location.href.includes(%s);
				return {ok: okText && okUrl, url: location.href};
			})()`, jsString(a.Text), jsString(a.Text), jsString(a.URL), jsString(a.URL))
			for {
				var res map[string]any
				if err := b.runEval(expr, &res); err != nil {
					return nil, err
				}
				if res["ok"] == true {
					return content("条件满足: " + fmt.Sprint(res["url"])), nil
				}
				if time.Now().After(deadline) {
					return nil, fmt.Errorf("等待超时（%dms），当前 URL: %v", int(a.TimeoutMs), res["url"])
				}
				select {
				case <-time.After(250 * time.Millisecond):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
		},
	})

	s.Add(mcpserver.Tool{
		Name: "browser_file_upload",
		Description: "向 input[type=file] 设置要上传的本地文件（先点开上传控件后调用，" +
			"或直接用 ref/selector 指定输入框）。文件路径相对插件工作目录。",
		Schema: obj(map[string]any{
			"paths":    parr(map[string]any{"type": "string"}),
			"ref":      pstr(refHelp),
			"selector": pstr("备用：直接 CSS 选择器（缺省 input[type=file]）"),
		}, "paths"),
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			var a struct {
				Paths    []string `json:"paths"`
				Ref      string   `json:"ref"`
				Selector string   `json:"selector"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			if len(a.Paths) == 0 {
				return nil, fmt.Errorf("paths 不能为空")
			}
			for _, p := range a.Paths {
				if !filepath.IsAbs(p) {
					wd, _ := os.Getwd()
					p = filepath.Join(wd, p)
				}
				if _, err := os.Stat(p); err != nil {
					return nil, fmt.Errorf("文件不可读 %q: %w", p, err)
				}
			}
			sel := a.Selector
			if sel == "" {
				sel = "input[type=file]"
			}
			if a.Ref != "" {
				sel = refSelector(a.Ref)
			}
			unlock, err := b.lock()
			if err != nil {
				return nil, err
			}
			defer unlock()
			if err := chromedp.Run(b.activeTab().ctx, chromedp.SetUploadFiles(sel, a.Paths)); err != nil {
				return nil, fmt.Errorf("设置上传文件失败（元素不是 input[type=file] 或未找到）: %w", err)
			}
			return content(fmt.Sprintf("已设置 %d 个上传文件，可继续点击提交", len(a.Paths))), nil
		},
	})

	s.Add(mcpserver.Tool{
		Name: "browser_cookies",
		Description: "Cookie 管理：list 列出全部 / set 设置（name+value，url 或 domain 定界）/" +
			"delete 删除。用于登录态查验与会话迁移。",
		Schema: obj(map[string]any{
			"action": map[string]any{"type": "string", "enum": []string{"list", "set", "delete"}, "description": "操作"},
			"name":   pstr("set/delete 的 cookie 名"),
			"value":  pstr("set 的值"),
			"url":    pstr("set/delete 的定界 URL（影响默认 domain/path）"),
			"domain": pstr("set/delete 的 domain（与 url 二选一）"),
			"path":   pstr("set 的 path（缺省 /）"),
		}, "action"),
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			var a struct {
				Action string `json:"action"`
				Name   string `json:"name"`
				Value  string `json:"value"`
				URL    string `json:"url"`
				Domain string `json:"domain"`
				Path   string `json:"path"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			unlock, err := b.lock()
			if err != nil {
				return nil, err
			}
			defer unlock()
			tab := b.activeTab()
			switch a.Action {
			case "list":
				var cs []*network.Cookie
				err := chromedp.Run(tab.ctx, chromedp.ActionFunc(func(c context.Context) error {
					var err error
					cs, err = network.GetCookies().Do(c)
					return err
				}))
				if err != nil {
					return nil, err
				}
				if len(cs) == 0 {
					return content("(无 cookie)"), nil
				}
				lines := make([]string, 0, len(cs))
				for _, c := range cs {
					lines = append(lines, fmt.Sprintf("%s=%s (domain=%s path=%s expires=%s size=%d)",
						c.Name, c.Value, c.Domain, c.Path,
						time.Unix(int64(c.Expires), 0).Format("2006-01-02 15:04"), len(c.Value)))
				}
				return content(strings.Join(lines, "\n")), nil
			case "set":
				if a.Name == "" {
					return nil, fmt.Errorf("set 需要 name 与 value")
				}
				p := network.SetCookie(a.Name, a.Value)
				switch {
				case a.URL != "":
					p = p.WithURL(a.URL)
				case a.Domain != "":
					p = p.WithDomain(a.Domain)
				default:
					return nil, fmt.Errorf("set 需要 url 或 domain 之一定界")
				}
				if a.Path != "" {
					p = p.WithPath(a.Path)
				}
				if err := chromedp.Run(tab.ctx, chromedp.ActionFunc(func(c context.Context) error {
					return p.Do(c)
				})); err != nil {
					return nil, err
				}
				return content("已设置 cookie " + a.Name), nil
			case "delete":
				if a.Name == "" {
					return nil, fmt.Errorf("delete 需要 name")
				}
				p := network.DeleteCookies(a.Name)
				if a.URL != "" {
					p = p.WithURL(a.URL)
				}
				if a.Domain != "" {
					p = p.WithDomain(a.Domain)
				}
				if err := chromedp.Run(tab.ctx, chromedp.ActionFunc(func(c context.Context) error {
					return p.Do(c)
				})); err != nil {
					return nil, err
				}
				return content("已删除 cookie " + a.Name), nil
			default:
				return nil, fmt.Errorf("未知 action %q", a.Action)
			}
		},
	})
}

// mouseClick 真实鼠标点击：滚动到可见 → 取视口中心 → CDP 鼠标事件
// （move 出→move 入→press→release，双击 clickCount=2）。合成 click()
// 无 isTrusted，部分页面不响应，故一律走真事件。
func (b *browser) mouseClick(sel string, double bool) error {
	_ = b.runEval(fmt.Sprintf(`(%s; dq(%s) && dq(%s).scrollIntoView({block: 'center'}))`,
		dqHelper, jsString(sel), jsString(sel)), nil)
	x, y, err := b.deepCenter(sel)
	if err != nil {
		return err
	}
	count := 1
	if double {
		count = 2
	}
	return chromedp.Run(b.activeTab().ctx,
		chromedp.MouseEvent(input.MouseMoved, 1, 1),
		chromedp.MouseEvent(input.MouseMoved, x, y),
		chromedp.MouseEvent(input.MousePressed, x, y, chromedp.Button("left"), chromedp.ClickCount(count)),
		chromedp.MouseEvent(input.MouseReleased, x, y, chromedp.Button("left"), chromedp.ClickCount(count)),
	)
}

// pressCombo 解析 "ctrl+shift+t" 形式的组合键并发送。
func (b *browser) pressCombo(combo string) error {
	tokens := strings.Split(strings.ToLower(strings.TrimSpace(combo)), "+")
	if len(tokens) == 0 || tokens[0] == "" {
		return fmt.Errorf("按键为空")
	}
	mods := input.Modifier(0)
	for _, t := range tokens[:len(tokens)-1] {
		switch t {
		case "ctrl", "control":
			mods |= input.ModifierCtrl
		case "alt", "option":
			mods |= input.ModifierAlt
		case "cmd", "meta", "command":
			mods |= input.ModifierMeta
		case "shift":
			mods |= input.ModifierShift
		default:
			return fmt.Errorf("未知修饰键 %q", t)
		}
	}
	key := tokens[len(tokens)-1]
	seq, named := keyParams[key]
	if !named && len([]rune(key)) != 1 {
		return fmt.Errorf("未知按键 %q", key)
	}
	r := []rune(seq)
	if !named {
		r = []rune(key)
	}
	if len(r) == 0 || r[0] == 0 {
		return fmt.Errorf("未知按键 %q", key)
	}
	evs := kb.Encode(r[0])
	if len(evs) == 0 {
		evs = kb.EncodeUnidentified(r[0])
	}
	return chromedp.Run(b.activeTab().ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		for _, e := range evs {
			e.Modifiers = mods
			if err := e.Do(ctx); err != nil {
				return err
			}
		}
		return nil
	}))
}
