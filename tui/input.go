// input.go 按键/鼠标分发与斜杠指令：模态（ask/计划/升审批/弹窗）优先
// 捕获按键；编辑键交给输入框；PgUp/PgDn/滚轮滚动内容区；Ctrl+C 中断
// 当前执行（空闲时顶栏提示后 3 秒内再按一次退出）；Ctrl+O 展开思考/工具块。
package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"grf/store"
	"grf/term"
)

// handleInput 分发一条终端输入事件；返回 true 表示退出。
func (c *Chat) handleInput(ev term.Event) bool {
	switch e := ev.(type) {
	case term.PasteEv:
		if c.modalInsert(e.Text) {
			c.paint(true)
			return false
		}
		c.ed.Insert(e.Text)
	case term.KeyEv:
		quit := c.handleKey(e)
		if !quit {
			c.paint(true) // 按键重绘不节流（低频且需要即时反馈）
		}
		return quit
	case term.MouseEv:
		c.handleMouse(e)
	}
	c.paint(false)
	return false
}

// modalInsert 模态打开时插入文本（ask 文本框 / 计划反馈框）。
func (c *Chat) modalInsert(text string) bool {
	switch {
	case c.ask != nil:
		c.ask.insert(text)
		return true
	case c.plan != nil:
		c.plan.insert(text)
		return true
	}
	return false
}

// handleKey 按键分发。
func (c *Chat) handleKey(k term.KeyEv) bool {
	// 模态优先
	if c.ask != nil {
		return c.askKey(k)
	}
	if c.plan != nil {
		return c.planKey(k)
	}
	if c.escv != nil {
		return c.escKey(k)
	}
	if c.pop != nil {
		return c.popKey(k)
	}

	switch k.Name {
	case "ctrl+c":
		if c.running {
			if c.cancel != nil {
				c.cancel()
			}
			c.paint(true)
			return false
		}
		// 空闲时按 Ctrl+C：顶栏提示后需在时限内再按一次才退出
		if time.Since(c.quitAt) < 3*time.Second {
			return true
		}
		c.quitAt = time.Now()
		c.status = "再按一次 Ctrl+C 退出"
		c.statusAt = c.quitAt.Add(3 * time.Second)
		return false
	case "ctrl+d":
		if c.ed.Len() == 0 && !c.running {
			return true
		}
	case "ctrl+o":
		c.toggleLastFold()
		return false
	case "f2":
		c.app.Root().SetPlanMode(!c.app.Root().InPlanMode())
		return false
	case "enter":
		if !c.running {
			// 指令提示可见时：先把选中的完整指令同步到输入框再执行
			if hints := c.slashHints(); len(hints) > 0 {
				fields := strings.Fields(hints[c.hintIdx])
				c.ed.SetValue(fields[0])
				c.hintIdx = 0
			}
			if c.submit(c.ed.Value()) {
				return true
			}
		}
		return false
	case "tab":
		if hints := c.slashHints(); len(hints) > 0 {
			fields := strings.Fields(hints[c.hintIdx])
			c.ed.SetValue(fields[0])
			c.hintIdx = 0
			return false
		}
		c.cycleMode()
		return false
	case "shift+tab":
		c.mouseOn = !c.mouseOn
		c.scr.SetMouse(c.mouseOn)
		c.saveMousePref(c.mouseOn)
		if c.mouseOn {
			c.status = "鼠标交互已开启：滚轮滚动、单击展开、拖选复制（Shift+Tab 关闭）"
		} else {
			c.status = "鼠标交互已关闭：用终端原生选择/复制（Shift+Tab 重新开启）"
		}
		c.statusAt = timeNow().Add(3 * time.Second)
		return false
	case "up":
		if hints := c.slashHints(); len(hints) > 0 {
			c.hintIdx = (c.hintIdx - 1 + len(hints)) % len(hints)
			return false
		}
		c.ed.SetValue(c.hist.up(c.ed.Value()))
		c.ed.CursorEnd()
		return false
	case "down":
		if hints := c.slashHints(); len(hints) > 0 {
			c.hintIdx = (c.hintIdx + 1) % len(hints)
			return false
		}
		c.ed.SetValue(c.hist.down(c.ed.Value()))
		c.ed.CursorEnd()
		return false
	case "pgup":
		c.scrollUp += c.pageStep()
		return false
	case "pgdn":
		c.scrollUp -= c.pageStep()
		if c.scrollUp < 0 {
			c.scrollUp = 0
		}
		return false
	case "esc":
		c.scrollUp = 0
		return false
	}

	// 行编辑键
	switch k.Name {
	case "backspace":
		c.ed.Backspace()
	case "delete":
		c.ed.Delete()
	case "left":
		c.ed.Left()
	case "right":
		c.ed.Right()
	case "home", "ctrl+a":
		c.ed.Home()
	case "end", "ctrl+e":
		c.ed.End()
	case "ctrl+k":
		c.ed.KillToEnd()
	case "ctrl+u":
		c.ed.KillToStart()
	case "ctrl+w":
		c.ed.KillWord()
	case "ctrl+b":
		c.ed.WordLeft()
	case "ctrl+f":
		c.ed.WordRight()
	default:
		if k.Rune != 0 {
			c.ed.InsertRune(k.Rune)
		} else {
			return false
		}
	}
	c.hintIdx = 0
	return false
}

