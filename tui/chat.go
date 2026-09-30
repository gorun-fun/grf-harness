// chat.go 全屏聊天 REPL：备用屏幕 + 底部输入框 + 上方内容滚动区。
// 本文件是状态机与事件循环（TUI 单 goroutine 规则：所有状态只在本
// goroutine 改，后台回调经 c.events 投递）；流式汇聚见 hub.go，整帧
// 渲染见 render.go，按键分发见 input.go，弹窗/交互宿主见 popups.go /
// host.go。SIGWINCH 或终端尺寸变化时按新宽度整帧重排重绘。
package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"git.gorun.fun/share/grf-harness/app"
	"git.gorun.fun/share/grf-harness/llm"
	"git.gorun.fun/share/grf-harness/selfupdate"
	"git.gorun.fun/share/grf-harness/term"
	"git.gorun.fun/share/grf-harness/tools"
)

// termKey 按键事件别名。
type termKey = term.KeyEv

// ---- 聊天主结构 ----

type runDoneMsg struct{ err error }

// bangDoneMsg "!cmd" 直执行落定（后台 goroutine → 事件循环）；out 为
// 已清理的展示文本（BangOutput 已在 RunBang 内完成），err 为错误文本。
type bangDoneMsg struct {
	out, err string
}

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

	bang bool // 命令模式（按 ! 进入，后续输入作为 shell 命令直接执行）

	scrollUp int // 距底部行数（0=钉底）
	running  bool
	cancel   context.CancelFunc
	runDone  chan struct{} // 运行 goroutine 的退出信号（关闭=已落定；finishRun 等它，不等 running 标志）
	quitAt   time.Time     // 空闲时首次 Ctrl+C 的时间（时限内再按才退出）

	// 弹窗/模态（互斥，非 nil 即捕获按键；沙箱升级/工具/计划审批均复用 ask）
	pop *popup   // 模型/会话列表选择
	ask *askForm // ask_user_question 表单

	hintIdx int // 指令提示选中

	mouseOn      bool
	dragging     bool
	dragA, dragB int // 拖选起止（内容绝对行号）

	width, height int
	lastPaint     time.Time
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

	// 后台探测新版本（dev 构建跳过；网络失败静默，不打扰交互）
	if app.Version != "dev" {
		go c.probeUpdate()
	}

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

// compactDoneMsg /compact 压缩落定（后台 goroutine → 事件循环）。
type compactDoneMsg struct {
	summary string
	err     string
}

// errStr error → 文本（nil 为空串，事件载荷不带 error）。
func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// updateMsg 自更新探测落定（后台 goroutine → 事件循环）。
type updateMsg struct{ version string }

// probeUpdate 后台探测下载服务器上的最新版本，有更新时经事件循环提示；
// 网络/服务器故障一律静默（提示是增益，不能阻塞或打扰交互）。
func (c *Chat) probeUpdate() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rel, err := selfupdate.Check(ctx, "")
	if err != nil || rel == nil || selfupdate.CompareVersions(rel.Version, app.Version) <= 0 {
		return
	}
	select {
	case c.events <- updateMsg{version: rel.Version}:
	case <-c.ctx.Done():
	}
}

// eventMsg agent 过程事件（后台 goroutine → 事件循环）。
type eventMsg struct {
	kind, detail string
}

