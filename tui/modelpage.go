// modelpage.go `gh model` 全屏配置页：提供商列表 → 模型列表（异步
// GET /models 检测，手动添加，删除）；添加提供商走表单流程。非 TTY 时
// 由 main 回退到编号输入（脚本化路径）。
package tui

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"grf/config"
	"grf/llm"
	"grf/term"
)

// ModelPage 模型配置页状态机。
type ModelPage struct {
	scr         *term.Screen
	rd          *term.Reader
	mode        string // "providers" | "models" | "form" | "input-model" | "delete-confirm"
	providerSel int    // 进入模型页时选中的提供商下标

	sel     int
	models  []string // 检测出的模型（含手动）
	loading bool
	loadErr string
	form    *formState
	status  string
	quit    bool

	// fetchDone 承载异步检测结果；nil 表示没有在途请求（nil channel
	// 在 select 里永久阻塞，可安全常驻 select 分支）。
	fetchDone chan fetchResult
}

// fetchResult 一次 GET /models 的结果。
type fetchResult struct {
	models []string
	err    error
}

type formState struct {
	fields []formField
	cur    int
}

type formField struct {
	label string
	value *Editor
}

// RunModelPage 启动模型配置页（全屏 TTY）；修改经 config 落盘，返回
// 启动/运行错误。
func RunModelPage() error {
	scr := term.NewScreen()
	if err := scr.Start(false); err != nil {
		return fmt.Errorf("进入全屏失败: %w", err)
	}
	defer scr.Stop()
	rd := term.ReadInput(os.Stdin)
	defer rd.Close()

	mp := &ModelPage{scr: scr, rd: rd, mode: "providers"}
	mp.paint()
	for {
		select {
		case <-scr.ResizeCh():
			mp.paint()
		case r := <-mp.fetchDone:
			// 检测结果只能在本 goroutine 应用（单 goroutine 状态规则）
			mp.fetchDone = nil
			mp.loading = false
			if r.err != nil {
				mp.loadErr = r.err.Error()
			} else {
				mp.loadErr = ""
				mp.models = mp.applyFetched(r.models)
				if mp.sel >= len(mp.models) {
					mp.sel = len(mp.models) - 1
				}
				if mp.sel < 0 {
					mp.sel = 0
				}
			}
			mp.paint()
		case ev := <-rd.Events():
			if ev == nil {
				return nil
			}
			mp.handle(ev)
			if mp.quit {
				return nil
			}
			mp.paint()
		}
	}
}

func (m *ModelPage) settings() *config.Settings {
	s, _ := config.LoadFileForUpdate()
	return s
}

func (m *ModelPage) handle(ev term.Event) {
	switch e := ev.(type) {
	case term.PasteEv:
		if m.form != nil {
			m.form.fields[m.form.cur].value.Insert(e.Text)
		}
	case term.KeyEv:
		m.key(e)
	case term.MouseEv:
	}
}

func (m *ModelPage) key(k term.KeyEv) {
	if k.Name == "ctrl+c" {
		m.quit = true
		return
	}
	switch m.mode {
	case "providers":
		m.providersKey(k)
	case "models":
		m.modelsKey(k)
	case "input-model":
		m.inputModelKey(k)
	case "delete-confirm":
		m.deleteKey(k)
	default: // form:name / form:url / ...
		m.formKey(k)
	}
}

// ---- 提供商列表 ----

func (m *ModelPage) providerRows() []string {
	s := m.settings()
	var rows []string
	for _, name := range s.SortedProviderNames() {
		p := s.Providers[name]
		mark := "  "
		if strings.HasPrefix(s.Default, name+"/") {
			mark = "* "
		}
		models := len(p.Models)
		rows = append(rows, fmt.Sprintf("%s%s — [%s]（%d 个模型）", mark, name, p.BaseURL, models))
	}
	rows = append(rows, "＋ 添加提供商")
	return rows
}

func (m *ModelPage) providersKey(k term.KeyEv) {
	rows := len(m.providerRows())
	switch k.Name {
	case "up":
		if m.sel > 0 {
			m.sel--
		}
	case "down":
		if m.sel < rows-1 {
			m.sel++
		}
	case "enter":
		s := m.settings()
		if m.sel == rows-1 {
			m.form = &formState{fields: []formField{
				{label: "名称（如 deepseek）", value: NewEditor("")},
				{label: "base_url（如 https://api.deepseek.com/v1）", value: NewEditor("")},
				{label: "api_key（可空）", value: NewEditor("")},
			}}
			m.mode = "form:0"
			m.sel = 0
			return
		}
		names := s.SortedProviderNames()
		if m.sel < len(names) {
			m.providerSel = m.sel
			m.mode = "models"
			m.sel = 0
			// 注意用 providerSel 取：此时 sel 已重置为 0
			m.models = append([]string(nil), s.Providers[names[m.providerSel]].Models...)
			m.fetchModels(names[m.providerSel])
		}
	case "q", "esc":
		m.quit = true
	}
}

