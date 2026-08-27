// chat.go 全屏聊天 REPL：备用屏幕 + 底部输入框 + 上方内容滚动区。
// 思考进度行实时刷新、正文原文增量显示（不做 markdown 渲染）、连续
// 工具事件合并折叠（可展开）、todo/goal 状态面板、斜杠指令提示、
// ask/计划/升审批弹窗。SIGWINCH 或终端尺寸变化时按新宽度整帧重排重绘。
package tui

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"grf/app"
	"grf/llm"
	"grf/term"
	"grf/tools"
)

// termKey 按键事件别名。
type termKey = term.KeyEv

// dim / red / cyan / bold 样式辅助。
func dim(s string) string     { return "\x1b[2m" + s + "\x1b[0m" }
func red(s string) string     { return "\x1b[31m" + s + "\x1b[0m" }
func cyan(s string) string    { return "\x1b[36m" + s + "\x1b[0m" }
func bold(s string) string    { return "\x1b[1m" + s + "\x1b[0m" }
func reverse(s string) string { return "\x1b[7m" + s + "\x1b[0m" }

// blockKind 内容块类型。
type blockKind int

const (
	blkText  blockKind = iota // 普通文本（用户输入 / 助手回复原文）
	blkDim                    // 暗色系统行
	blkErr                    // 错误
	blkThink                  // 思考摘要（可展开）
	blkTools                  // 工具事件合并（可展开）
)

type block struct {
	kind     blockKind
	text     string
	think    string
	toolEvs  []string
	toolN    int
	toolDone bool
	expanded bool
}

// hubSeg 流式分段：模型回复中按输出顺序交替出现的思考段/正文段。
type hubSeg struct {
	think bool
	sb    strings.Builder
	chars int
	start time.Time
	end   time.Time
}

// segView 分段只读快照（事件循环 goroutine 渲染用）。
type segView struct {
	think bool
	text  string
	chars int
	secs  int
	live  bool // 仍在接收增量
}

// streamHub 汇聚后台 agent 的流式状态（锁保护），notify 通知重绘。
// 思考与正文按模型实际输出顺序存为分段，工具事件到达时（applyEvent）
// 由事件循环把已完成分段固化成块，保证 思考/工具/正文 分开展示。
type streamHub struct {
	mu     sync.Mutex
	segs   []hubSeg
	notify chan struct{}
}

func newHub() *streamHub { return &streamHub{notify: make(chan struct{}, 1)} }

func (h *streamHub) begin() {
	h.mu.Lock()
	h.segs = nil
	h.mu.Unlock()
}

func (h *streamHub) ping() {
	select {
	case h.notify <- struct{}{}:
	default:
	}
}

// append 写入增量；模式切换时开新分段并给上一段记结束时间。
func (h *streamHub) append(d string, think bool) {
	h.mu.Lock()
	now := time.Now()
	n := len(h.segs)
	if n == 0 || h.segs[n-1].think != think {
		if n > 0 && h.segs[n-1].end.IsZero() {
			h.segs[n-1].end = now
		}
		h.segs = append(h.segs, hubSeg{think: think, start: now})
		n++
	}
	s := &h.segs[n-1]
	s.chars += len([]rune(d))
	s.sb.WriteString(d)
	h.mu.Unlock()
	h.ping()
}

func (h *streamHub) content(d string)   { h.append(d, false) }
func (h *streamHub) reasoning(d string) { h.append(d, true) }

// segViews 分段快照（live 段按当前时间算秒数）。
func (h *streamHub) segViews() []segView {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]segView, 0, len(h.segs))
	for i, s := range h.segs {
		end := s.end
		live := end.IsZero()
		if live {
			end = time.Now()
		}
		out = append(out, segView{
			think: s.think, text: s.sb.String(), chars: s.chars,
			secs: int(end.Sub(s.start).Seconds()), live: live || i == len(h.segs)-1,
		})
	}
	return out
}

