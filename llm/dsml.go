// dsml.go DeepSeek DSML 标签风格工具调用的回退解析：部分 DeepSeek 端点
// 不走 OpenAI tool_calls 协议，而是把调用以如下标签写进正文：
//
//	<｜｜DSML｜｜tool_calls>
//	  <｜｜DSML｜｜invoke name="glob">
//	    <｜｜DSML｜｜parameter name="pattern" string="true">**/*.png</｜｜DSML｜｜parameter>
//	  </｜｜DSML｜｜invoke>
//	</｜｜DSML｜｜tool_calls>
//
// 与 toolcall_fallback.go 同理：不解析的话 agent 循环会把标签当普通文本
// 结束回合。参数值默认按 JSON 解析，string="true"（或解析失败）时按
// 原始字符串处理。
package llm

import (
	"encoding/json"
	"regexp"
	"strings"
)

var (
	// 闭合标签容忍写法：名称可能被截断（tool_c/tool_cal/tool_calls 任意
	// 程度）、">" 可能因流截断缺失；整体缺失时懒惰体匹配到结尾（$ 兜底）。
	// 闭合只认 tool 前缀的同名标签，避免 invoke 体被体内 parameter 的
	// 闭合标签提前截断；DSML 开标签也可能配普通 </tool_call> 闭合（混搭）
	dsmlCloser = `(?:</(?:｜｜DSML｜｜)?tool\w*\s*>?|$)`
	reDSMLWrapper = regexp.MustCompile(`(?s)<｜｜DSML｜｜tool_calls>\s*(.*?)` + dsmlCloser)
	// 单数形式：标签体直接是一个 JSON 对象（name 平铺在顶层，其余键为参数）
	reDSMLSingle = regexp.MustCompile(`(?s)<｜｜DSML｜｜tool_call>\s*(.*?)` + dsmlCloser)
	reDSMLInvoke  = regexp.MustCompile(`(?s)<｜｜DSML｜｜invoke\s+name\s*=\s*"([^"]+)"[^>]*>(.*?)(?:</｜｜DSML｜｜inv\w*\s*>?|$)`)
	reDSMLParam   = regexp.MustCompile(`(?s)<｜｜DSML｜｜parameter\b([^>]*)>(.*?)(?:</｜｜DSML｜｜parameter>|$)`)
	reDSMLAttr    = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_-]*)\s*=\s*"([^"]*)"`)
)

// extractDSMLCalls 从一段文本提取全部 DSML 工具调用，并返回剔除标签后
// 的文本。先剥掉 tool_calls 包装块再扫 invoke，保证包装块里的调用不会
// 因为外层标签被整体删除而丢失；正文里散落的裸 invoke 块也容忍。
func extractDSMLCalls(text string) (calls []ToolCall, cleaned string) {
	// 先把竖线/空格变体标记归一为标准全角形式（reMarkerVariant 见
	// fallback_tags.go），后续正则只需处理一种写法
	text = reMarkerVariant.ReplaceAllString(text, "｜｜DSML｜｜")
	// 单数形式 <｜｜DSML｜｜tool_call>{...}</｜｜DSML｜｜tool_call>：体是
	// JSON 对象，name 平铺在顶层，其余键即参数（兼容嵌套 arguments）。
	for _, blk := range reDSMLSingle.FindAllStringSubmatchIndex(text, -1) {
		if call, ok := dsmlSingleCall(text[blk[2]:blk[3]]); ok {
			calls = append(calls, call)
			text = strings.ReplaceAll(text, text[blk[0]:blk[1]], "")
		}
	}
	// 把包装块整段移除，但先解析其中的 invoke，避免随外层标签一起丢失
	segments := []string{text}
	for _, blk := range reDSMLWrapper.FindAllStringSubmatchIndex(text, -1) {
		segments = append(segments, text[blk[2]:blk[3]])
		text = strings.ReplaceAll(text, text[blk[0]:blk[1]], "")
	}
	segments[0] = text
	for _, seg := range segments {
		for _, blk := range reDSMLInvoke.FindAllStringSubmatchIndex(seg, -1) {
			name := seg[blk[2]:blk[3]]
			body := seg[blk[4]:blk[5]]
			args, ok := dsmlParams(body)
			if !ok {
				continue
			}
			calls = append(calls, ToolCall{Function: FunctionCall{
				Name:      name,
				Arguments: args,
			}})
			text = strings.ReplaceAll(text, seg[blk[0]:blk[1]], "")
		}
	}
	return calls, text
}

