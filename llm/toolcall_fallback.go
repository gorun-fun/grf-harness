// toolcall_fallback.go 正文内 XML 风格 <tool_call> 的回退解析：部分模型
// （如 Qwen 系）在不走 OpenAI tool_calls 协议时会把调用写进正文。
// 不解析的话 agent 循环会当作普通回复结束，会话表现为"莫名停止"。
// 参数非法时不丢弃调用——照常交给工具层，让解码错误作为工具结果
// 反馈给模型，模型可自行纠正后重试。
package llm

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var (
	// 属性形式（自闭合）：<tool_call name="bash" arguments={...} />。
	// 懒惰匹配到 />，保证 arguments 里的 JSON 可以含 ">"（shell 重定向）。
	reAttrToolCall = regexp.MustCompile(`(?s)<tool_call\b(.*?)/\s*>`)
	// 属性形式（无闭合的非自闭合标签，兜底）
	reAttrToolCallOpen = regexp.MustCompile(`(?s)<tool_call\b([^>]*)>`)
	// 正文形式：<tool_call>{"name":"read","arguments":{...}}</tool_call>
	reBodyToolCall = regexp.MustCompile(`(?s)<tool_call\s*>(.*?)</tool_call>`)
	reAttrName     = regexp.MustCompile(`name\s*[:=]\s*["']?([A-Za-z0-9_.-]+)`)
	reAttrArgs     = regexp.MustCompile(`arguments\s*[:=]\s*`)
)

// fallbackContentToolCalls 从正文提取 XML 风格工具调用并从正文中移除
// 相应片段。部分服务端（Qwen 系部署）会把同一条消息里的一部分调用
// 解析成协议层 tool_calls 而把其余留在正文，因此即使已有 ToolCalls 也
// 尝试解析，只跳过与协议层重复（同名同参数）的调用。
func fallbackContentToolCalls(m *Message) bool {
	var calls []ToolCall
	// 逐个来源（正文、思考）提取两类正文内调用：DSML 标签风格（DeepSeek）
	// 与 <tool_call> XML 风格（Qwen 系）。正文优先；正文没有再扫思考
	// 内容——部分模型把工具调用写进 reasoning 流，不在正文里解析会话
	// 就会静默结束。
	for _, src := range []*string{&m.Content, &m.Reasoning} {
		if strings.Contains(*src, dsmlMarker) {
			extracted, cleaned := extractDSMLCalls(*src)
			calls = append(calls, extracted...)
			*src = strings.TrimSpace(cleaned)
		}
		if strings.Contains(*src, "<tool_call") {
			extracted, cleaned := extractToolCalls(*src)
			calls = append(calls, extracted...)
			*src = strings.TrimSpace(cleaned)
		}
	}

	if len(calls) == 0 {
		return false
	}
	// 正文与思考、思考内部都可能出现同一次调用，同名同参数去重
	seen := map[string]bool{}
	uniq := calls[:0]
	for _, c := range calls {
		key := c.Name() + "\x00" + strings.TrimSpace(c.Arguments())
		if !seen[key] {
			seen[key] = true
			uniq = append(uniq, c)
		}
	}
	calls = uniq
	if len(calls) == 0 {
		return false
	}
	// 与协议层调用去重（同名同参数视为同一次调用）
	if len(m.ToolCalls) > 0 {
		seen := make(map[string]bool, len(m.ToolCalls))
		for _, c := range m.ToolCalls {
			seen[c.Name()+"\x00"+strings.TrimSpace(c.Arguments())] = true
		}
		uniq := calls[:0]
		for _, c := range calls {
			if !seen[c.Name()+"\x00"+strings.TrimSpace(c.Arguments())] {
				uniq = append(uniq, c)
			}
		}
		calls = uniq
		if len(calls) == 0 {
			return false
		}
	}
	for i := range calls {
		calls[i].ID = fmt.Sprintf("call_fallback_%d", i)
		calls[i].Type = "function"
	}
	m.ToolCalls = append(m.ToolCalls, calls...)
	return true
}

// extractToolCalls 从一段文本提取全部 XML 风格工具调用，并返回剔除
// 标签后的文本。
func extractToolCalls(text string) (calls []ToolCall, cleaned string) {
	remove := map[string]bool{} // 待剔除的片段

	// 正文形式优先（结构化最完整）
	for _, blk := range reBodyToolCall.FindAllStringSubmatchIndex(text, -1) {
		whole := text[blk[0]:blk[1]]
		body := strings.TrimSpace(text[blk[2]:blk[3]])
		if c, ok := bodyJSONCall(body); ok {
			calls = append(calls, c)
			remove[whole] = true
		}
	}
	// 属性形式：先试自闭合，再兜底非自闭合开标签
	attrPass := func(re *regexp.Regexp) {
		for _, blk := range re.FindAllStringSubmatchIndex(text, -1) {
			whole := text[blk[0]:blk[1]]
			if remove[whole] || strings.HasSuffix(strings.TrimSpace(whole), "</tool_call>") {
				continue
			}
			c, ok := attrToolCall(text[blk[2]:blk[3]])
			if !ok {
				continue
			}
			calls = append(calls, c)
			remove[whole] = true
		}
	}
	attrPass(reAttrToolCall)
	attrPass(reAttrToolCallOpen)
	for frag := range remove {
		text = strings.ReplaceAll(text, frag, "")
	}
	return calls, text
}

// balancedJSONObject 从文本中提取第一个花括号配平的 JSON 对象
// （跳过字符串字面量内的花括号）。用于容忍模型在参数 JSON 后面
// 追加的垃圾尾巴，如 arguments={...},"name":"bash"}。
func balancedJSONObject(s string) string {
	start := strings.Index(s, "{")
	if start < 0 {
		return ""
	}
	depth := 0
	inStr, esc := false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// bodyJSONCall 解析 <tool_call>{...}</tool_call> 的 JSON 体。
func bodyJSONCall(body string) (ToolCall, bool) {
	obj := balancedJSONObject(body)
	if obj == "" {
		return ToolCall{}, false
	}
	var parsed struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(obj), &parsed); err != nil || parsed.Name == "" {
		return ToolCall{}, false
	}
	args := strings.TrimSpace(string(parsed.Arguments))
	if args == "" {
		args = "{}"
	}
	return ToolCall{Function: FunctionCall{Name: parsed.Name, Arguments: args}}, true
}

// attrToolCall 解析属性形式；arguments 值可能是裸 JSON（带引号会破坏
// XML 属性语法），因此不做 XML 反序列化，直接取 name 之后的 {...} 段。
func attrToolCall(attrs string) (ToolCall, bool) {
	nm := reAttrName.FindStringSubmatch(attrs)
	if nm == nil {
		return ToolCall{}, false
	}
	args := ""
	if loc := reAttrArgs.FindStringIndex(attrs); loc != nil {
		rest := strings.TrimSpace(attrs[loc[1]:])
		rest = strings.TrimPrefix(rest, "=")
		rest = strings.Trim(rest, `"' `)
		if obj := balancedJSONObject(rest); obj != "" {
			args = obj
		}
	}
	if args == "" {
		args = "{}"
	}
	return ToolCall{Function: FunctionCall{Name: nm[1], Arguments: args}}, true
}