// seal 取走全部分段并转成块（须在事件循环 goroutine 调用）。
func (h *streamHub) seal() []block {
	h.mu.Lock()
	segs := h.segs
	h.segs = nil
	h.mu.Unlock()
	now := time.Now()
	var out []block
	for _, s := range segs {
		end := s.end
		if end.IsZero() {
			end = now
		}
		if s.think {
			if s.chars == 0 {
				continue
			}
			out = append(out, block{
				kind:  blkThink,
				text:  fmt.Sprintf("💭 思考 %ds，%d 字（Ctrl+O 展开）", int(end.Sub(s.start).Seconds()), s.chars),
				think: s.sb.String(),
			})
		} else if s.sb.Len() > 0 {
			out = append(out, block{kind: blkText, text: s.sb.String()})
		}
	}
	return out
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

func lastLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

// ---- 聊天主结构 ----

type runDoneMsg struct{ err error }

// Chat 全屏聊天状态机。
type Chat struct {
	app *app.App
	scr *term.Screen
	rd  *term.Reader
	ctx context.Context

	hub *streamHub

	events chan any // term.Event / 内部消息聚合

	blocks   []block
	todos    []tools.Todo
	todoOpen bool // todo 面板展开态（点击切换）
	// todo 面板在帧行中的行区间（frameLines 计算供鼠标命中）
	todoRowLo, todoRowHi int
	goalText             string
	status               string // 顶部分隔线短暂状态
	statusAt             time.Time

	ed   *Editor
	hist *lineHistory

	scrollUp int // 距底部行数（0=钉底）
	running  bool
	cancel   context.CancelFunc
	quitAt   time.Time // 空闲时首次 Ctrl+C 的时间（时限内再按才退出）

	// 弹窗/模态（互斥，非 nil 即捕获按键）
	pop  *popup      // 模型/会话列表选择
	ask  *askForm    // ask_user_question 表单
	plan *planView   // 计划审批
	escv *escApprove // 沙箱升审批

	hintIdx int // 指令提示选中

	mouseOn      bool
	dragging     bool
	dragA, dragB int // 拖选起止（内容绝对行号）

	width, height int
	lastPaint     time.Time
	painting      bool
}

// NewChat 创建聊天界面（尚未启动）。
func NewChat(a *app.App) *Chat {
	return &Chat{
		app:     a,
		hub:     newHub(),
		events:  make(chan any, 128),
		ed:      NewEditor("> "),
		hist:    loadHistory(),
		mouseOn: resolveMouseOn(),
	}
}

// resolveMouseOn 鼠标初始状态：GRF_MOUSE > settings.mouse > 默认开启。
func resolveMouseOn() bool {
	if v := os.Getenv("GRF_MOUSE"); v != "" {
		return v != "0" && !strings.EqualFold(v, "false")
	}
	if s, err := loadSettings(); err == nil && s.Mouse != nil {
		return *s.Mouse
	}
	return true
}

// Run 启动全屏交互（阻塞至退出）。
func (c *Chat) Run(ctx context.Context) error {
	c.ctx = ctx
	scr := term.NewScreen()
	c.scr = scr
	if err := scr.Start(c.mouseOn); err != nil {
		return fmt.Errorf("进入全屏失败: %w", err)
	}
	defer scr.Stop()
	c.rd = term.ReadInput(os.Stdin)
	defer c.rd.Close()

	c.width, c.height = scr.Size()
	if c.width < 8 || c.height < 6 {
		c.width, c.height = 80, 24
	}

	// TUI 接管交互宿主
	host := newTUIHost(c)
	c.app.SetHost(host)

	// agent 流式接线
	root := c.app.Root()
	root.SetStreamHooks(c.hub.content, c.hub.reasoning)
	root.SetOnEvent(c.onEvent)
	root.OnIdleNotice = func(text string) {
		c.events <- noticeMsg{text: text}
	}

	c.emitBlock(block{kind: blkDim, text: fmt.Sprintf("grf-harness %s - 输入消息开始对话，/help 查看命令（Tab 切换模式 · Ctrl+O 思考 · Shift+Tab 鼠标）", app.Version)})
	// -resume 启动：上方重放已恢复的历史聊天
	if len(c.app.Root().History()) > 1 {
		c.replayHistory()
	}
	c.paint(true)

	// 事件循环
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			c.finishRun()
			return nil
		case <-ticker.C:
			if c.running {
				c.paint(false)
			}
		case <-c.scr.ResizeCh():
			c.width, c.height = c.scr.Size()
			c.paint(true) // 尺寸变化：整帧按新宽度重排
		case <-c.hub.notify:
			c.paint(false)
		case ev := <-c.rd.Events():
			if ev == nil {
				return nil // stdin 关闭
			}
			if quit := c.handleInput(ev); quit {
				c.finishRun()
				return nil
			}
		case msg := <-c.events:
			if quit := c.handleMsg(msg); quit {
				c.finishRun()
				return nil
			}
		}
	}
}

