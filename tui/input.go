// input.go 按键/鼠标分发与斜杠指令：模态（ask/计划/升审批/弹窗）优先
// 捕获按键；编辑键交给输入框；PgUp/PgDn/滚轮滚动内容区；Ctrl+C 中断
// 当前执行（空闲时顶栏提示后 3 秒内再按一次退出）；Ctrl+O 展开思考/工具块。
package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"git.gorun.fun/share/grf-harness/term"
)

// handleInput 分发一条终端输入事件；返回 true 表示退出。
func (c *Chat) handleInput(ev term.Event) bool {
	switch e := ev.(type) {
	case term.PasteEv:
		if c.modalInsert(e.Text) {
			c.paint(true)
			return false
		}
		if c.ed.Len() == 0 && strings.HasPrefix(e.Text, "!") {
			if c.bang {
				c.exitBang()
			} else {
				c.enterBang()
			}
			e.Text = strings.TrimPrefix(e.Text, "!")
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

// modalInsert 模态打开时插入文本（ask 文本框）。
func (c *Chat) modalInsert(text string) bool {
	switch {
	case c.ask != nil:
		c.ask.insert(text)
		return true
	}
	return false
}

// handleKey 按键分发：模态（ask/弹窗）优先捕获；其次控制键层
// （中断/退出、折叠、模式切换、提交、导航、翻页）；未消费的键交行编辑。
func (c *Chat) handleKey(k term.KeyEv) bool {
	// 模态优先
	if c.ask != nil {
		return c.askKey(k)
	}
	if c.pop != nil {
		return c.popKey(k)
	}
	if quit, handled := c.controlKey(k); handled {
		return quit
	}
	return c.lineEditKey(k)
}

// controlKey 控制键层。返回 handled=false 表示该键不属于本层，交行编辑。
func (c *Chat) controlKey(k term.KeyEv) (quit, handled bool) {
	switch k.Name {
	case "ctrl+c":
		return c.interruptKey(), true
	case "ctrl+d":
		if c.bang {
			return false, true
		}
		if c.ed.Len() == 0 && !c.running {
			return true, true
		}
		return false, false // 交行编辑层（无对应键，即忽略）
	case "ctrl+o":
		c.toggleLastFold()
		return false, true
	case "f2":
		c.app.Root().SetPlanMode(!c.app.Root().InPlanMode())
		return false, true
	case "enter":
		return c.enterKey(), true
	case "tab":
		if hints := c.slashHints(); len(hints) > 0 {
			fields := strings.Fields(hints[c.hintIdx])
			c.ed.SetValue(fields[0])
			c.hintIdx = 0
			return false, true
		}
		c.cycleMode()
		return false, true
	case "shift+tab":
		c.toggleMouse()
		return false, true
	case "up":
		c.navHintOrHist(-1)
		return false, true
	case "down":
		c.navHintOrHist(1)
		return false, true
	case "pgup":
		c.scrollUp += c.pageStep()
		return false, true
	case "pgdn":
		c.scrollUp -= c.pageStep()
		if c.scrollUp < 0 {
			c.scrollUp = 0
		}
		return false, true
	case "esc":
		c.scrollUp = 0
		return false, true
	}
	return false, false
}

// interruptKey Ctrl+C：执行中中断当前回合；空闲时顶栏提示后需在时限内
// 再按一次才退出。
func (c *Chat) interruptKey() bool {
	if c.running {
		if c.cancel != nil {
			c.cancel()
		}
		c.paint(true)
		return false
	}
	if time.Since(c.quitAt) < 3*time.Second {
		return true
	}
	c.quitAt = time.Now()
	c.status = "再按一次 Ctrl+C 退出"
	c.statusAt = c.quitAt.Add(3 * time.Second)
	return false
}

// enterKey 回车提交：命令模式下输入整体作为命令直执行；普通模式在指令
// 提示可见时先把选中的完整指令同步到输入框再执行。运行中同样提交——
// 纯文本经 submit 转为运行中插话（steer），指令各自有运行态守卫。
func (c *Chat) enterKey() bool {
	if c.bang {
		// 命令模式：输入整体作为命令直执行；执行后留在模式内
		// 便于连发命令，仅空输入框再按 ! 退出
		return c.submit("!" + c.ed.Value())
	}
	// 指令提示可见时：先把选中的完整指令同步到输入框再执行
	if hints := c.slashHints(); len(hints) > 0 {
		fields := strings.Fields(hints[c.hintIdx])
		c.ed.SetValue(fields[0])
		c.hintIdx = 0
	}
	return c.submit(c.ed.Value())
}

// toggleMouse Shift+Tab 开关鼠标交互并记忆偏好。
func (c *Chat) toggleMouse() {
	c.mouseOn = !c.mouseOn
	c.scr.SetMouse(c.mouseOn)
	c.saveMousePref(c.mouseOn)
	if c.mouseOn {
		c.status = "鼠标交互已开启：滚轮滚动、单击展开、拖选复制（Shift+Tab 关闭）"
	} else {
		c.status = "鼠标交互已关闭：用终端原生选择/复制（Shift+Tab 重新开启）"
	}
	c.statusAt = timeNow().Add(3 * time.Second)
}

// navHintOrHist 上/下方向键（dir=-1/+1）：指令提示可见时轮选提示，
// 否则走输入历史（命令模式下进出带 "!" 前缀，取值后光标落行尾）。
func (c *Chat) navHintOrHist(dir int) {
	if hints := c.slashHints(); len(hints) > 0 {
		c.hintIdx = (c.hintIdx + dir + len(hints)) % len(hints)
		return
	}
	base := c.ed.Value()
	if c.bang {
		base = "!" + base
	}
	var v string
	if dir < 0 {
		v = c.hist.up(base)
	} else {
		v = c.hist.down(base)
	}
	if c.bang {
		v = strings.TrimPrefix(v, "!")
	}
	c.ed.SetValue(v)
	c.ed.CursorEnd()
}

// lineEditKey 行编辑键：删除/光标移动/词跳转与字符插入（空输入框按
// "!" 切换命令模式）；其余键忽略。
func (c *Chat) lineEditKey(k term.KeyEv) bool {
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
			if k.Rune == '!' && c.ed.Len() == 0 {
				if c.bang {
					c.exitBang()
				} else {
					c.enterBang()
				}
			} else {
				c.ed.InsertRune(k.Rune)
			}
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

// enterBang 进入命令模式（输入框换 "!" 提示符，所输内容回车后直接
// 作为 shell 命令执行，不经模型）。
func (c *Chat) enterBang() {
	c.bang = true
	c.ed.Prompt = "! "
	c.hintIdx = 0
	c.status = "命令模式：回车直接执行，再按 ! 退出"
	c.statusAt = timeNow().Add(3 * time.Second)
}

// exitBang 退出命令模式。
func (c *Chat) exitBang() {
	c.bang = false
	c.ed.Prompt = "> "
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
		// 拖选：复制覆盖的内容行（与渲染共用同一份组帧结果，行号换算
		// 才不会因 live 段文案/视口预算的漂移取错行）
		lo, hi := c.dragA, c.dragB
		if lo > hi {
			lo, hi = hi, lo
		}
		f := c.frame()
		var picked []string
		for abs := lo; abs <= hi; abs++ {
			row := abs - f.base
			if row >= 0 && row < len(f.show) {
				picked = append(picked, term.StripANSI(f.show[row]))
			}
		}
		if len(picked) > 0 {
			osc52Copy(strings.Join(picked, "\n"))
			c.status = fmt.Sprintf("已复制 %d 行", len(picked))
			c.statusAt = timeNow().Add(3 * time.Second)
		}
	}
}

// absLine 显示行 Y → 内容绝对行号（与 frame 的滚动窗口共用同一预算）。
func (c *Chat) absLine(y int) int {
	return c.frame().base + y
}

// clickExpand 单击内容行：命中思考/工具块则展开/折叠（行→块映射来自
// 组帧结果，不重复 wrap）。
func (c *Chat) clickExpand(abs int) {
	idx, ok := c.frame().lineBlk[abs]
	if !ok || idx < 0 {
		return
	}
	if c.blocks[idx].kind == blkThink || c.blocks[idx].kind == blkTools {
		c.blocks[idx].expanded = !c.blocks[idx].expanded
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

	if strings.HasPrefix(input, "!") {
		c.execBang(strings.TrimSpace(strings.TrimPrefix(input, "!")))
		return false
	}
	if handled, quit := c.handleSlash(input); handled {
		return quit
	}
	if input == "/model" || strings.HasPrefix(input, "/model ") {
		// 回合运行中不切换：中途换模型会让同一回合前后半落在不同模型上
		//（身份行/子代理 client 的改写虽然并发安全，但行为不可预期）
		if c.running {
			c.emitBlock(block{kind: blkErr, text: "当前有任务在执行，等空闲后再切换模型"})
			return false
		}
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
		title, rerr := c.app.ResumeByIndex(n)
		if rerr != nil {
			c.emitBlock(block{kind: blkErr, text: rerr.Error()})
			return false
		}
		c.resetForReplay("已恢复会话: " + title)
		return false
	}
	if strings.HasPrefix(input, "/") {
		c.emitBlock(block{kind: blkErr, text: "未知指令 " + input + "（/help 查看可用指令）"})
		return false
	}
	if c.running {
		// 运行中插话（steer）：排队到下一个步骤边界注入，不打断在跑的
		// 工具；回合结束时仍未注入的由 finishStream 转为下一回合输入
		c.app.Root().Steer(input)
		if c.app.Root().IsRunning() {
			c.emitBlock(block{kind: blkDim, text: "· 已并入当前任务（当前步骤后生效）"})
		} else {
			c.emitBlock(block{kind: blkDim, text: "· 已排队（收尾中，稍后生效）"})
		}
		c.paint(true)
		return false
	}
	c.startRun(input)
	return false
}

// handleSlash 无参数的纯开关类斜杠指令；返回 handled=false 表示不属于
// 下表指令（/model 与 /resume 含弹窗与参数路径，留在 submit 处理）。
func (c *Chat) handleSlash(input string) (handled, quit bool) {
	switch input {
	case "/exit", "/quit":
		return true, true
	case "/new", "/clear":
		if c.running {
			c.emitBlock(block{kind: blkErr, text: "当前有任务在执行，等空闲后再开新会话"})
			return true, false
		}
		c.app.NewSession()
		c.blocks = nil
		c.todos = nil
		c.goalText = ""
		c.scrollUp = 0
		c.emitBlock(block{kind: blkDim, text: "已保存当前会话并开启新会话"})
		return true, false
	case "/help":
		// 指令集与 app.SlashCmds 表保持同步（表是前缀提示/补全的事实
		// 来源）；此处手写保留 TUI 特有的详细说明与快捷键/鼠标行
		c.emitBlock(block{kind: blkText, text: `/help   显示本帮助
!       空输入框按 ! 进入命令模式：所输命令回车直接执行（再按 ! 退出）
/init   分析代码库并生成/更新 AGENTS.md
/model  弹窗选择模型；/model 提供商/模型 直填切换
/resume 弹窗恢复历史会话；/resume N 按序号恢复
/plan   进入/退出计划模式（F2 同效）
/goal   查看当前 goal 状态
/compact 压缩会话历史为摘要（历史过长时也会在回合开始自动压缩）
/new    保存当前会话并清屏开新会话（/clear 同效）
/exit   退出（/quit 同效；Ctrl+C 空闲时按两次退出，Ctrl+D 同）
运行中输入消息回车 = 并入当前任务（当前步骤后生效，不打断在跑的工具）
快捷键：Ctrl+O 展开/折叠思考与工具块 · Tab 补全/切换模式（auto→计划→审批） · Shift+Tab 切换鼠标 · PgUp/PgDn 翻页
鼠标：滚轮滚动 · 单击展开 · 拖选复制`})
		return true, false
	case "/init":
		if c.running {
			c.emitBlock(block{kind: blkErr, text: "当前有任务在执行，等空闲后再 /init"})
			return true, false
		}
		c.emitBlock(block{kind: blkDim, text: "（初始化工作区：分析代码库并生成 AGENTS.md）"})
		c.startRun("初始化工作区说明：请先分析当前工作区的代码结构（目录布局、构建命令、测试命令、技术栈与架构约定），" +
			"然后在工作区根目录创建 AGENTS.md——面向 coding agent 的工作指南，包含：项目概述、构建与测试命令、" +
			"代码风格与关键约定、常见的坑。若 AGENTS.md 已存在，在其基础上核对并增补缺漏，不要整体重写。" +
			"注意：AGENTS.md 不会被自动注入上下文（保持默认提示词精简），后续会话的 agent 会按需自行读取，" +
			"因此内容务必精炼、可扫描、只写稳定事实。")
		return true, false
	case "/plan":
		c.app.Root().SetPlanMode(!c.app.Root().InPlanMode())
		return true, false
	case "/goal":
		if g := c.app.Harness.Goals.Marshal(); g != "" {
			c.emitBlock(block{kind: blkText, text: g})
		} else {
			c.emitBlock(block{kind: blkDim, text: "（当前无 goal）"})
		}
		return true, false
	case "/compact":
		if c.running {
			c.emitBlock(block{kind: blkErr, text: "当前有任务在执行，等空闲后再压缩"})
			return true, false
		}
		c.running = true
		c.emitBlock(block{kind: blkDim, text: "（压缩会话历史…）"})
		go func() {
			ctx, cancel := context.WithTimeout(c.ctx, 120*time.Second)
			summary, err := c.app.Root().Compact(ctx)
			cancel()
			select {
			case c.events <- compactDoneMsg{summary: summary, err: errStr(err)}:
			case <-c.ctx.Done():
			}
		}()
		return true, false
	}
	return false, false
}

// timeNow 可测试时间点。
var timeNow = func() time.Time { return time.Now() }
