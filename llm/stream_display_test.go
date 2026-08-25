// stream_display_test.go 流式显示过滤测试。
package llm

import (
	"strings"
	"testing"
)

func TestDisplayFilterSuppressesDSMLStream(t *testing.T) {
	var f displayFilter
	var got strings.Builder
	var full strings.Builder
	deltas := []string{
		"我来转换时间戳。\n<｜｜DSML｜｜tool_call>\n{\"name\": \"bash\"",
		", \"command\": \"date -r 1\"}\n</｜｜DSML｜｜tool_call>",
		"\n接下来继续。",
	}
	for _, d := range deltas {
		full.WriteString(d)
		if out := f.delta(full.String()); out != "" {
			got.WriteString(out)
		}
	}
	want := "我来转换时间戳。\n\n接下来继续。"
	if got.String() != want {
		t.Errorf("shown = %q, want %q", got.String(), want)
	}
}

func TestDisplayFilterSuppressesToolCallTag(t *testing.T) {
	var f displayFilter
	var got strings.Builder
	var full strings.Builder
	deltas := []string{
		"先读文件 <tool_c", "all>{\"name\": \"read\"", ", \"arguments\": {\"path\": \"a.go\"}}</tool_call>",
		" 完成",
	}
	for _, d := range deltas {
		full.WriteString(d)
		if out := f.delta(full.String()); out != "" {
			got.WriteString(out)
		}
	}
	if got.String() != "先读文件  完成" {
		t.Errorf("shown = %q", got.String())
	}
}
