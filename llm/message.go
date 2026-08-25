// Package llm 提供对 OpenAI 兼容接口 (/chat/completions) 的最小客户端，
// 仅用标准库：支持 tool use（function calling）、SSE 流式（含
// reasoning_content 思考流）与多模态内容（文本 + base64 图像）。
package llm

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Tool 描述一个暴露给模型的工具（JSON Schema 参数定义）。
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// FunctionCall 工具调用的函数名与参数。
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // 原始 JSON 字符串
}

// ToolCall 模型返回的一次工具调用请求。
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

func (c ToolCall) Name() string      { return c.Function.Name }
func (c ToolCall) Arguments() string { return c.Function.Arguments }

// Part 多模态内容片段：文本或图像（data URL）。
type Part struct {
	Type     string    `json:"type"` // "text" | "image_url"
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

// ImagePart 构造图像内容片段（url 应为 data:image/...;base64,... 形式）。
func ImagePart(mime, b64 string) Part {
	return Part{Type: "image_url", ImageURL: &imageURL{URL: "data:" + mime + ";base64," + b64}}
}

// TextPart 构造文本内容片段。
func TextPart(text string) Part { return Part{Type: "text", Text: text} }

// Message 对话中的一条消息。Role: system / user / assistant / tool。
// ContentParts 非空时按多模态数组序列化（用于回传图像），否则按纯文本。
// Reasoning 仅本地保存（思考内容），不会回传给服务端。
type Message struct {
	Role         string     `json:"role"`
	Content      string     `json:"content,omitempty"`
	ContentParts []Part     `json:"-"`
	Reasoning    string     `json:"-"`
	ToolCalls    []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID   string     `json:"tool_call_id,omitempty"`
	Name         string     `json:"name,omitempty"`
	// FinishReason 仅来自本次响应（stop/length/tool_calls...），本地使用
	// 不回传。length 表示输出被 token 上限截断。
	FinishReason string `json:"-"`
}

// MarshalJSON 在 ContentParts 非空时把 content 序列化为多模态数组。
func (m Message) MarshalJSON() ([]byte, error) {
	type alias Message
	if len(m.ContentParts) == 0 {
		return json.Marshal(alias(m))
	}
	a := alias(m)
	// 借助中间 map 保证字段顺序无关，content 用数组覆盖
	data, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, err
	}
	obj["content"] = m.ContentParts
	return json.Marshal(obj)
}

func (m Message) String() string {
	if m.Content != "" {
		return m.Content
	}
	return fmt.Sprintf("<%s parts=%d>", m.Role, len(m.ContentParts))
}

// truncate 长文本截断（错误信息展示用）。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// CompactArgs 压缩工具参数文本用于事件展示。
func CompactArgs(args string, n int) string {
	s := strings.TrimSpace(args)
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}
