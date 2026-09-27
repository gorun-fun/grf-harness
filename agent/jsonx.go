// jsonx.go workflow 子代理最终消息的 JSON 提取：从自然语言包裹的文本
// 里捞出候选 JSON 对象（整体 / ```json 围栏 / 首个平衡块），供
// schema.go 校验（自 workflow.go 拆出）。
package agent

import (
	"encoding/json"
	"strings"
)

// extractJSONForSchema 从子代理最终消息提取并校验 JSON 对象。
func extractJSONForSchema(out string, schema map[string]any) (map[string]any, bool) {
	candidates := []string{}
	trimmed := strings.TrimSpace(out)
	// 1) 整体即 JSON
	if strings.HasPrefix(trimmed, "{") {
		candidates = append(candidates, trimmed)
	}
	// 2) ```json ... ``` 围栏
	for _, m := range fencedJSONBlocks(out) {
		candidates = append(candidates, m)
	}
	// 3) 首个平衡 {...} 块
	if b := balancedJSON(trimmed); b != "" {
		candidates = append(candidates, b)
	}
	for _, c := range candidates {
		var v any
		if json.Unmarshal([]byte(c), &v) != nil {
			continue
		}
		if err := ValidateSchema(v, schema); err == nil {
			obj, ok := v.(map[string]any)
			if !ok {
				continue
			}
			return obj, true
		}
	}
	return nil, false
}

// fencedJSONBlocks 按三个反引号围栏切分文本，返回形如 JSON 对象的
// 内容块（```json 围栏剥掉语言标注）。实现是字符串 split，不是正则。
func fencedJSONBlocks(s string) []string {
	var out []string
	for _, chunk := range strings.Split(s, "```") {
		chunk = strings.TrimSpace(chunk)
		if strings.HasPrefix(chunk, "json") {
			chunk = strings.TrimSpace(strings.TrimPrefix(chunk, "json"))
		}
		if strings.HasPrefix(chunk, "{") && strings.HasSuffix(chunk, "}") {
			out = append(out, chunk)
		}
	}
	return out
}

// balancedJSON 找到字符串中第一个配平的 {...} 段。
func balancedJSON(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth := 0
	inStr := false
	esc := false
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
