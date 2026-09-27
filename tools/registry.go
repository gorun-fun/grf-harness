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

	"git.gorun.fun/share/grf-harness/llm"
)

// Image 工具结果携带的图像（data URL 的 mime 与 base64）。
type Image struct {
	Mime string
	B64  string
}

// Result 工具执行结果：文本为主，read_image 等可附带图像。Err 非空表示
// 执行失败（文本仍完整回给模型），供结构化工具事件区分 done/error。
type Result struct {
	Text   string
	Images []Image
	Err    string
}

// TextResult 纯文本结果快捷构造。
func TextResult(s string) Result { return Result{Text: s} }

// Handler 工具执行函数。返回的 error 会被格式化为工具结果反馈给模型。
type Handler func(ctx context.Context, args string) (Result, error)

// Registry 工具注册表（并发安全）。
type Registry struct {
	mu       sync.RWMutex
	handlers map[string]Handler
	custom   map[string]llm.Tool // spec 之外的自定义工具定义（嵌入方扩展）
	client   map[string]struct{} // 客户端执行工具（ORS function 工具，本地不执行）
	disabled map[string]bool     // 禁用标记（可逆：EnableAll 恢复）
}

func NewRegistry() *Registry {
	return &Registry{handlers: map[string]Handler{}, custom: map[string]llm.Tool{}, client: map[string]struct{}{}, disabled: map[string]bool{}}
}

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

// RegisterCustom 注册 spec 之外的自定义工具（嵌入方扩展入口）：定义由
// 调用方完整给出（随请求发给模型），名字不得与内置 28 个冲突——内置
// 工具定义的唯一来源仍是 spec，不允许借道覆盖。注册后的自定义工具与
// 内置工具在 Disable/Execute/Defs/Registered 里同等对待。
func (r *Registry) RegisterCustom(def llm.Tool, h Handler) {
	if def.Name == "" {
		panic("tools: custom tool definition needs a name")
	}
	if _, ok := SpecDef(def.Name); ok {
		panic("tools: custom tool name conflicts with spec: " + def.Name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.custom[def.Name] = def
	r.handlers[def.Name] = h
}

// RegisterClient 注册"客户端执行"工具（对应 OpenAI Responses 的 function
// 工具）：定义随请求发给模型，但调用不在本地执行——agent 循环遇到它时
// 暂停回合（历史尾部保留未闭合 tool_call），由接入方执行后经
// Agent.Resume 回传结果。名字规则同 RegisterCustom（不得与内置冲突）。
func (r *Registry) RegisterClient(def llm.Tool) {
	if def.Name == "" {
		panic("tools: client tool definition needs a name")
	}
	if _, ok := SpecDef(def.Name); ok {
		panic("tools: client tool name conflicts with spec: " + def.Name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.handlers[def.Name]; ok {
		panic("tools: tool already registered: " + def.Name)
	}
	r.custom[def.Name] = def
	r.handlers[def.Name] = clientPendingHandler
	r.client[def.Name] = struct{}{}
}

// IsClient 是否客户端执行工具（已注册且未禁用）。agent 循环在 Execute
// 前据此拦截，转入暂停协议。
func (r *Registry) IsClient(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.off(name) {
		return false
	}
	_, ok := r.client[name]
	return ok
}

// clientPendingHandler 兜底：正常路径下 agent 循环在 Execute 前已把
// client 工具转入暂停；走到这里说明调用方绕过了循环直接 Execute。
func clientPendingHandler(ctx context.Context, args string) (Result, error) {
	return Result{Text: "[client tool] not executed locally; the caller must run it and feed the result back via Agent.Resume"}, nil
}

// Disable 按名禁用已注册工具（未注册的名静默忽略）。禁用后工具不再随
// Defs 发给模型、Execute 返回 unknown tool，用于宿主按场景裁剪工具链；
// 标记式禁用，EnableAll 可整体恢复。
func (r *Registry) Disable(names ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, name := range names {
		r.disabled[name] = true
	}
}

// EnableAll 清除全部禁用标记（恢复到注册完成时的全量工具集）。
func (r *Registry) EnableAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.disabled = map[string]bool{}
}

func (r *Registry) off(name string) bool { return r.disabled[name] }

// Has 是否已注册（且未被禁用）。
func (r *Registry) Has(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.handlers[name]
	return ok && !r.off(name)
}

// Registered 已注册的工具名（内置按 spec 顺序在前，自定义按字典序在后）。
func (r *Registry) Registered() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var names []string
	for _, n := range SpecNames() {
		if _, ok := r.handlers[n]; ok && !r.off(n) {
			names = append(names, n)
		}
	}
	var custom []string
	for n := range r.custom {
		if _, ok := r.handlers[n]; ok && !r.off(n) {
			custom = append(custom, n)
		}
	}
	sort.Strings(custom)
	return append(names, custom...)
}

// Defs 返回已注册工具的定义（内置按 spec 顺序，自定义按字典序），
// 随请求发送给 API。单次持锁完成收集（不借道 Registered 避免双重加锁）。
func (r *Registry) Defs() []llm.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var names []string
	for _, n := range SpecNames() {
		if _, ok := r.handlers[n]; ok && !r.off(n) {
			names = append(names, n)
		}
	}
	var custom []string
	for n := range r.custom {
		if _, ok := r.handlers[n]; ok && !r.off(n) {
			custom = append(custom, n)
		}
	}
	sort.Strings(custom)
	names = append(names, custom...)
	defs := make([]llm.Tool, 0, len(names))
	for _, n := range names {
		if d, ok := SpecDef(n); ok {
			defs = append(defs, d)
		} else if d, ok := r.custom[n]; ok {
			defs = append(defs, d)
		}
	}
	return defs
}

// Execute 执行工具。未知工具/执行错误/处理器 panic 都转为文本结果，
// 保证任何失败都作为工具结果反馈给模型而不是中断会话；失败时 Err 字段
// 携带错误信息（Text 保持完整反馈文本不变）。
func (r *Registry) Execute(ctx context.Context, name, args string) (res Result) {
	r.mu.RLock()
	h, ok := r.handlers[name]
	off := r.off(name)
	r.mu.RUnlock()
	if !ok || off {
		msg := fmt.Sprintf("error: unknown tool %q, available: %v", name, r.Registered())
		return Result{Text: msg, Err: msg}
	}
	defer func() {
		if rec := recover(); rec != nil {
			msg := fmt.Sprintf("error: tool %q panicked: %v", name, rec)
			res = Result{Text: msg, Err: msg}
		}
	}()
	res, err := h(ctx, args)
	if err != nil {
		return Result{Text: "error: " + err.Error(), Err: err.Error()}
	}
	if res.Err != "" && res.Text == "" {
		res.Text = "error: " + res.Err
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