// dsmlSingleCall 解析单数 tool_call 标签体。两种写法：
//   - 体是 JSON 对象：顶层 name 之外的键全部作为参数；若有嵌套 arguments
//     对象则以其为准；
//   - 体是 parameter 子标签：name 与 arguments 各用一个 parameter 表达。
func dsmlSingleCall(body string) (ToolCall, bool) {
	var obj map[string]interface{}
	// 闭合标签完全无法识别时（$ 兜底），体里可能带着残缺闭合尾巴，
	// 整体 Unmarshal 会失败——体以 "{" 开头时先取第一个配平 JSON 对象
	if trimmed := strings.TrimLeft(body, " \t\r\n"); strings.HasPrefix(trimmed, "{") {
		if balanced := balancedJSONObject(trimmed); balanced != "" {
			body = balanced
		}
	}
	if err := json.Unmarshal([]byte(body), &obj); err == nil {
		name, _ := obj["name"].(string)
		if name == "" {
			return ToolCall{}, false
		}
		args := map[string]interface{}{}
		if nested, ok := obj["arguments"].(map[string]interface{}); ok {
			args = nested
		} else {
			for k, v := range obj {
				if k != "name" {
					args[k] = v
				}
			}
		}
		data, err := json.Marshal(args)
		if err != nil {
			return ToolCall{}, false
		}
		return ToolCall{Function: FunctionCall{Name: name, Arguments: string(data)}}, true
	}
	// parameter 子标签形式
	params, ok := dsmlParams(body)
	if !ok {
		return ToolCall{}, false
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(params), &m); err != nil {
		return ToolCall{}, false
	}
	name, _ := m["name"].(string)
	if name == "" {
		return ToolCall{}, false
	}
	var args interface{} = map[string]interface{}{}
	switch v := m["arguments"].(type) {
	case map[string]interface{}:
		args = v
	case string:
		// arguments 以字符串承载：内容通常本身是 JSON 对象，是则解开后用
		var nested interface{}
		if err := json.Unmarshal([]byte(v), &nested); err == nil {
			args = nested
		} else {
			args = v
		}
	}
	data, err := json.Marshal(args)
	if err != nil {
		return ToolCall{}, false
	}
	return ToolCall{Function: FunctionCall{Name: name, Arguments: string(data)}}, true
}

// dsmlParams 解析 invoke 体里的全部 parameter 标签，组装成参数 JSON
// 对象。没有任何 parameter 时返回 "{}"。
func dsmlParams(body string) (string, bool) {
	args := map[string]interface{}{}
	for _, blk := range reDSMLParam.FindAllStringSubmatch(body, -1) {
		attrs := dsmlAttrs(blk[1])
		name, ok := attrs["name"]
		if !ok || name == "" {
			return "", false
		}
		raw := blk[2]
		if strings.TrimSpace(raw) == "" && strings.EqualFold(attrs["string"], "true") {
			args[name] = ""
			continue
		}
		if !strings.EqualFold(attrs["string"], "true") {
			var v interface{}
			if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &v); err == nil {
				args[name] = v
				continue
			}
		}
		args[name] = raw
	}
	data, err := json.Marshal(args)
	if err != nil {
		return "", false
	}
	return string(data), true
}

// dsmlAttrs 解析标签属性键值对。
func dsmlAttrs(s string) map[string]string {
	out := map[string]string{}
	for _, m := range reDSMLAttr.FindAllStringSubmatch(s, -1) {
		out[m[1]] = m[2]
	}
	return out
}
