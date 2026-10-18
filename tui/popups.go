// popups.go 模态组件：通用列表弹窗（模型选择/会话恢复）、ask_user_question
// 表单（单选/多选/自由输入）、exit_plan_mode 计划审批、沙箱升审批。
package tui

import (
	"fmt"
	"strings"

	"git.gorun.fun/share/grf-harness/term"
	"git.gorun.fun/share/grf-harness/tools"
)

// ---- 通用列表弹窗 ----

type popup struct {
	title  string
	items  []string
	sel    int
	top    int    // 可视窗口首项（长列表滚动）
	mark   string // 打 ✓ 的项
	onDone func(idx int)
}

// render 列表超出 maxH 行时只显示可视窗口，选中项始终可见，
// 上下被截断处以 "↑/↓ 剩余 N 项" 提示。
func (p *popup) render(w, maxH int) []string {
	if maxH < 4 {
		maxH = 4
	}
	lines := []string{dim(p.title + "（↑↓ 选择 · 回车确认 · Esc 取消）")}
	n := len(p.items)
	vis := n
	if n > maxH-1 {
		vis = maxH - 3 // 标题 + 上下两条截断提示
		if vis < 1 {
			vis = 1
		}
	}
	if p.sel < p.top {
		p.top = p.sel
	}
	if p.sel >= p.top+vis {
		p.top = p.sel - vis + 1
	}
	if p.top > n-vis {
		p.top = n - vis
	}
	if p.top < 0 {
		p.top = 0
	}
	if vis < n && p.top > 0 {
		lines = append(lines, dim(fmt.Sprintf("  ↑ 还有 %d 项", p.top)))
	}
	for i := p.top; i < p.top+vis && i < n; i++ {
		it := p.items[i]
		name := it
		if it == p.mark {
			name += " ✓"
		}
		if i == p.sel {
			lines = append(lines, cyan("❯ ")+bold(term.ClipRunes(name, w-4)))
		} else {
			lines = append(lines, "  "+term.ClipRunes(name, w-4))
		}
	}
	if vis < n && p.top+vis < n {
		lines = append(lines, dim(fmt.Sprintf("  ↓ 还有 %d 项", n-p.top-vis)))
	}
	return lines
}

// ---- ask 表单 ----

type askForm struct {
	qs      []tools.Question
	cur     int
	sel     int
	checked map[int]bool
	custom  bool    // 有选项问题切到"✎ 自定义输入"行后的手输模式
	text    *Editor // 无选项问题与自定义输入的文本输入
	answers map[string]any
	// picked 每题作答来源：选中选项的下标；-1 = 文本作答（自定义输入/
	// 无选项题；多选无单一选中下标也记 -1）。审批类消费方（host.go）
	// 据此按下标判定，避免自定义输入恰好与选项文案相同或选项改文案
	// 导致的误判。
	picked map[string]int
	head   string // 标题行前缀（空 = ask_user_question）
	done   func(answers map[string]any, picked map[string]int, err error)
}

func newAskForm(qs []tools.Question, done func(map[string]any, map[string]int, error)) *askForm {
	f := &askForm{qs: qs, checked: map[int]bool{}, answers: map[string]any{},
		picked: map[string]int{}, done: done}
	f.text = NewEditor("答: ")
	return f
}

func (f *askForm) q() tools.Question { return f.qs[f.cur] }

func (f *askForm) insert(s string) {
	if len(f.q().Options) == 0 || f.custom {
		f.text.Insert(s)
	}
}

// customRow 自定义输入行（选项列表末尾的伪选项，sel 指向 len(Options)）。
const customRow = "✎ 自定义输入"

func (f *askForm) render(w int) []string {
	var lines []string
	header := fmt.Sprintf("问题 %d/%d", f.cur+1, len(f.qs))
	if h := f.q().Header; h != "" {
		header += " · " + h
	}
	head := f.head
	if head == "" {
		head = "ask_user_question"
	}
	lines = append(lines, dim("── "+head+" · "+header+" ──"))
	for _, ln := range term.Wrap(f.q().Question, w-2) {
		lines = append(lines, " "+ln)
	}
	if len(f.q().Options) == 0 || f.custom {
		t, _ := f.text.Render(w - 2)
		hint := "（回车提交 · Esc 取消全部提问）"
		if f.custom {
			hint = "（回车提交自定义答案 · Esc 返回选项）"
		}
		lines = append(lines, " "+t, dim("  "+hint))
		return lines
	}
	for i, opt := range f.q().Options {
		label := "○ " + opt.Label
		if f.q().MultiSelect {
			label = "□ " + opt.Label
			if f.checked[i] {
				label = "☑ " + opt.Label
			}
		}
		if i == f.sel {
			label = cyan("❯ ") + bold(label)
		} else {
			label = "  " + label
		}
		if opt.Description != "" {
			label += dim(" — " + term.ClipRunes(opt.Description, w-term.StringWidth(opt.Label)-10))
		}
		lines = append(lines, term.ClipRunes(label, w))
	}
	if f.sel == len(f.q().Options) {
		lines = append(lines, term.ClipRunes(cyan("❯ ")+bold(customRow), w))
	} else {
		lines = append(lines, term.ClipRunes("  "+customRow, w))
	}
	hint := "↑↓ 选择 · 回车确认 · ✎ 行改手输"
	if f.q().MultiSelect {
		hint = "空格 勾选 · ↑↓ 移动 · 回车提交 · ✎ 行改手输"
	}
	lines = append(lines, dim("  "+hint+" · Esc 取消全部提问"))
	return lines
}

// finishQuestion 记录当前问题答案并前进；全部完成时回调。
// 自定义输入（含无选项问题）以文本作答，多选拆勾选标签、单选取选中标签。
func (f *askForm) finishQuestion() {
	q := f.q()
	switch {
	case len(q.Options) == 0 || f.custom:
		f.answers[q.ID] = strings.TrimSpace(f.text.Value())
		f.picked[q.ID] = -1
	case q.MultiSelect:
		var labels []string
		for i, opt := range q.Options {
			if f.checked[i] {
				labels = append(labels, opt.Label)
			}
		}
		if len(labels) == 0 && f.sel < len(q.Options) {
			labels = append(labels, q.Options[f.sel].Label)
		}
		f.answers[q.ID] = labels
		f.picked[q.ID] = -1
	default: // 单选取选中标签（sel==选项数时走的是上面的自定义输入分支）
		f.answers[q.ID] = q.Options[f.sel].Label
		f.picked[q.ID] = f.sel
	}
	f.cur++
	f.sel = 0
	f.checked = map[int]bool{}
	f.custom = false
	f.text.Reset()
	if f.cur >= len(f.qs) {
		f.done(f.answers, f.picked, nil)
	}
}

// ---- 工具审批 ----
// （沙箱升级审批已复用 ask 表单，见 host.go 的 escRequest 分支）