// curProvider 当前选中的提供商名（models 模式）。
func (m *ModelPage) curProvider() string {
	s := m.settings()
	names := s.SortedProviderNames()
	// providers 列表里选中的下标在进入 models 时保存于 providerSel
	if m.providerSel < len(names) {
		return names[m.providerSel]
	}
	return ""
}

// fetchModels 异步检测模型列表。
func (m *ModelPage) fetchModels(name string) {
	s := m.settings()
	p := s.Providers[name]
	m.loading = true
	m.loadErr = ""
	ch := make(chan fetchResult, 1)
	m.fetchDone = ch
	go func() {
		client := llm.NewClient(llm.Config{BaseURL: p.BaseURL, APIKey: p.APIKey, Model: ""})
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		models, err := client.Models(ctx)
		ch <- fetchResult{models: models, err: err}
	}()
}

// applyFetched 应用检测结果：以服务端为准并保留本地手动添加过的模型，
// 写回 settings 持久化（否则 `gh model list` 看不到检测出的模型）。
func (m *ModelPage) applyFetched(server []string) []string {
	s := m.settings()
	p := s.Providers[m.curProvider()]
	if p == nil {
		return server
	}
	merged := append([]string(nil), server...)
	for _, mm := range p.Models {
		if !containsStr(merged, mm) {
			merged = append(merged, mm)
		}
	}
	p.Models = merged
	_ = s.Save()
	return merged
}

