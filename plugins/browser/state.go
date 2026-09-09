// state.go grf-browser 的浏览器状态：懒启动一个 chrome 实例（chromedp
// ExecAllocator），多标签页管理（每标签独立 chromedp context），控制台
// 与网络事件按标签截留（供 browser_console_messages /
// browser_network_requests 回放）。
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	cdbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// maxEventLog 每标签控制台/网络记录上限（超出丢最旧）。
const maxEventLog = 500

// reqInfo 在途网络请求的元数据（响应到达时计算耗时）。
type reqInfo struct {
	desc  string
	start time.Time
}

// btab 一个标签页：独立 target/context 与事件日志。
type btab struct {
	ctx     context.Context
	cxl     context.CancelFunc
	mu      sync.Mutex
	console []string
	nets    []string
	pending map[network.RequestID]reqInfo
}

// browser 整个插件共享的浏览器实例。
type browser struct {
	mu         sync.Mutex
	alloc      context.Context
	allocCxl   context.CancelFunc
	tabs       []*btab
	active     int
	headless   bool
	chromePath string
	closed     map[target.ID]bool // 已主动关闭的 target：同步对账时不得重新接管
}

func newBrowser(headless bool, chromePath string) *browser {
	return &browser{headless: headless, chromePath: chromePath, closed: map[target.ID]bool{}}
}

// setVisible 请求以可见窗口运行（浏览器懒启动，须在首次 lock() 之前
// 调用才生效；已启动的实例不受影响，需 browser_close 后重开）。
func (b *browser) setVisible() {
	b.mu.Lock()
	b.headless = false
	b.mu.Unlock()
}

// ensure 懒启动浏览器并确保存在活跃标签页。调用方持有 b.mu（工具处理
// 器统一经 b.lock() 串行化，规避 chromedp 并发跑动作的限制）。
func (b *browser) ensure() error {
	if b.alloc == nil {
		opts := append(chromedp.DefaultExecAllocatorOptions[:],
			chromedp.Flag("headless", b.headless),
			// 关掉 enable-automation（chromedp 默认开启）：它触发
			// "正受到自动测试软件的控制"提示条，可见模式下很扎眼
			chromedp.Flag("enable-automation", false),
			chromedp.Flag("remote-allow-origins", "*"),
			chromedp.WindowSize(1440, 900),
		)
		if b.chromePath != "" {
			opts = append(opts, chromedp.ExecPath(b.chromePath))
		}
		b.alloc, b.allocCxl = chromedp.NewExecAllocator(context.Background(), opts...)
	}
	if len(b.tabs) == 0 {
		if err := b.openTabLocked("about:blank"); err != nil {
			return err
		}
		b.armDownloadsLocked()
		return nil
	}
	if b.active >= len(b.tabs) {
		b.active = len(b.tabs) - 1
	}
	b.syncTabsLocked()
	return nil
}

// armDownloadsLocked 允许下载并落到工作目录 downloads/（allowAndName 以
// guid 命名，进度事件进各标签控制台日志）。浏览器级设置，首个标签建立
// 后设一次即可。
func (b *browser) armDownloadsLocked() {
	dir := filepath.Join(mustWd(), "downloads")
	_ = os.MkdirAll(dir, 0o755)
	_ = chromedp.Run(b.tabs[0].ctx, chromedp.ActionFunc(func(c context.Context) error {
		return cdbrowser.SetDownloadBehavior(cdbrowser.SetDownloadBehaviorBehaviorAllowAndName).
			WithDownloadPath(dir).WithEventsEnabled(true).Do(c)
	}))
}

func mustWd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

// syncTabsLocked 与浏览器实际 target 对账（b.mu 已持有）：chromedp 只跟踪
// 自己创建的 target，用户在浏览器界面手动开的标签页不在 b.tabs 里、手动
// 关掉的也不会消失。枚举浏览器全部 page target：外部的接入管理（不改变
// 活跃指针），已消失的剔除（活跃页被关则就近收敛）。
func (b *browser) syncTabsLocked() {
	infos, err := chromedp.Targets(b.tabs[0].ctx)
	if err != nil {
		return // 枚举失败保持现状（下个动作重试）
	}
	pages := map[target.ID]*target.Info{}
	for _, info := range infos {
		if info.Type == "page" {
			pages[info.TargetID] = info
		}
	}
	known := map[target.ID]bool{}
	var kept []*btab
	activeKept := false
	for i, t := range b.tabs {
		c := chromedp.FromContext(t.ctx)
		if c == nil || c.Target == nil || pages[c.Target.TargetID] == nil {
			if i < b.active {
				b.active--
			}
			continue // 用户已手动关闭
		}
		known[c.Target.TargetID] = true
		kept = append(kept, t)
		if i == b.active {
			activeKept = true
			b.active = len(kept) - 1
		}
	}
	b.tabs = kept
	if len(kept) == 0 {
		_ = b.openTabLocked("about:blank")
		return
	}
	if !activeKept && b.active >= len(b.tabs) {
		b.active = len(b.tabs) - 1
	}
	for id := range pages {
		if !known[id] && !b.closed[id] {
			b.attachLocked(id) // 用户手动新开的标签页，静默接入
		}
	}
}

