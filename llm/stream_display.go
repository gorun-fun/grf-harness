// stream_display.go 流式输出的显示过滤：DSML/<tool_call> 标签只有在
// 完整消息上才能被 fallbackContentToolCalls 剔除，流式过程中原始 delta
// 会先经 hooks 送到 UI，标签文本就会闪现在终端上。这里在增量层面扣住
// 尚未闭合的标签片段、跳过已闭合标签对应区间，只放行干净的正文。
package llm

import "strings"

// displayFilter 记录已放行的正文长度，按"全量清洗后再取增量"的方式工作。
type displayFilter struct {
	emitted int
}

// delta 输入到目前为止累积的全文，返回本次应当放行给 UI 的新增正文。
func (f *displayFilter) delta(full string) string {
	cleaned := full
	if strings.Contains(cleaned, dsmlMarker) {
		_, cleaned = extractDSMLCalls(cleaned)
	}
	if strings.Contains(cleaned, "<tool_call") {
		_, cleaned = extractToolCalls(cleaned)
	}
	cleaned = holdUnclosedTag(cleaned)
	// 理论上不会回缩（扣住策略保证了已放行前缀不会被剔除），兜底防御
	if len(cleaned) < f.emitted {
		f.emitted = len(cleaned)
		return ""
	}
	if len(cleaned) == f.emitted {
		return ""
	}
	out := cleaned[f.emitted:]
	f.emitted = len(cleaned)
	return out
}

// holdUnclosedTag 扣住尾部尚未闭合的标签开头（包括被 delta 切成一半的
// 标记前缀，如 "<tool_c"），等闭合后由 extract 整体剔除。
func holdUnclosedTag(s string) string {
	cut := len(s)
	for _, m := range []string{dsmlMarker, "<tool_call"} {
		if i := strings.LastIndex(s, m); i >= 0 && i < cut {
			cut = i
		}
	}
	if cut < len(s) {
		return s[:cut]
	}
	// 无完整标记：检查尾部是否是某个标记的真前缀（从 "<" 开始）
	for n := len(s); n > 0; n-- {
		tail := s[len(s)-n:]
		if strings.HasPrefix(dsmlMarker, tail) || strings.HasPrefix("<tool_call", tail) {
			return s[:len(s)-n]
		}
	}
	return s
}