type noticeMsg struct{ text string }

// eventMsg agent 过程事件（后台 goroutine → 事件循环）。
type eventMsg struct {
	kind, detail string
}

// onEvent agent 过程事件投递（可能在后台 goroutine 被调用，绝不直接改状态）。
// 阻塞等待事件循环（带超时兜底），todos/goal 等状态事件不允许被丢弃。
func (c *Chat) onEvent(kind, detail string) {
	select {
	case c.events <- eventMsg{kind: kind, detail: detail}:
	case <-time.After(5 * time.Second):
	}
}

// applyEvent 在事件循环 goroutine 上应用 agent 事件。
func (c *Chat) applyEvent(kind, detail string) {
	switch kind {
	case "tool":
		c.sealHub()
		c.pushToolEvent("[tool] " + detail)
	case "result":
		c.pushToolEvent("[->]  " + detail)
	case "todos":
		var todos []tools.Todo
		if json.Unmarshal([]byte(detail), &todos) == nil {
			c.todos = todos
		}
	case "goal":
		c.goalText = detail
	case "notice":
		c.emitBlock(block{kind: blkDim, text: "[info] " + detail})
	case "plan":
		// 模式提示已在输入框上方状态栏展示，不再打印内容行
	}
	// todos/goal 等状态变更立即重绘（节流），避免空闲时面板滞后到下一次按键
	c.paint(false)
}

// sealHub 把流式分段按序固化成块（工具事件/回合结束前调用，保证
// 思考、正文、工具块按模型实际输出顺序排列）。
func (c *Chat) sealHub() {
	for _, b := range c.hub.seal() {
		c.emitBlock(b)
	}
}

// pushToolEvent 连续 tool/result 合并进当前工具块。
func (c *Chat) pushToolEvent(e string) {
	if n := len(c.blocks); n > 0 && c.blocks[n-1].kind == blkTools && !c.blocks[n-1].toolDone {
		b := &c.blocks[n-1]
		b.toolEvs = append(b.toolEvs, e)
		if strings.HasPrefix(e, "[tool] ") {
			b.toolN++
		}
		return
	}
	b := block{kind: blkTools, toolEvs: []string{e}, toolN: 0}
	if strings.HasPrefix(e, "[tool] ") {
		b.toolN = 1
	}
	c.blocks = append(c.blocks, b)
}

func (c *Chat) emitBlock(b block) {
	// 思考摘要打断工具块合并
	if b.kind == blkThink {
		if n := len(c.blocks); n > 0 && c.blocks[n-1].kind == blkTools {
			c.blocks[n-1].toolDone = true
		}
	}
	c.blocks = append(c.blocks, b)
	c.scrollUp = 0
}

// resetForReplay 恢复会话后切新会话重放：清空当前内容区/状态面板，
// 显示标题行后把历史消息重放到上方（事件循环 goroutine 上调用）。
func (c *Chat) resetForReplay(title string) {
	c.blocks = nil
	c.todos = nil
	c.goalText = ""
	c.scrollUp = 0
	c.emitBlock(block{kind: blkDim, text: title})
	c.replayHistory()
	c.paint(false)
}

