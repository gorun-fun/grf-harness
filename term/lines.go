// lines.go 行选取助手：多行文本里取首个/最后一个非空行（供各上层
// 统一"首行摘要"类逻辑，避免各自实现漂移）。
package term

import "strings"

// FirstLine 返回 s 的首个非空行（trim 后）；max>0 时超过 max rune 截断
// 并加省略号；无非空行返回空串。
func FirstLine(s string, max int) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			if max > 0 && len([]rune(t)) > max {
				return string([]rune(t)[:max]) + "…"
			}
			return t
		}
	}
	return ""
}

// LastLine 返回 s 的最后一个非空行（trim 后）；无则空串。
func LastLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}