func (c *Chat) pageStep() int {
	n := c.height - 6
	if n < 3 {
		n = 3
	}
	return n
}

// cycleMode Tab 循环权限模式：auto（默认）→ 计划模式 → 审批模式 → auto。
// 切换不打印内容行，模式提示由输入框上方状态栏展示。
func (c *Chat) cycleMode() {
	root := c.app.Root()
	switch {
	case !root.InPlanMode() && !root.ApproveEach():
		root.SetPlanMode(true)
	case root.InPlanMode():
		root.SetPlanMode(false)
		root.SetApproveMode(true)
	default:
		root.SetApproveMode(false)
	}
}

// toggleLastFold 展开最近的思考或工具合并块。
func (c *Chat) toggleLastFold() {
	for i := len(c.blocks) - 1; i >= 0; i-- {
		if c.blocks[i].kind == blkThink || c.blocks[i].kind == blkTools {
			c.blocks[i].expanded = !c.blocks[i].expanded
			return
		}
	}
	c.emitBlock(block{kind: blkDim, text: "（暂无可展开的思考/工具块）"})
}

// handleMouse 鼠标：滚轮滚动、单击展开、拖选复制。
func (c *Chat) handleMouse(m term.MouseEv) {
	switch m.Type {
	case "wheel":
		if m.Button == 0 {
			c.scrollUp += 3
		} else {
			c.scrollUp -= 3
			if c.scrollUp < 0 {
				c.scrollUp = 0
			}
		}
	case "press":
		if m.Button == 0 {
			// 点击 todo 面板：切换展开/收起（不进入拖选）
			if c.todoRowLo >= 0 && m.Y >= c.todoRowLo && m.Y < c.todoRowHi {
				c.todoOpen = !c.todoOpen
				return
			}
			c.dragging = true
			c.dragA = c.absLine(m.Y)
			c.dragB = c.dragA
		}
	case "motion":
		if c.dragging {
			if h := c.absLine(m.Y); h != c.dragB {
				c.dragB = h
			}
		}
	case "release":
		if !c.dragging {
			return
		}
		c.dragging = false
		if c.dragA == c.dragB {
			c.clickExpand(c.dragA)
			return
		}
		// 拖选：复制覆盖的内容行
		lo, hi := c.dragA, c.dragB
		if lo > hi {
			lo, hi = hi, lo
		}
		lines, _, _, _ := c.frameLines()
		var picked []string
		vp := c.height - 3
		if len(lines) < vp {
			vp = len(lines)
		}
		base := len(c.visibleContentLines()) - vp
		if base < 0 {
			base = 0
		}
		for abs := lo; abs <= hi; abs++ {
			row := abs - base
			if row >= 0 && row < len(lines)-3 && row < vp {
				picked = append(picked, term.StripANSI(lines[row]))
			}
		}
		if len(picked) > 0 {
			osc52Copy(strings.Join(picked, "\n"))
			c.status = fmt.Sprintf("已复制 %d 行", len(picked))
			c.statusAt = timeNow().Add(3 * time.Second)
		}
	}
}

// visibleContentLines 内容区总行数（按当前宽度；用于绝对行号换算）。
func (c *Chat) visibleContentLines() []string {
	w := c.width - 2
	if w < 4 {
		w = 4
	}
	var lines []string
	for _, b := range c.blocks {
		lines = append(lines, term.Wrap(renderBlock(b), w)...)
	}
	if c.running {
		for _, sv := range c.hub.segViews() {
			text := sv.text
			if sv.think {
				text = fmt.Sprintf("💭 思考 %ds，%d 字", sv.secs, sv.chars)
			}
			lines = append(lines, term.Wrap(text, w)...)
		}
	}
	return lines
}

// absLine 显示行 Y → 内容绝对行号。
func (c *Chat) absLine(y int) int {
	lines := c.visibleContentLines()
	vp := c.height - 3
	if len(lines) > vp {
		from := len(lines) - vp - c.scrollUp
		if from < 0 {
			from = 0
		}
		return from + y
	}
	return y
}

// clickExpand 单击内容行：命中思考/工具块则展开/折叠。
func (c *Chat) clickExpand(absLine int) {
	w := c.width - 2
	if w < 4 {
		w = 4
	}
	pos := 0
	for i, b := range c.blocks {
		h := len(term.Wrap(renderBlock(b), w))
		if absLine >= pos && absLine < pos+h {
			if b.kind == blkThink || b.kind == blkTools {
				c.blocks[i].expanded = !c.blocks[i].expanded
			}
			return
		}
		pos += h
	}
}