// replayHistory 把恢复的历史消息按聊天记录形式重放到上方内容区
// （-resume 启动与 /resume 指令共用；只走块渲染，不触发任何请求）。
func (c *Chat) replayHistory() {
	// sealTools 收口紧邻的工具块（文本块出现前工具段已结束）
	sealTools := func() {
		if n := len(c.blocks); n > 0 && c.blocks[n-1].kind == blkTools {
			c.blocks[n-1].toolDone = true
		}
	}
	for _, m := range c.app.Root().History() {
		switch m.Role {
		case "user":
			if m.Content == "" || strings.HasPrefix(m.Content, "[system notice]") {
				continue // 图像消息与系统通知不重放
			}
			sealTools()
			c.emitBlock(block{kind: blkText, text: "> " + m.Content})
		case "assistant":
			if m.Reasoning != "" {
				sealTools()
				c.emitBlock(block{
					kind:  blkThink,
					text:  fmt.Sprintf("💭 思考 %d 字（Ctrl+O 展开）", len([]rune(m.Reasoning))),
					think: m.Reasoning,
				})
			}
			if m.Content != "" {
				sealTools()
				c.emitBlock(block{kind: blkText, text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				c.pushToolEvent("[tool] " + tc.Name() + " " + llm.CompactArgs(tc.Arguments(), 120))
			}
		case "tool":
			c.pushToolEvent("[->]  " + firstLine(m.Content))
		}
	}
	sealTools()
}

// handleMsg 内部消息（runDone / notice / host 请求已在 host.go 通过 events 传递）。
func (c *Chat) handleMsg(msg any) bool {
	switch m := msg.(type) {
	case runDoneMsg:
		c.finishStream(m.err)
		// 回合结束自动保存会话（复用同一会话文件）
		c.app.AutoSave()
	case eventMsg:
		c.applyEvent(m.kind, m.detail)
	case noticeMsg:
		// 空闲时后台子代理落定：通知并入 → 自动续跑收尾
		c.emitBlock(block{kind: blkDim, text: "[notice] " + firstLine(m.text)})
		if !c.running {
			c.startRun("(background work settled; the runtime sent you notices — collect results with job_output/list_agents as needed and wrap up your answer)")
		}
	case *askRequest, *planRequest, *escRequest:
		c.hostRequest(m)
	case struct{}:
		c.paint(false)
	}
	return false
}

// startRun 后台执行一轮（goal 自动续跑）。
func (c *Chat) startRun(input string) {
	if c.running {
		return
	}
	c.running = true
	c.scrollUp = 0
	c.hub.begin()
	runCtx, cancel := context.WithCancel(c.ctx)
	c.cancel = cancel
	root := c.app.Root()
	go func() {
		_, err := root.RunAutoGoal(runCtx, input)
		c.events <- runDoneMsg{err: err}
	}()
	c.paint(true)
}

// finishStream 一轮结束：折叠思考、提交正文、工具块收尾。
func (c *Chat) finishStream(err error) {
	if !c.running {
		return
	}
	c.running = false
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	c.sealHub()
	if n := len(c.blocks); n > 0 && c.blocks[n-1].kind == blkText {
		c.emitBlock(block{kind: blkDim, text: ""})
	}
	if n := len(c.blocks); n > 0 && c.blocks[n-1].kind == blkTools {
		c.blocks[n-1].toolDone = true
	}
	switch {
	case err == nil:
	case isCanceledErr(err):
		c.emitBlock(block{kind: blkDim, text: "(已中断)"})
	default:
		c.emitBlock(block{kind: blkErr, text: "错误: " + err.Error()})
	}
	c.paint(true)
}

func isCanceledErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "context canceled")
}

// finishRun 退出前等待运行中回合结束。
func (c *Chat) finishRun() {
	if c.cancel != nil {
		c.cancel()
	}
	deadline := time.Now().Add(2 * time.Second)
	for c.running && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	c.app.Harness.Artifacts.Cleanup()
}

