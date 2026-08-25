// Package tools 实现 harness 的全部内置工具。工具对模型可见的定义
// （名称/描述/参数 Schema）严格来自 docs/tools.json 的嵌入副本，一个不少；
// 各工具按其文档语义实现。
package tools

import (
	_ "embed"
	"encoding/json"

	"grf/llm"
)

//go:embed spec/tools.json
var specJSON []byte

var specDefs []llm.Tool

func init() {
	if err := json.Unmarshal(specJSON, &specDefs); err != nil {
		panic("tools: parse embedded tools.json: " + err.Error())
	}
}

// SpecDefs 返回 tools.json 中定义的全部工具定义（按文档原始顺序）。
func SpecDefs() []llm.Tool { return append([]llm.Tool(nil), specDefs...) }

// SpecDef 按名称取单个工具定义。
func SpecDef(name string) (llm.Tool, bool) {
	for _, t := range specDefs {
		if t.Name == name {
			return t, true
		}
	}
	return llm.Tool{}, false
}

// SpecNames 全部工具名（文档顺序）。
func SpecNames() []string {
	names := make([]string, 0, len(specDefs))
	for _, t := range specDefs {
		names = append(names, t.Name)
	}
	return names
}