// submit 提交输入；返回 true 表示请求退出。
func (c *Chat) submit(raw string) bool {
	input := strings.TrimSpace(raw)
	c.ed.Reset()
	c.hintIdx = 0
	if input == "" {
		return false
	}
	c.hist.add(input)
	c.emitBlock(block{kind: blkText, text: "> " + input})
	c.scrollUp = 0

	switch input {
	case "/exit", "/quit":
		return true
	case "/new", "/clear":
		if c.running {
			c.emitBlock(block{kind: blkErr, text: "当前有任务在执行，等空闲后再开新会话"})
			return false
		}
		c.app.NewSession()
		c.blocks = nil
		c.todos = nil
		c.goalText = ""
		c.scrollUp = 0
		c.emitBlock(block{kind: blkDim, text: "已保存当前会话并开启新会话"})
		return false
	case "/help":
		c.emitBlock(block{kind: blkText, text: `/help   显示本帮助
/init   分析代码库并生成/更新 AGENTS.md
/model  弹窗选择模型；/model 提供商/模型 直填切换
/resume 弹窗恢复历史会话；/resume N 按序号恢复
/plan   进入/退出计划模式（F2 同效）
/goal   查看当前 goal 状态
/new    保存当前会话并清屏开新会话（/clear 同效）
/exit   退出（/quit 同效；Ctrl+C 空闲时按两次退出，Ctrl+D 同）
快捷键：Ctrl+O 展开/折叠思考与工具块 · Tab 补全/切换模式（auto→计划→审批） · Shift+Tab 切换鼠标 · PgUp/PgDn 翻页
鼠标：滚轮滚动 · 单击展开 · 拖选复制`})
		return false
	case "/init":
		if c.running {
			c.emitBlock(block{kind: blkErr, text: "当前有任务在执行，等空闲后再 /init"})
			return false
		}
		c.emitBlock(block{kind: blkDim, text: "（初始化工作区：分析代码库并生成 AGENTS.md）"})
		c.startRun("初始化工作区说明：请先分析当前工作区的代码结构（目录布局、构建命令、测试命令、技术栈与架构约定），" +
			"然后在工作区根目录创建 AGENTS.md——面向 coding agent 的工作指南，包含：项目概述、构建与测试命令、" +
			"代码风格与关键约定、常见的坑。若 AGENTS.md 已存在，在其基础上核对并增补缺漏，不要整体重写。" +
			"注意：AGENTS.md 不会被自动注入上下文（保持默认提示词精简），后续会话的 agent 会按需自行读取，" +
			"因此内容务必精炼、可扫描、只写稳定事实。")
		return false
	case "/plan":
		c.app.Root().SetPlanMode(!c.app.Root().InPlanMode())
		return false
	case "/goal":
		if g := c.app.Harness.Goals.Marshal(); g != "" {
			c.emitBlock(block{kind: blkText, text: g})
		} else {
			c.emitBlock(block{kind: blkDim, text: "（当前无 goal）"})
		}
		return false
	}
	if input == "/model" || strings.HasPrefix(input, "/model ") {
		arg := strings.TrimSpace(strings.TrimPrefix(input, "/model"))
		if arg == "" {
			c.openModelPopup()
			return false
		}
		if text, err := c.app.ModelCmd(arg); err != nil {
			c.emitBlock(block{kind: blkErr, text: "切换模型失败: " + err.Error()})
		} else {
			c.emitBlock(block{kind: blkText, text: text})
		}
		return false
	}
	if input == "/resume" || strings.HasPrefix(input, "/resume ") {
		arg := strings.TrimSpace(strings.TrimPrefix(input, "/resume"))
		if arg == "" {
			c.openResumePopup()
			return false
		}
		n, err := strconv.Atoi(arg)
		if err != nil || n < 1 {
			c.emitBlock(block{kind: blkErr, text: "用法: /resume N（N 为列表序号）"})
			return false
		}
		sessions := c.app.Harness.Store.ListSessions(0)
		if n > len(sessions) {
			c.emitBlock(block{kind: blkErr, text: fmt.Sprintf("序号超出范围（共 %d 条会话）", len(sessions))})
			return false
		}
		sess, rerr := store.LoadSessionFull(sessions[n-1].Path)
		if rerr != nil {
			c.emitBlock(block{kind: blkErr, text: "读取会话失败: " + rerr.Error()})
			return false
		}
		c.app.Restore(sess)
		c.resetForReplay("已恢复会话: " + sessions[n-1].Title)
		return false
	}
	if strings.HasPrefix(input, "/") {
		c.emitBlock(block{kind: blkErr, text: "未知指令 " + input + "（/help 查看可用指令）"})
		return false
	}
	c.startRun(input)
	return false
}

// timeNow 可测试时间点。
var timeNow = func() time.Time { return time.Now() }