// ---- 渲染 ----

// renderBlock 单块文本（未换行）。
func renderBlock(b block) string {
	switch b.kind {
	case blkText:
		return b.text
	case blkDim:
		if b.text == "" {
			return ""
		}
		return dim(b.text)
	case blkErr:
		return red(b.text)
	case blkThink:
		if b.expanded {
			// 展开后摘要与思考正文整体灰色
			return dim(b.text + "\n" + b.think)
		}
		return dim(b.text)
	case blkTools:
		if b.expanded {
			return dim(strings.Join(b.toolEvs, "\n"))
		}
		if b.toolDone {
			return dim(fmt.Sprintf("🔧 共调用 %d 次工具（点击/Ctrl+O 展开）", b.toolN))
		}
		last := b.toolEvs[len(b.toolEvs)-1]
		return dim(last)
	}
	return b.text
}

// frameLines 组装整帧：返回（帧行、光标行、光标列、行→块映射）。
func (c *Chat) frameLines() (lines []string, curRow, curCol int, lineBlock map[int]int) {
	w := c.width - 2
	if w < 4 {
		w = 4
	}
	lineBlock = map[int]int{}

	add := func(text string, blkIdx int) {
		for _, ln := range term.Wrap(text, w) {
			lineBlock[len(lines)] = blkIdx
			lines = append(lines, ln)
		}
	}

	for i, b := range c.blocks {
		add(renderBlock(b), i)
	}
	// live 回复分段：按模型输出顺序展示（思考段摘要/进度 + 正文增量）
	if c.running {
		for _, sv := range c.hub.segViews() {
			if !sv.think {
				add(sv.text, -1)
				continue
			}
			line := fmt.Sprintf("💭 思考 %ds，%d 字", sv.secs, sv.chars)
			if sv.live {
				line = fmt.Sprintf("💭 思考中… %ds", sv.secs)
				if tail := lastLine(sv.text); tail != "" {
					if space := w - term.StringWidth(line) - 3; space > 4 {
						line += " · " + clipRunesT(tail, space)
					}
				}
			}
			add(dim(line), -1)
		}
	}

	// 弹窗/模态行
	bottom := []string{}
	if c.ask != nil {
		bottom = append(bottom, c.ask.render(w)...)
	} else if c.plan != nil {
		bottom = append(bottom, c.plan.render(w)...)
	} else if c.escv != nil {
		bottom = append(bottom, c.escv.render(w)...)
	} else if c.pop != nil {
		bottom = append(bottom, c.pop.render(w)...)
	} else if hints := c.slashHints(); len(hints) > 0 {
		for i, h := range hints {
			if i == c.hintIdx {
				bottom = append(bottom, cyan("❯ "+h))
			} else {
				bottom = append(bottom, "  "+dim(h))
			}
		}
	}
	// todo 面板（先记 bottom 内偏移，show 定稿后换算帧行区间）
	todoOffLo, todoOffHi := -1, -1
	if len(c.todos) > 0 {
		todoOffLo = len(bottom)
		bottom = append(bottom, c.todoPanel(w)...)
		todoOffHi = len(bottom)
	}

	sepW := c.width - 1
	if sepW < 3 {
		sepW = 3
	}
	statusParts := []string{}
	if m := c.app.Root().Model(); m != "" {
		statusParts = append(statusParts, m)
	}
	if c.app.Root().InPlanMode() {
		statusParts = append(statusParts, bold("计划模式"))
	}
	if c.app.Root().ApproveEach() {
		statusParts = append(statusParts, bold("审批模式"))
	}
	if c.goalText != "" {
		statusParts = append(statusParts, "goal: "+clipRunesT(c.goalText, 30))
	}
	if len(c.todos) > 0 {
		done := 0
		for _, t := range c.todos {
			if t.Status == "completed" {
				done++
			}
		}
		statusParts = append(statusParts, fmt.Sprintf("事项 %d/%d", done, len(c.todos)))
	}
	if c.running {
		statusParts = append(statusParts, "● 运行中 Ctrl+C 中断")
	}
	top := dim(strings.Repeat("─", sepW))
	if len(statusParts) > 0 || (c.status != "" && time.Now().Before(c.statusAt)) {
		text := " " + strings.Join(statusParts, " · ") + " "
		if c.status != "" && time.Now().Before(c.statusAt) {
			text = " " + c.status + " "
		}
		pad := sepW - term.StringWidth(text)
		if pad > 0 {
			top = dim(strings.Repeat("─", pad/2) + text + strings.Repeat("─", pad-pad/2))
		} else {
			top = dim(clipRunesT(text, sepW))
		}
	}

	inputText, inputCol := c.ed.Render(c.width - 2)

	// viewport 高度预算：总高 - 输入区(3) - bottom
	vpAvail := c.height - 3 - len(bottom)
	if vpAvail < 1 {
		vpAvail = 1
	}
	// 滚动窗口（距底部 scrollUp 行）
	show := lines
	if len(show) > vpAvail {
		from := len(show) - vpAvail - c.scrollUp
		if from < 0 {
			from = 0
		}
		to := from + vpAvail
		if to > len(show) {
			to = len(show)
		}
		show = show[from:to]
	}
	base := len(lines) - len(show) // show[0] 的绝对行号
	// todo 面板帧行区间（紧随内容区之后）
	if todoOffLo >= 0 {
		c.todoRowLo, c.todoRowHi = len(show)+todoOffLo, len(show)+todoOffHi
	} else {
		c.todoRowLo, c.todoRowHi = -1, -1
	}
	out := append([]string{}, show...)
	// 拖选反显
	if c.dragging && c.dragA != c.dragB {
		lo, hi := c.dragA, c.dragB
		if lo > hi {
			lo, hi = hi, lo
		}
		for i := range out {
			abs := base + i
			if abs >= lo && abs <= hi {
				out[i] = reverse(out[i])
			}
		}
	}
	out = append(out, bottom...)
	out = append(out, top, inputText, c.statsSep(sepW))
	// 光标在输入行
	curRow = len(out) - 2
	return out, curRow, inputCol, lineBlock
}