// attachLocked 接管一个已存在的 target 为受管标签页（b.mu 已持有，
// 不改变活跃指针；接管失败直接放弃，下次同步重试）。对异常 target 建
// 会话可能永远等不到应答（如刚被关闭的），必须带超时，否则整个插件
// 卡死在 lock() 上。
func (b *browser) attachLocked(id target.ID) {
	ctx, cxl := chromedp.NewContext(b.parentLocked(), chromedp.WithTargetID(id))
	tab := &btab{ctx: ctx, cxl: cxl, pending: map[network.RequestID]reqInfo{}}
	chromedp.ListenTarget(ctx, func(ev any) { tab.onEvent(ev) })
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := chromedp.Run(runCtx); err != nil {
		cxl()
		b.closed[id] = true // 接管失败的 target 不再反复重试
		return
	}
	_ = b.armStealth(ctx) // 注入失败不阻断接管（下次同步会重试）
	b.tabs = append(b.tabs, tab)
}

// closeTabLocked 关闭一个受管标签页（b.mu 已持有）：先记墓碑再同步取消
// target context（带超时）。异步取消会让随后的 syncTabsLocked 看到尚未
// 消失的 target 而误接管，接管已死 target 会导致永久卡死。
func (b *browser) closeTabLocked(idx int) {
	tab := b.tabs[idx]
	if c := chromedp.FromContext(tab.ctx); c != nil && c.Target != nil {
		b.closed[c.Target.TargetID] = true
	}
	b.tabs = append(b.tabs[:idx], b.tabs[idx+1:]...)
	done := make(chan struct{})
	go func() {
		tab.cxl()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
}

// openTabLocked 新开标签页（b.mu 已持有）。立即 Run 一次物化 target，
// 再挂事件监听（导航/控制台/网络）。
// parentLocked 新标签页 context 的父级：必须挂在已有标签页的 context 上
// （继承 Browser 连接，首个 Run 在同一浏览器里 CreateTarget）。直接挂
// b.alloc 会因缺 Browser 连接而另起一个浏览器进程——多标签就变成多浏览器。
func (b *browser) parentLocked() context.Context {
	if len(b.tabs) > 0 {
		return b.tabs[0].ctx
	}
	return b.alloc
}

func (b *browser) openTabLocked(url string) error {
	ctx, cxl := chromedp.NewContext(b.parentLocked())
	tab := &btab{ctx: ctx, cxl: cxl, pending: map[network.RequestID]reqInfo{}}
	chromedp.ListenTarget(ctx, func(ev any) { tab.onEvent(ev) })
	if err := b.armStealth(ctx); err != nil {
		cxl()
		return fmt.Errorf("隐藏自动化痕迹失败: %w", err)
	}
	if err := chromedp.Run(ctx, chromedp.Navigate(url)); err != nil {
		cxl()
		return fmt.Errorf("打开标签页失败: %w", err)
	}
	b.tabs = append(b.tabs, tab)
	b.active = len(b.tabs) - 1
	return nil
}

// stealthJS 每个新文档执行前注入，抹掉最常见的自动化指纹（搜索引擎/
// 反爬脚本据此识破 CDP 控制）。无法掩盖所有侧信道（CDP 连接本身、
// Runtime.enable 的栈帧差异等），只求过常规前端检测。
const stealthJS = `
Object.defineProperty(navigator, 'webdriver', {get: () => undefined});
// Permissions.query 对 Notification 的应答与真实用户一致（无头/自动化的
// denied 与 permissions API 的 granted 不一致是经典检测点）
if (window.Notification) {
  Object.defineProperty(Notification, 'permission', {get: () => 'default'});
}
`

// armStealth 给标签页挂 stealth 注入脚本（attach 外部标签页同样调用）。
func (b *browser) armStealth(ctx context.Context) error {
	return chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(stealthJS).Do(c)
		return err
	}))
}

// active 返回活跃标签页（b.mu 已持有）。
func (b *browser) activeTab() *btab { return b.tabs[b.active] }

// activateLocked 把标签页真正切换到前台（b.mu 已持有）：除了更新
// b.active 指针，还要通知浏览器激活 target——否则可见模式下标签页
// 不动，document.visibilityState / hasFocus 也不会变。
func (b *browser) activateLocked(idx int) {
	if idx < 0 || idx >= len(b.tabs) {
		return
	}
	tab := b.tabs[idx]
	c := chromedp.FromContext(tab.ctx)
	if c == nil || c.Target == nil {
		return
	}
	_ = chromedp.Run(tab.ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		return target.ActivateTarget(c.Target.TargetID).Do(ctx)
	}))
}

