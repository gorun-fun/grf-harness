// Package tui 全屏交互界面：基于 term 包（零依赖）实现的聊天 REPL、
// ask 表单、计划审批、模型配置页。终端尺寸变化时整帧按新宽度重排重绘。
package tui

import (
	"strings"

	"grf/term"
)

// Editor 单行输入编辑器（UTF-8 感知，横向滚动窗口）。
type Editor struct {
	buf    []rune
	cur    int // 光标位置（rune 下标）
	Prompt string
}

func NewEditor(prompt string) *Editor { return &Editor{Prompt: prompt} }

func (e *Editor) Value() string { return string(e.buf) }

func (e *Editor) Len() int { return len(e.buf) }

func (e *Editor) SetValue(s string) {
	e.buf = []rune(s)
	e.CursorEnd()
}

func (e *Editor) Reset() {
	e.buf = e.buf[:0]
	e.cur = 0
}

func (e *Editor) CursorEnd() { e.cur = len(e.buf) }

// CurWidth 光标前内容的显示宽度。
func (e *Editor) CurWidth() int { return term.StringWidth(string(e.buf[:e.cur])) }

// Insert 在光标处插入文本（粘贴的换行替换为空格）。
func (e *Editor) Insert(s string) {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", " "), "\n", " ")
	e.buf = append(e.buf[:e.cur], append([]rune(s), e.buf[e.cur:]...)...)
	e.cur += len([]rune(s))
}

func (e *Editor) InsertRune(r rune) {
	e.buf = append(e.buf[:e.cur], append([]rune{r}, e.buf[e.cur:]...)...)
	e.cur++
}

func (e *Editor) Backspace() {
	if e.cur == 0 {
		return
	}
	e.buf = append(e.buf[:e.cur-1], e.buf[e.cur:]...)
	e.cur--
}

func (e *Editor) Delete() {
	if e.cur >= len(e.buf) {
		return
	}
	e.buf = append(e.buf[:e.cur], e.buf[e.cur+1:]...)
}

func (e *Editor) Left() {
	if e.cur > 0 {
		e.cur--
	}
}

func (e *Editor) Right() {
	if e.cur < len(e.buf) {
		e.cur++
	}
}

func (e *Editor) Home() { e.cur = 0 }

func (e *Editor) End() { e.cur = len(e.buf) }

// KillToEnd Ctrl+K：删到行尾。
func (e *Editor) KillToEnd() { e.buf = e.buf[:e.cur] }

// KillToStart Ctrl+U：删到行首。
func (e *Editor) KillToStart() {
	e.buf = e.buf[e.cur:]
	e.cur = 0
}

// KillWord Ctrl+W：删除光标前一个词。
func (e *Editor) KillWord() {
	i := e.cur
	for i > 0 && e.buf[i-1] == ' ' {
		i--
	}
	for i > 0 && e.buf[i-1] != ' ' {
		i--
	}
	e.buf = append(e.buf[:i], e.buf[e.cur:]...)
	e.cur = i
}

// WordLeft / WordRight 词移动（Alt+B/F / Ctrl+B/F 交给词移动）。
func (e *Editor) WordLeft() {
	i := e.cur
	for i > 0 && e.buf[i-1] == ' ' {
		i--
	}
	for i > 0 && e.buf[i-1] != ' ' {
		i--
	}
	e.cur = i
}

func (e *Editor) WordRight() {
	i := e.cur
	for i < len(e.buf) && e.buf[i] == ' ' {
		i++
	}
	for i < len(e.buf) && e.buf[i] != ' ' {
		i++
	}
	e.cur = i
}

// Render 渲染为 (显示文本, 光标列)。宽度不足时横向滚动窗口。
func (e *Editor) Render(width int) (string, int) {
	promptW := term.StringWidth(e.Prompt)
	avail := width - promptW
	if avail < 1 {
		avail = 1
	}
	// 光标相对窗口
	left := 0
	curW := term.StringWidth(string(e.buf[:e.cur]))
	totalW := term.StringWidth(string(e.buf))
	if totalW <= avail {
		left = 0
	} else if curW > avail {
		left = totalW - avail
		if curW-avail < left {
			left = curW - avail
		}
	}
	// 按显示宽度切片
	seg := sliceByWidth(string(e.buf), left, left+avail)
	cursorCol := promptW + curW - left
	if cursorCol < 0 {
		cursorCol = 0
	}
	return e.Prompt + seg, cursorCol
}

// sliceByWidth 按显示宽度截取子串。
func sliceByWidth(s string, from, to int) string {
	var out []rune
	w := 0
	for _, r := range s {
		rw := term.RuneWidth(r)
		if w+rw > from {
			if w+rw > to {
				break
			}
			out = append(out, r)
		}
		w += rw
	}
	return string(out)
}

// editKey 把行编辑按键分发给编辑器（模态内输入框复用）。
func editKey(e *Editor, k termKey) {
	switch k.Name {
	case "backspace":
		e.Backspace()
	case "delete":
		e.Delete()
	case "left":
		e.Left()
	case "right":
		e.Right()
	case "home", "ctrl+a":
		e.Home()
	case "end", "ctrl+e":
		e.End()
	case "ctrl+k":
		e.KillToEnd()
	case "ctrl+u":
		e.KillToStart()
	case "ctrl+w":
		e.KillWord()
	default:
		if k.Rune != 0 {
			e.InsertRune(k.Rune)
		}
	}
}