// statsSep 底部横线：嵌入本轮会话统计（轮/步 · LLM/工具耗时 · 首 token
// 与吞吐 · 缓存命中 · token 用量），无统计时纯横线。
func (c *Chat) statsSep(sepW int) string {
	line := dim(strings.Repeat("─", sepW))
	txt := c.statsLine()
	if txt == "" {
		return line
	}
	text := " " + txt + " "
	pad := sepW - term.StringWidth(text)
	if pad > 0 {
		return dim(strings.Repeat("─", pad/2) + text + strings.Repeat("─", pad-pad/2))
	}
	return dim(clipRunesT(text, sepW))
}

// statsLine 统计文本（样式：3 轮 · 52 步 │ LLM 6m55s · 工具 39.1s │
// 首 token 平均 2.6s · 86 tok/s │ 缓存命中 97% │ 输入 2.3M tok · 输出 24K tok）。
func (c *Chat) statsLine() string {
	root := c.app.Root()
	ls := root.LLMStats()
	calls, toolTime := root.ToolStats()
	if ls.Rounds == 0 && calls == 0 {
		return ""
	}
	parts := []string{
		fmt.Sprintf("%d 轮 · %d 步", ls.Rounds, calls),
		fmt.Sprintf("LLM %s · 工具 %s", fmtDur(ls.LLMTime), fmtDur(toolTime)),
	}
	if tps := ls.TokPerSec(); tps > 0 {
		parts = append(parts, fmt.Sprintf("首 token 平均 %s · %d tok/s", fmtDur(ls.FirstTokenAvg), tps))
	} else {
		parts = append(parts, fmt.Sprintf("首 token 平均 %s", fmtDur(ls.FirstTokenAvg)))
	}
	if ls.UsageSeen {
		if ls.CachePct >= 0 {
			parts = append(parts, fmt.Sprintf("缓存命中 %d%%", ls.CachePct))
		}
		parts = append(parts, fmt.Sprintf("输入 %s tok · 输出 %s tok", fmtTok(ls.PromptTokens), fmtTok(ls.CompletionTokens)))
	}
	return strings.Join(parts, " │ ")
}