// lock 串行化入口：锁住浏览器 + 懒初始化。返回解锁函数。
func (b *browser) lock() (func(), error) {
	b.mu.Lock()
	if err := b.ensure(); err != nil {
		b.mu.Unlock()
		return nil, err
	}
	return b.mu.Unlock, nil
}

// shutdown 关闭整个浏览器（进程退出路径调用：stdin EOF / 信号）。
func (b *browser) shutdown() {
	b.mu.Lock()
	b.closeLocked()
	b.mu.Unlock()
}

// closeLocked 关闭整个浏览器（取消 allocator 连带关闭所有 target；
// 须已持有 b.mu，供工具处理器在 lock() 内调用）。
func (b *browser) closeLocked() {
	if b.allocCxl != nil {
		b.allocCxl()
		b.alloc, b.allocCxl = nil, nil
	}
	b.tabs, b.active = nil, 0
}

// onEvent 标签页事件收集（chromedp 事件 goroutine 调用）。
func (t *btab) onEvent(ev any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch e := ev.(type) {
	case *runtime.EventConsoleAPICalled:
		parts := make([]string, 0, len(e.Args))
		for _, arg := range e.Args {
			switch {
			case arg.Value != nil:
				parts = append(parts, string(arg.Value))
			case arg.Description != "":
				parts = append(parts, arg.Description)
			default:
				parts = append(parts, arg.ObjectID.String())
			}
		}
		t.console = appendCap(t.console, fmt.Sprintf("[%s] %s", e.Type, join(parts, " ")))
	case *runtime.EventExceptionThrown:
		if e.ExceptionDetails != nil {
			t.console = appendCap(t.console, "[pageerror] "+e.ExceptionDetails.Text)
		}
	case *network.EventRequestWillBeSent:
		t.pending[e.RequestID] = reqInfo{
			desc:  e.Request.Method + " " + e.Request.URL,
			start: time.Now(),
		}
	case *network.EventResponseReceived:
		if p, ok := t.pending[e.RequestID]; ok {
			extra := ""
			if e.Response.MimeType != "" {
				extra += ", " + e.Response.MimeType
			}
			if n := e.Response.EncodedDataLength; n > 0 {
				extra += fmt.Sprintf(", %.1fKB", float64(n)/1024)
			}
			t.nets = appendCap(t.nets, fmt.Sprintf("%s -> %d (%s%.0fms%s)",
				p.desc, e.Response.Status, e.Type, time.Since(p.start).Seconds()*1000, extra))
			delete(t.pending, e.RequestID)
		}
	case *network.EventLoadingFailed:
		if p, ok := t.pending[e.RequestID]; ok {
			t.nets = appendCap(t.nets, p.desc+" -> 失败: "+e.ErrorText)
			delete(t.pending, e.RequestID)
		}
	case *page.EventJavascriptDialogOpening:
		// 原生对话框（alert/confirm/prompt）会挂起页面 JS 与后续指令，
		// 必须立即应答。默认 dismiss（等价点"取消"，不会误触确认动作），
		// 内容记入控制台日志供 browser_console_messages 回看。
		t.console = appendCap(t.console, fmt.Sprintf("[dialog] %s: %s", e.Type, e.Message))
		go func() {
			_ = chromedp.Run(t.ctx, page.HandleJavaScriptDialog(false))
		}()
	case *cdbrowser.EventDownloadProgress:
		switch e.State {
		case cdbrowser.DownloadProgressStateCompleted:
			t.console = appendCap(t.console,
				fmt.Sprintf("[download] 完成 guid=%s（文件在 downloads/ 目录，名为 guid）", e.GUID))
		case cdbrowser.DownloadProgressStateCanceled:
			t.console = appendCap(t.console, "[download] 已取消 guid="+e.GUID)
		}
	}
}

func appendCap(s []string, item string) []string {
	if len(s) >= maxEventLog {
		s = s[1:]
	}
	return append(s, item)
}

func join(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}

// keyParams 具名按键 → kb 常量（browser_press_key 用）。
var keyParams = map[string]string{
	"enter": kb.Enter, "return": kb.Enter, "tab": kb.Tab, "escape": kb.Escape, "esc": kb.Escape,
	"backspace": kb.Backspace, "delete": kb.Delete, "space": " ",
	"arrowup": kb.ArrowUp, "arrowdown": kb.ArrowDown, "arrowleft": kb.ArrowLeft, "arrowright": kb.ArrowRight,
	"up": kb.ArrowUp, "down": kb.ArrowDown, "left": kb.ArrowLeft, "right": kb.ArrowRight,
	"pageup": kb.PageUp, "pagedown": kb.PageDown, "home": kb.Home, "end": kb.End,
}
