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
	"sync/atomic"
)

var (
	// 属性形式（自闭合）：<tool_call name="bash" arguments={...} />。
	// 懒惰匹配到 />，保证 arguments 里的 JSON 可以含 ">"（shell 重定向）。
	reAttrToolCall = regexp.MustCompile(`(?s)<tool_call\b(.*?)/\s*>`)
	// 属性形式（无闭合的非自闭合标签，兜底）
	reAttrToolCallOpen = regexp.MustCompile(`(?s)<tool_call\b([^>]*)>`)
	// 正文形式：<tool_call>{"name":"read","arguments":{...}}</tool_call>
	reBodyToolCall = regexp.MustCompile(`(?s)<tool_call\s*>(.*?)</tool_call>`)
	// 宽松体形式的开标签（无属性、精确 ">" 结尾），供闭合残缺时的兜底扫描
	reOpenToolCall = regexp.MustCompile(`<tool_call\s*>`)
	// 闭合标签变体穷举：</tool_call>、</tool_call（缺 >）、任意程度截断的
	// 名称（</tool_c>）、混用 DSML 风格的 </｜｜DSML｜｜tool_c...> 及其截断
	//（变体 pattern 见 fallback_tags.go，与此处保持同一套写法）
	reLooseCloser = regexp.MustCompile(`\s*</(?:(?:` + markerVariantPattern + `)?tool\w*|` + markerVariantPattern + `\w+)\s*>?`)
	reAttrName    = regexp.MustCompile(`name\s*[:=]\s*["']?([A-Za-z0-9_.-]+)`)
	reAttrArgs    = regexp.MustCompile(`arguments\s*[:=]\s*`)
)

// fallbackSeq 回退调用的 ID 计数：进程级唯一。按消息内编号（call_fallback_0…）
// 会在跨消息时重号，下游按 ID 关联事件的场景会把两次调用混为一次。
var fallbackSeq atomic.Int64

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
		// DSML 标记检测放宽到 "DSML" 关键字：竖线可能是全半角/空格变体，
		// 归一化在 extractDSMLCalls 内部进行
		if strings.Contains(*src, "DSML") {
			extracted, cleaned := extractDSMLCalls(*src)
			calls = append(calls, extracted...)
			*src = strings.TrimSpace(cleaned)
		}
		if strings.Contains(*src, toolCallTag) {
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
		calls[i].ID = fmt.Sprintf("call_fallback_%d", fallbackSeq.Add(1))
		calls[i].Type = "function"
	}
	m.ToolCalls = append(m.ToolCalls, calls...)
	return true
}

// extractToolCalls 从一段文本提取全部 XML 风格工具调用，并返回剔除
// 标签后的文本。
func extractToolCalls(text string) (calls []ToolCall, cleaned string) {
	remove := map[string]bool{} // 待剔除的片段

	// 宽松体形式优先：开标签后紧跟配平 JSON 体即可解析，闭合标签残缺
	// （</tool_cal>、混用 DSML 风格或流截断整体缺失）也能处理；
	// 结构完整的写法会被这里提前消费，后续 pass 靠 remove 判重跳过
	for _, loc := range reOpenToolCall.FindAllStringIndex(text, -1) {
		rest := text[loc[1]:]
		i := strings.Index(rest, "{")
		if i < 0 {
			continue
		}
		obj := balancedJSONObject(rest[i:])
		if obj == "" {
			continue
		}
		c, ok := bodyJSONCall(obj)
		if !ok {
			continue
		}
		end := loc[1] + i + len(obj)
		whole := text[loc[0]:end]
		if m := reLooseCloser.FindStringIndex(text[end:]); m != nil {
			whole = text[loc[0] : end+m[1]]
		}
		calls = append(calls, c)
		remove[whole] = true
	}
	// 正文形式（结构化最完整）
	for _, blk := range reBodyToolCall.FindAllStringSubmatchIndex(text, -1) {
		whole := text[blk[0]:blk[1]]
		if remove[whole] {
			continue
		}
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

// bodyJSONCall 解析 <tool_call>{...}</tool_call> 的 JSON 体。两种写法：
// 标准的 {"name":..,"arguments":{...}}，以及 name 平铺在顶层、其余键
// 直接作为参数的写法（与 DSML 单数形式一致）。
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
		var flat map[string]json.RawMessage
		if err := json.Unmarshal([]byte(obj), &flat); err != nil {
			return ToolCall{}, false
		}
		delete(flat, "name")
		data, err := json.Marshal(flat)
		if err != nil {
			return ToolCall{}, false
		}
		args = string(data)
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