// onEvent agent 过程事件投递（可能在后台 goroutine 被调用，绝不直接改状态）。
// todos/goal 等状态事件不允许被丢弃——等待到 Chat 生命周期结束（c.ctx）
// 而非超时放弃：事件循环被慢操作卡住超过旧实现的 5 秒兜底时，状态事件
// 会无痕丢失。
func (c *Chat) onEvent(kind, detail string) {
	select {
	case c.events <- eventMsg{kind: kind, detail: detail}:
	case <-c.ctx.Done():
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
	case "compact":
		// 自动压缩已在后台发生（新回合入口）：展示摘要首行
		c.emitBlock(block{kind: blkDim, text: "已自动压缩会话: " + detail})
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
			c.pushToolEvent("[->]  " + term.FirstLine(m.Content, 0))
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
	case bangDoneMsg:
		c.finishBang(m)
	case eventMsg:
		c.applyEvent(m.kind, m.detail)
	case noticeMsg:
		// 空闲时后台子代理落定：通知并入 → 自动续跑收尾
		c.emitBlock(block{kind: blkDim, text: "[notice] " + term.FirstLine(m.text, 0)})
		if !c.running {
			c.startRun("(background work settled; the runtime sent you notices — collect results with job_output/list_agents as needed and wrap up your answer)")
		}
	case compactDoneMsg:
		// /compact 落定：展示摘要首行并保存（历史已被替换）
		c.running = false
		if m.err != "" {
			c.emitBlock(block{kind: blkErr, text: "压缩失败: " + m.err})
		} else {
			c.emitBlock(block{kind: blkDim, text: "已压缩会话: " + term.FirstLine(m.summary, 0)})
		}
		c.app.AutoSave()
		c.paint(true)
	case updateMsg:
		c.emitBlock(block{kind: blkDim, text: fmt.Sprintf("✨ 有新版本 %s：退出后运行 gh update 更新", m.version)})
		// 空闲时一次性到达的消息：绕过 60ms 节流强制重绘，否则启动后
		// 无任何按键/流式活动时提示块永远不上屏
		c.paint(true)
	case *askRequest, *planRequest, *escRequest, *toolApproveRequest:
		c.hostRequest(m)
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
	done := make(chan struct{})
	c.runDone = done
	go func() {
		defer close(done) // 落定信号：goroutine 退出即关闭（finishRun 等待）
		_, err := root.RunAutoGoal(runCtx, input)
		select { // 事件循环退出后不再投递，避免 goroutine 泄漏。守卫必须是
		// c.ctx 而非 runCtx：runCtx 正是 Ctrl+C 取消的，中断后两个 case
		// 同时就绪会被 select 随机吞掉落定消息，running 永久卡 true，
		// 之后每次 Ctrl+C 都对已取消的 cancel 空转（中断失效）
		case c.events <- runDoneMsg{err: err}:
		case <-c.ctx.Done():
		}
	}()
	c.paint(true)
}

// execBang "!cmd" 直执行：走 bash 工具（沿用工作区/沙箱/超时），不经
// 模型、不进会话历史（也无自动保存）。
func (c *Chat) execBang(cmd string) {
	if c.running {
		c.emitBlock(block{kind: blkErr, text: "当前有任务在执行，等空闲后再执行命令"})
		return
	}
	if cmd == "" {
		c.emitBlock(block{kind: blkDim, text: "用法: !<命令>"})
		return
	}
	c.running = true
	c.scrollUp = 0
	runCtx, cancel := context.WithCancel(c.ctx)
	c.cancel = cancel
	done := make(chan struct{})
	c.runDone = done
	go func() {
		defer close(done) // 落定信号：goroutine 退出即关闭（finishRun 等待）
		out, err := c.app.RunBang(runCtx, cmd)
		es := ""
		if err != nil {
			es = err.Error()
		}
		select { // 事件循环退出后不再投递（守卫须用 c.ctx，理由见 startRun）
		case c.events <- bangDoneMsg{out: out, err: es}:
		case <-c.ctx.Done():
		}
	}()
	c.paint(true)
}

// finishBang 直执行落定：呈现原始输出或错误（仅展示，不落历史）。
func (c *Chat) finishBang(m bangDoneMsg) {
	if !c.running {
		return
	}
	c.running = false
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	switch {
	case m.err != "" && strings.Contains(m.err, "context canceled"):
		c.emitBlock(block{kind: blkDim, text: "(已中断)"})
		c.paint(true)
		return
	case m.err != "":
		c.emitBlock(block{kind: blkErr, text: "错误: " + m.err})
		c.paint(true)
		return
	}
	if m.out != "" {
		c.emitBlock(block{kind: blkText, text: m.out})
	} else {
		c.emitBlock(block{kind: blkDim, text: "（无输出）"})
	}
	c.emitBlock(block{kind: blkDim, text: ""})
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
	// 回合结束仍在排队的插话（回合尾部到达）：转为下一回合输入（dsh
	// inbox 的 nextTurn 语义），丢给模型前先展示并入内容；被中断的回合
	// 不续跑——残留排队消息一并丢弃，保证 Ctrl+C 是完全停下（interruptKey
	// 已在按键时丢弃过，此处兜底取消后才排入的竞态）
	texts := c.app.Root().DrainSteer()
	if isCanceledErr(err) {
		if len(texts) > 0 {
			c.emitBlock(block{kind: blkDim, text: fmt.Sprintf("· 已丢弃 %d 条排队消息（回合被中断）", len(texts))})
		}
		return
	}
	if len(texts) > 0 {
		c.emitBlock(block{kind: blkDim, text: "· 排队消息转入新一轮"})
		c.startRun(strings.Join(texts, "\n\n"))
	}
}

func isCanceledErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "context canceled")
}

// finishRun 退出前收尾：取消运行中回合、等运行 goroutine 真正落定（上限
// 2 秒，防个别工具无视取消），再清理工件目录——避免在工具写入途中删目录。
// 等待对象是 goroutine 的退出信号而非 running 标志：该标志只由事件循环
// 复位，而事件循环此刻正阻塞在本函数内，等它必然空转满 2 秒。
func (c *Chat) finishRun() {
	if c.cancel != nil {
		c.cancel()
	}
	if c.runDone != nil {
		select {
		case <-c.runDone:
		case <-time.After(2 * time.Second):
		}
	}
	c.app.Harness.Artifacts.Cleanup()
}
