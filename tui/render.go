// render.go 整帧渲染：内容块模型（block）与单块文本、frameLines 组帧
//（内容区 + 弹窗 + todo 面板 + 状态分隔线 + 输入区）、统计底栏、斜杠
// 指令提示、ANSI 样式与 OSC52 剪贴板。块模型由 chat.go 的事件循环维护。
package tui

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"time"

	"git.gorun.fun/share/grf-harness/term"
)

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

// renderBlock 单块文本（未换行）。
func renderBlock(b block) string {
	switch b.kind {
	case blkText:
		return strings.TrimRight(b.text, "\n")
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
				add(strings.TrimRight(sv.text, "\n"), -1)
				continue
			}
			line := fmt.Sprintf("💭 思考 %ds，%d 字", sv.secs, sv.chars)
			if sv.live {
				line = fmt.Sprintf("💭 思考中… %ds", sv.secs)
				if tail := term.LastLine(sv.text); tail != "" {
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
	if c.bang {
		statusParts = append(statusParts, bold("命令模式"))
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

// slashCmds 前缀匹配指令表；alias 为同效别名（提示列表合并成一行展示，
// 前缀匹配别名也会命中主指令）。
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
	if c.bang {
		return nil
	}
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
