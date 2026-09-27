// width.go 显示宽度与软换行：ANSI 感知（跳过转义序列）、CJK 宽字符按
// 2 列计、组合符按 0 列计。Wrap 在指定宽度处软换行，行尾补重置码、
// 行首重开样式，保证折行后颜色/暗色样式延续。
package term

import (
	"strings"
	"unicode"
)

// RuneWidth 返回单个 rune 的显示列宽（组合符 0、宽字符 2、其余 1）。
func RuneWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case r < 32 || (r >= 0x7f && r < 0xa0):
		return 0 // 控制字符
	case unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || unicode.Is(unicode.Cf, r):
		return 0 // 组合附加符 / 格式字符（ZWJ 等）
	case isWide(r):
		return 2
	}
	return 1
}

// isWide 覆盖 East Asian Wide / Fullwidth 常用区间。
func isWide(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F, // Hangul Jamo
		r >= 0x2E80 && r <= 0x303E, // CJK 部首、康熙部首、CJK 标点
		r >= 0x3041 && r <= 0x33FF, // 假名、注音、CJK 兼容
		r >= 0x3400 && r <= 0x4DBF, // CJK 扩展 A
		r >= 0x4E00 && r <= 0x9FFF, // CJK 统一表意
		r >= 0xA000 && r <= 0xA4CF, // 彝文
		r >= 0xAC00 && r <= 0xD7A3, // Hangul 音节
		r >= 0xF900 && r <= 0xFAFF, // CJK 兼容表意
		r >= 0xFE30 && r <= 0xFE4F, // CJK 兼容形式
		r >= 0xFF00 && r <= 0xFF60, // 全角形式
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x17000 && r <= 0x18AFF, // Tangut
		r >= 0x1F300 && r <= 0x1F64F, // Emoji 表情
		r >= 0x1F900 && r <= 0x1F9FF,
		r >= 0x20000 && r <= 0x3FFFD: // CJK 扩展 B+
		return true
	}
	return false
}

// forEachRune ANSI 感知逐 rune 遍历：转义序列中的 rune（含起始 ESC
// 本身）以 esc=true 回调，转义序列终止判定沿用 isFinalByte；fn 返回
// false 时立即停止遍历。StringWidth / StripANSI / Truncate 共用此遍历；
// wrapLine 需要收集完整 SGR 串，保持专用实现。
func forEachRune(s string, fn func(r rune, esc bool) bool) {
	esc := false
	for _, r := range s {
		if esc {
			if !fn(r, true) {
				return
			}
			if isFinalByte(r) {
				esc = false
			}
			continue
		}
		if r == 0x1b {
			if !fn(r, true) {
				return
			}
			esc = true
			continue
		}
		if !fn(r, false) {
			return
		}
	}
}

// StringWidth 计算 ANSI 转义序列之外的显示宽度。
func StringWidth(s string) int {
	w := 0
	forEachRune(s, func(r rune, esc bool) bool {
		if !esc {
			w += RuneWidth(r)
		}
		return true
	})
	return w
}

func isFinalByte(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '~' || r == '\\'
}

// StripANSI 去除所有 ANSI 转义序列。
func StripANSI(s string) string {
	var b strings.Builder
	forEachRune(s, func(r rune, esc bool) bool {
		if !esc {
			b.WriteRune(r)
		}
		return true
	})
	return b.String()
}

// Wrap 把文本按 width 列软换行成多行，保留其中的 ANSI 样式：折行处补
// 重置码，新行行首重开折行前的有效样式。样式跟踪按整段 SGR 序列处理
// （本项目内容仅使用 dim/red/cyan/bold 等整行样式，足够可靠）。
func Wrap(s string, width int) []string {
	if width < 1 {
		width = 1
	}
	var out []string
	for _, line := range strings.Split(s, "\n") {
		out = append(out, wrapLine(line, width)...)
	}
	return out
}

func wrapLine(line string, width int) []string {
	// 跟踪行内样式状态：以 (前缀样式串) 记录最近一次未重置的 SGR。
	var cur []string // 活动样式序列（每个元素为一个 SGR 转义串）
	activeStyle := func() string {
		var b strings.Builder
		for _, s := range cur {
			b.WriteString(s)
		}
		return b.String()
	}
	var rows []string
	var row []rune
	rowW := 0
	esc := false
	var seq strings.Builder

	flush := func() {
		rows = append(rows, string(row))
		row = row[:0]
		rowW = 0
	}

	for _, r := range line {
		if esc {
			seq.WriteRune(r)
			if isFinalByte(r) {
				esc = false
				s := seq.String()
				row = append(row, []rune(s)...)
				if strings.HasSuffix(s, "m") {
					if s == "\x1b[0m" || s == "\x1b[m" {
						cur = cur[:0]
					} else {
						cur = append(cur, s)
					}
				}
			}
			continue
		}
		if r == 0x1b {
			esc = true
			seq.Reset()
			seq.WriteRune(r)
			continue
		}
		w := RuneWidth(r)
		if rowW+w > width {
			// 折行：行尾重置样式，行首重开
			if len(cur) > 0 {
				row = append(row, []rune("\x1b[0m")...)
			}
			flush()
			if st := activeStyle(); st != "" {
				row = append(row, []rune(st)...)
			}
		}
		row = append(row, r)
		rowW += w
	}
	if len(row) > 0 || len(rows) == 0 {
		if len(cur) > 0 && len(rows) > 0 {
			row = append(row, []rune("\x1b[0m")...)
		}
		rows = append(rows, string(row))
	}
	return rows
}

// Truncate 按显示宽度截断（尾部不加省略号），保留行首已有 ANSI。
func Truncate(s string, width int) string {
	if StringWidth(s) <= width {
		return s
	}
	var b strings.Builder
	w := 0
	forEachRune(s, func(r rune, esc bool) bool {
		if !esc {
			rw := RuneWidth(r)
			if w+rw > width {
				return false
			}
			w += rw
		}
		b.WriteRune(r)
		return true
	})
	return b.String()
}

// Pad 把文本用空格补齐到 width 显示列（超宽时原样返回）。
func Pad(s string, width int) string {
	d := width - StringWidth(s)
	if d <= 0 {
		return s
	}
	return s + strings.Repeat(" ", d)
}