// fmtDur 时长：<1min 一位小数秒；否则 m/s（超 1h 用 h/m）。
func fmtDur(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

// fmtTok token 数紧凑显示（K/M）。
func fmtTok(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%dK", (n+500)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// paint 重绘（节流 60ms；force 立即）。
func (c *Chat) paint(force bool) {
	if !force && time.Since(c.lastPaint) < 60*time.Millisecond {
		return
	}
	c.lastPaint = time.Now()
	lines, curRow, curCol, _ := c.frameLines()
	if len(lines) > c.height {
		lines = lines[:c.height]
	}
	c.scr.Paint(lines, curRow, curCol)
}

// todoPanel todo 面板：默认压缩（完成数 + 进行中项，最多 3 行），
// todoOpen 时展开全部条目（点击面板切换）。
func (c *Chat) todoPanel(w int) []string {
	done, total := 0, len(c.todos)
	var actives []string
	for _, t := range c.todos {
		if t.Status == "completed" {
			done++
		} else if t.Status == "in_progress" {
			actives = append(actives, t.Content)
		}
	}
	if c.todoOpen {
		head := fmt.Sprintf("☑ 事项 %d/%d（点击收起）", done, total)
		lines := []string{dim(head)}
		for _, t := range c.todos {
			mark := "☐"
			switch t.Status {
			case "completed":
				mark = "☑"
			case "in_progress":
				mark = "◐"
			}
			lines = append(lines, dim("  "+mark+" "+clipRunesT(t.Content, w-6)))
		}
		return lines
	}
	head := fmt.Sprintf("☑ 事项 %d/%d（点击展开）", done, total)
	lines := []string{dim(head)}
	for i, a := range actives {
		if i >= 2 {
			lines = append(lines, dim("  …"))
			break
		}
		lines = append(lines, dim("  ▸ "+clipRunesT(a, w-6)))
	}
	return lines
}

// slashMatches 当前输入的前缀匹配指令；alias 为同效别名（提示列表合并成
// 一行展示，前缀匹配别名也会命中主指令）。
var slashCmds = []struct{ name, alias, desc string }{
	{"/help", "", "显示本帮助"},
	{"/init", "", "分析代码库并生成/更新 AGENTS.md"},
	{"/model", "", "选择/切换模型"},
	{"/resume", "", "恢复历史会话"},
	{"/plan", "", "进入/退出计划模式"},
	{"/goal", "", "查看当前 goal"},
	{"/new", "/clear", "保存当前会话并开新会话"},
	{"/exit", "/quit", "退出"},
}

func (c *Chat) slashHints() []string {
	v := c.ed.Value()
	if !strings.HasPrefix(v, "/") || strings.ContainsAny(v, " \t") {
		return nil
	}
	var out []string
	for _, cmd := range slashCmds {
		matched := strings.HasPrefix(cmd.name, v) ||
			(cmd.alias != "" && strings.HasPrefix(cmd.alias, v))
		if !matched {
			continue
		}
		// 别名与主指令同行展示（空格分隔，保证 Tab 补全取 fields[0] 为主指令）
		line := cmd.name
		if cmd.alias != "" {
			line += " " + cmd.alias
		}
		out = append(out, line+"  "+cmd.desc)
	}
	return out
}

func clipRunesT(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// osc52Copy 经 OSC52 写系统剪贴板。
func osc52Copy(text string) {
	enc := base64.StdEncoding.EncodeToString([]byte(text))
	fmt.Fprintf(os.Stdout, "\x1b]52;c;%s\x07", enc)
}