func containsStr(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func (m *ModelPage) modelsKey(k term.KeyEv) {
	switch k.Name {
	case "up":
		if m.sel > 0 {
			m.sel--
		}
	case "down":
		if m.sel < len(m.modelRows())-1 {
			m.sel++
		}
	case "enter":
		rows := m.modelRows()
		if m.sel < len(rows)-3 {
			// 选了一个模型 → 设默认并退出
			model := m.models[m.sel]
			s := m.settings()
			s.Default = m.curProvider() + "/" + model
			if err := s.Save(); err != nil {
				m.status = "保存失败: " + err.Error()
				return
			}
			m.status = "已设默认: " + s.Default
			m.quit = true
		} else if m.sel == len(rows)-3 {
			m.fetchModels(m.curProvider()) // 重新获取
		} else if m.sel == len(rows)-2 {
			m.mode = "input-model" // 输入模型
			m.form = &formState{fields: []formField{{label: "模型名", value: NewEditor("")}}}
		} else {
			m.mode = "delete-confirm" // 删除提供商
		}
	case "r":
		m.fetchModels(m.curProvider())
	case "esc":
		m.mode = "providers"
		m.sel = 0
	}
}

func (m *ModelPage) modelRows() []string {
	var rows []string
	rows = append(rows, m.models...)
	rows = append(rows, "⟳ 重新获取（以服务端为准）", "✎ 输入模型名", "🗑 删除这个提供商")
	return rows
}

func (m *ModelPage) inputModelKey(k term.KeyEv) {
	f := m.form
	switch k.Name {
	case "enter":
		name := strings.TrimSpace(f.fields[0].value.Value())
		if name != "" {
			s := m.settings()
			if s.Providers[m.curProvider()].AddModel(name) {
				_ = s.Save()
			}
			m.models = append(m.models, name)
			sort.Strings(m.models)
		}
		m.mode = "models"
	case "esc":
		m.mode = "models"
	default:
		editKey(f.fields[0].value, k)
	}
}

func (m *ModelPage) deleteKey(k term.KeyEv) {
	switch k.Name {
	case "y", "Y":
		s := m.settings()
		delete(s.Providers, m.curProvider())
		if strings.HasPrefix(s.Default, m.curProvider()+"/") {
			s.Default = ""
		}
		_ = s.Save()
		m.mode = "providers"
		m.sel = 0
	case "n", "N", "esc":
		m.mode = "models"
	}
}

// ---- 表单（添加提供商）----

func (m *ModelPage) formKey(k term.KeyEv) {
	f := m.form
	switch k.Name {
	case "up", "shift+tab":
		if f.cur > 0 {
			f.cur--
		}
	case "down", "tab":
		if f.cur < len(f.fields)-1 {
			f.cur++
		}
	case "enter":
		if f.cur < len(f.fields)-1 {
			f.cur++
			return
		}
		name := strings.TrimSpace(f.fields[0].value.Value())
		url := strings.TrimSpace(f.fields[1].value.Value())
		key := strings.TrimSpace(f.fields[2].value.Value())
		if name == "" || url == "" {
			m.status = "名称与 base_url 不能为空"
			return
		}
		s := m.settings()
		if s.Providers == nil {
			s.Providers = map[string]*config.Provider{}
		}
		p := s.Providers[name]
		if p == nil {
			p = &config.Provider{}
			s.Providers[name] = p
		}
		p.BaseURL = url
		if key != "" {
			p.APIKey = key
		}
		if err := s.Save(); err != nil {
			m.status = "保存失败: " + err.Error()
			return
		}
		m.mode = "providers"
		m.form = nil
		m.status = "已添加 " + name + "（回车进入选择默认模型）"
	case "esc":
		m.mode = "providers"
		m.form = nil
	default:
		editKey(f.fields[f.cur].value, k)
	}
}

// listWindow 计算列表滚动窗口：返回可见下标区间与上/下方是否还有内容。
func listWindow(total, sel, avail int) (start, end int, up, down bool) {
	if avail < 1 {
		avail = 1
	}
	if total <= avail {
		return 0, total, false, false
	}
	start = sel - avail/2
	if start < 0 {
		start = 0
	}
	if start+avail > total {
		start = total - avail
	}
	return start, start + avail, start > 0, start+avail < total
}

// statusLines 底部状态行占用的行数（含空行）。
func (m *ModelPage) statusLines() int {
	if m.status != "" {
		return 2
	}
	return 0
}

// ---- 渲染 ----

func (m *ModelPage) paint() {
	w, h := m.scr.Size()
	if w < 20 {
		w = 20
	}
	cw := w - 4
	var body []string
	add := func(lines ...string) { body = append(body, lines...) }

	add(bold(" 模型配置"), dim(strings.Repeat("─", cw-4)))
	switch m.mode {
	case "providers", "":
		add(dim(" ↑↓ 选择 · Enter 进入/添加 · q 退出"))
		add("")
		rows := m.providerRows()
		avail := (h - 1) - len(body) - m.statusLines()
		scrollable := len(rows) > avail
		if scrollable {
			avail-- // 给 "↓ …" 提示行留位置
		}
		start, end, up, down := listWindow(len(rows), m.sel, avail)
		if scrollable && up {
			add(dim("  ↑ …"))
		}
		for i := start; i < end; i++ {
			if i == m.sel {
				add(cyan("❯ ") + clipRunesT(rows[i], cw-4))
			} else {
				add("  " + clipRunesT(rows[i], cw-4))
			}
		}
		if scrollable && down {
			add(dim("  ↓ …"))
		}
	case "models":
		title := " " + m.curProvider() + " 的模型"
		add(bold(title))
		if m.loading {
			add(dim(" ⟳ 正在检测服务端模型（GET /models）…"))
		}
		if m.loadErr != "" {
			add(red(" 检测失败: " + clipRunesT(m.loadErr, cw-8)))
		}
		add(dim(" Enter 设为默认 · r 重新获取 · Esc 返回"))
		add("")
		// 模型列表滚动，底部三个操作项固定可见
		const actions = 3
		avail := (h - 1) - len(body) - actions - m.statusLines()
		scrollable := len(m.models) > avail
		if scrollable {
			avail--
		}
		start, end, up, down := listWindow(len(m.models), m.sel, avail)
		if scrollable && up {
			add(dim("  ↑ …"))
		}
		for i := start; i < end; i++ {
			if i == m.sel {
				add(cyan("❯ ") + clipRunesT(m.models[i], cw-4))
			} else {
				add("  " + clipRunesT(m.models[i], cw-4))
			}
		}
		if scrollable && down {
			add(dim("  ↓ …"))
		}
		actionRows := []string{"⟳ 重新获取（以服务端为准）", "✎ 输入模型名", "🗑 删除这个提供商"}
		for j, row := range actionRows {
			i := len(m.models) + j
			if i == m.sel {
				add(cyan("❯ ") + clipRunesT(row, cw-4))
			} else {
				add("  " + clipRunesT(row, cw-4))
			}
		}
	case "input-model":
		add(dim(" 输入模型名（Enter 确认 · Esc 取消）"))
		add("")
		t, _ := m.form.fields[0].value.Render(cw - 8)
		add("  模型名: " + t)
	case "delete-confirm":
		add(red(" 确认删除提供商 " + m.curProvider() + " ？"))
		add(dim(" y 删除 · n/Esc 取消"))
	default:
		if m.form != nil {
			add(dim(" 添加提供商（Enter 下一项/提交 · Esc 取消）"))
			add("")
			for i, f := range m.form.fields {
				t, _ := f.value.Render(cw - 10)
				prefix := "  "
				if i == m.form.cur {
					prefix = cyan("❯ ")
				}
				add(prefix + clipRunesT(f.label+": "+t, cw))
			}
		}
	}
	if m.status != "" {
		add("")
		add(dim(" " + m.status))
	}
	lines := append([]string{}, body...)
	for len(lines) < h-1 {
		lines = append(lines, "")
	}
	lines = lines[:h-1]
	lines = append(lines, dim(strings.Repeat("─", w-1)))
	// 光标：表单当前输入框（列 = 前缀2列 + 标签宽 + Render 返回的
	// 窗口内光标列——后者已扣掉横向滚动的偏移）
	curRow, curCol := -1, 0
	if (strings.HasPrefix(m.mode, "form:") || m.mode == "input-model") && m.form != nil {
		if m.mode == "input-model" {
			curRow = 4
			_, col := m.form.fields[0].value.Render(cw - 8)
			curCol = 2 + term.StringWidth("模型名: ") + col
		} else {
			idx := m.form.cur
			curRow = 4 + idx
			_, col := m.form.fields[idx].value.Render(cw - 10)
			curCol = 2 + term.StringWidth(m.form.fields[idx].label+": ") + col
		}
	}
	m.scr.Paint(lines, curRow, curCol)
}
