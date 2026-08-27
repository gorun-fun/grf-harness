// registry.go 工具注册表：定义来自嵌入 spec，处理器按名注册；执行时
// 未知工具与错误均以文本形式返回给模型（不中断循环）。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"grf/llm"
)

// Image 工具结果携带的图像（data URL 的 mime 与 base64）。
type Image struct {
	Mime string
	B64  string
}

// Result 工具执行结果：文本为主，read_image 等可附带图像。
type Result struct {
	Text   string
	Images []Image
}

// TextResult 纯文本结果快捷构造。
func TextResult(s string) Result { return Result{Text: s} }

// Handler 工具执行函数。返回的 error 会被格式化为工具结果反馈给模型。
type Handler func(ctx context.Context, args string) (Result, error)

// Registry 工具注册表（并发安全）。
type Registry struct {
	mu       sync.RWMutex
	handlers map[string]Handler
}

func NewRegistry() *Registry { return &Registry{handlers: map[string]Handler{}} }

// Register 按工具名注册处理器（定义取自 spec，不存在则 panic——保证
// 实现与文档一一对齐）。
func (r *Registry) Register(name string, h Handler) {
	if _, ok := SpecDef(name); !ok {
		panic("tools: unknown tool in spec: " + name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[name] = h
}

// Disable 按名移除已注册工具（未注册的名静默忽略）。禁用后工具不再随
// Defs 发给模型、Execute 返回 unknown tool，用于宿主按场景裁剪工具链。
func (r *Registry) Disable(names ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, name := range names {
		delete(r.handlers, name)
	}
}

// Has 是否已注册。
func (r *Registry) Has(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.handlers[name]
	return ok
}

// Registered 已注册的工具名（按 spec 顺序）。
func (r *Registry) Registered() []string {
	var names []string
	for _, n := range SpecNames() {
		if r.Has(n) {
			names = append(names, n)
		}
	}
	return names
}

// Defs 返回已注册工具的定义（spec 顺序），随请求发送给 API。
func (r *Registry) Defs() []llm.Tool {
	var defs []llm.Tool
	for _, n := range SpecNames() {
		if r.Has(n) {
			d, _ := SpecDef(n)
			defs = append(defs, d)
		}
	}
	return defs
}

// Execute 执行工具。未知工具/执行错误/处理器 panic 都转为文本结果，
// 保证任何失败都作为工具结果反馈给模型而不是中断会话。
func (r *Registry) Execute(ctx context.Context, name, args string) (res Result) {
	r.mu.RLock()
	h, ok := r.handlers[name]
	r.mu.RUnlock()
	if !ok {
		return TextResult(fmt.Sprintf("error: unknown tool %q, available: %v", name, SpecNames()))
	}
	defer func() {
		if rec := recover(); rec != nil {
			res = TextResult(fmt.Sprintf("error: tool %q panicked: %v", name, rec))
		}
	}()
	res, err := h(ctx, args)
	if err != nil {
		return TextResult("error: " + err.Error())
	}
	if res.Text == "" && len(res.Images) == 0 {
		res.Text = "(no output)"
	}
	return res
}

// DecodeJSON JSON 参数解码到任意结构（空参数视为空对象）。
func DecodeJSON(args string, v any) error { return decodeArgs(args, v) }

// decodeArgs JSON 参数解码到任意结构（空参数视为空对象）。
func decodeArgs(args string, v any) error {
	s := strings.TrimSpace(args)
	if s == "" {
		s = "{}"
	}
	if err := json.Unmarshal([]byte(s), v); err != nil {
		return fmt.Errorf("invalid arguments %q: %w", args, err)
	}
	return nil
}

// sortedJSONKeyNames 仅供调试。
func sortedJSONKeyNames(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
