// workflow.go workflow 工具：以文档规定的 JavaScript 脚本编排子代理。
// 脚本体提供 agent/pipeline/parallel/phase/log 钩子与 args 全局，顶层
// await 可用，return 值 JSON 序列化后作为工具结果。schema 校验限定
// type/properties/required/additionalProperties/items/enum/const/oneOf
// 键集；钩子误用抛错立即终止整个脚本。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"grf/js"
	"grf/tools"
)

const (
	workflowConcurrency = 8  // 同时运行的子代理上限
	workflowAgentCap    = 64 // 单个 workflow 的子代理总数上限
)

// workflowCtx 一次 workflow 执行的共享守卫。
type workflowCtx struct {
	mu      sync.Mutex
	sem     chan struct{}
	used    int
	a       *Agent
	m       *Manager
	failErr error
}

// RegisterWorkflow 注册 workflow 工具。
func RegisterWorkflow(r *tools.Registry, a *Agent, m *Manager) {
	r.Register("workflow", func(ctx context.Context, args string) (tools.Result, error) {
		var p struct {
			Script string         `json:"script"`
			Meta   map[string]any `json:"meta"`
			Args   map[string]any `json:"args"`
		}
		if err := tools.DecodeJSON(args, &p); err != nil {
			return tools.Result{}, err
		}
		if strings.TrimSpace(p.Script) == "" {
			return tools.Result{}, fmt.Errorf("script is required (plain JavaScript body, ends with return <json-value>)")
		}
		if err := validateWorkflowMeta(p.Meta); err != nil {
			return tools.Result{}, err
		}
		name, _ := p.Meta["name"].(string)

		wc := &workflowCtx{
			sem: make(chan struct{}, workflowConcurrency),
			a:   a,
			m:   m,
		}

		host := map[string]js.HostFn{
			"agent":    wc.hookAgent,
			"pipeline": wc.hookPipeline,
			"parallel": wc.hookParallel,
			"phase": func(ip *js.Interp, args []js.Value) (js.Value, error) {
				if len(args) > 0 {
					a.emit("phase", js.ToString(js.Arg(args, 0)))
				}
				return nil, nil
			},
			"log": func(ip *js.Interp, args []js.Value) (js.Value, error) {
				var parts []string
				for _, v := range args {
					parts = append(parts, js.ToString(v))
				}
				a.emit("phase", "["+name+"] "+strings.Join(parts, " "))
				return nil, nil
			},
		}

		ip := js.New(host)
		if p.Args != nil {
			ip.Global.Declare("args", toJSValue(p.Args))
		} else {
			ip.Global.Declare("args", map[string]any{})
		}
		out, err := ip.RunEval(p.Script)
		if err != nil {
			return tools.Result{}, fmt.Errorf("workflow script failed: %v", err)
		}
		if out == nil {
			return tools.TextResult("null"), nil
		}
		return tools.TextResult(js.JSONStringify(out)), nil
	})
}

// validateWorkflowMeta meta 参数校验：name/description 必填。
func validateWorkflowMeta(meta map[string]any) error {
	if meta == nil {
		return fmt.Errorf("meta is required (name + description)")
	}
	name, _ := meta["name"].(string)
	desc, _ := meta["description"].(string)
	if strings.TrimSpace(name) == "" || strings.TrimSpace(desc) == "" {
		return fmt.Errorf("meta requires non-empty name and description strings")
	}
	if strings.Contains(name, " ") {
		return fmt.Errorf("meta.name must be short kebab-case (no spaces)")
	}
	return nil
}

func toJSValue(v any) js.Value {
	switch t := v.(type) {
	case nil:
		return nil
	case bool, string, float64:
		return t
	case int:
		return float64(t)
	case []any:
		out := make([]any, 0, len(t))
		for _, e := range t {
			out = append(out, toJSValue(e))
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = toJSValue(e)
		}
		return out
	}
	return nil
}

func toGoValue(v js.Value) any {
	switch t := v.(type) {
	case nil:
		return nil
	case bool, string:
		return t
	case float64:
		return t
	case []any:
		out := make([]any, 0, len(t))
		for _, e := range t {
			out = append(out, toGoValue(e))
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = toGoValue(e)
		}
		return out
	}
	return nil
}

// acquire 并发/总量守卫；超额即抛错终止脚本。
func (w *workflowCtx) acquire() error {
	w.mu.Lock()
	if w.used >= workflowAgentCap {
		w.mu.Unlock()
		return fmt.Errorf("workflow agent cap reached (%d concurrent agents total); refusing to start more", workflowAgentCap)
	}
	w.used++
	w.mu.Unlock()
	w.sem <- struct{}{}
	return nil
}

func (w *workflowCtx) release() {
	<-w.sem
}

// runAgent 执行一个 workflow 子代理（带并发守卫），返回最终文本或结构
// 化结果；子代理失败返回 nil（配合 .filter(Boolean)）。
func (w *workflowCtx) runAgent(prompt string, opts map[string]any) js.Value {
	if err := w.acquire(); err != nil {
		panicWorkflow(err)
	}
	defer w.release()

	label := "workflow-agent"
	phase := ""
	if opts != nil {
		if l, ok := opts["label"].(string); ok && l != "" {
			label = l
		}
		if ph, ok := opts["phase"].(string); ok && ph != "" {
			phase = ph
		}
		for k := range opts {
			switch k {
			case "label", "phase", "schema", "provider", "model":
			default:
				panicWorkflow(fmt.Errorf("agent() opts.%s is rejected (allowed: label, phase, schema, provider, model)", k))
			}
		}
	}
	if phase != "" {
		w.a.emit("phase", "["+phase+"] "+label)
	}

	promptText := prompt
	var schema map[string]any
	if opts != nil {
		if s, ok := opts["schema"].(map[string]any); ok && s != nil {
			schema = s
			promptText += "\n\n(End your final message with a single JSON object — no prose around it — satisfying the requested schema.)"
		}
	}
	_, out, err := w.m.Subagent(w.a, label, promptText, false)
	if err != nil {
		return nil // 子代理失败 → null
	}
	if schema == nil {
		return out
	}
	obj, ok := extractJSONForSchema(out, schema)
	if !ok {
		return nil // 无法产出合法 JSON → 视为失败
	}
	return toJSValue(obj)
}

// panicWorkflow 以钩子错误终止脚本（由外层 RunEval 捕获）。
func panicWorkflow(err error) {
	panic(&js.RuntimeError{Msg: err.Error()})
}

// extractJSONForSchema 从子代理最终消息提取并校验 JSON 对象。
func extractJSONForSchema(out string, schema map[string]any) (map[string]any, bool) {
	candidates := []string{}
	trimmed := strings.TrimSpace(out)
	// 1) 整体即 JSON
	if strings.HasPrefix(trimmed, "{") {
		candidates = append(candidates, trimmed)
	}
	// 2) ```json ... ``` 围栏
	for _, m := range jsonFenceRe(out) {
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

func jsonFenceRe(s string) []string {
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

// ---- js 宿主钩子 ----

func (w *workflowCtx) hookAgent(ip *js.Interp, args []js.Value) (js.Value, error) {
	prompt, ok := js.Arg(args, 0).(string)
	if !ok || strings.TrimSpace(prompt) == "" {
		return nil, js.RtErr("agent(prompt, opts?) expects a non-empty string prompt")
	}
	var opts map[string]any
	if o, ok := js.Arg(args, 1).(map[string]any); ok && o != nil {
		opts = make(map[string]any)
		for k, v := range o {
			opts[k] = toGoValue(v)
		}
	}
	return js.NewFuture(func() (out js.Value, err error) {
		// panicWorkflow（钩子误用）→ Future 错误，await 处冒泡终止脚本
		defer func() {
			if r := recover(); r != nil {
				if re, ok := r.(*js.RuntimeError); ok {
					w.mu.Lock()
					w.failErr = re
					w.mu.Unlock()
					out, err = nil, re
					return
				}
				panic(r)
			}
		}()
		return w.runAgent(prompt, opts), nil
	}), nil
}

func (w *workflowCtx) hookPipeline(ip *js.Interp, args []js.Value) (js.Value, error) {
	items, ok := js.Arg(args, 0).([]any)
	if !ok {
		return nil, js.RtErr("pipeline(items, ...stages) expects an array as its first argument")
	}
	stages := make([]*js.Fn, 0, len(args)-1)
	for i, a := range args[1:] {
		f, ok := a.(*js.Fn)
		if !ok {
			return nil, js.RtErr("pipeline stage %d is not a function", i)
		}
		stages = append(stages, f)
	}
	return js.NewFuture(func() (js.Value, error) {
		results := make([]any, len(items))
		var wg sync.WaitGroup
		for idx, it := range items {
			idx, it := idx, it
			wg.Add(1)
			go func() {
				defer wg.Done()
				prev := it
				for _, st := range stages {
					v, err := ip.CallValue(st, prev, it, float64(idx))
					if err != nil {
						prev = nil // 普通阶段抛错 → 该 item 置 null
						break
					}
					prev = v
				}
				results[idx] = prev
			}()
		}
		wg.Wait()
		if err := w.takeFail(); err != nil {
			return nil, err
		}
		return results, nil
	}), nil
}

func (w *workflowCtx) hookParallel(ip *js.Interp, args []js.Value) (js.Value, error) {
	thunks, ok := js.Arg(args, 0).([]any)
	if !ok {
		return nil, js.RtErr("parallel(thunks) expects an array of zero-argument functions")
	}
	for i, t := range thunks {
		if _, ok := t.(*js.Fn); !ok {
			return nil, js.RtErr("parallel item %d is not a function", i)
		}
	}
	return js.NewFuture(func() (js.Value, error) {
		results := make([]any, len(thunks))
		var wg sync.WaitGroup
		for idx, th := range thunks {
			idx, th := idx, th
			wg.Add(1)
			go func() {
				defer wg.Done()
				v, err := ip.CallValue(th)
				if err != nil {
					results[idx] = nil
					return
				}
				results[idx] = v
			}()
		}
		wg.Wait()
		if err := w.takeFail(); err != nil {
			return nil, err
		}
		return results, nil
	}), nil
}

// takeFail 取出钩子误用错误（一旦发生终止脚本）。
func (w *workflowCtx) takeFail() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.failErr
}

// ---- schema 校验（仅允许文档规定的 JSON Schema 键集） ----

// ValidateSchema 用受限键集校验值：type/properties/required/
// additionalProperties/items/enum/const/oneOf。
func ValidateSchema(v any, schema map[string]any) error {
	if len(schema) == 0 {
		return nil
	}
	for k := range schema {
		switch k {
		case "type", "properties", "required", "additionalProperties", "items", "enum", "const", "oneOf", "description":
		default:
			return fmt.Errorf("schema key %q is not allowed (only type/properties/required/additionalProperties/items/enum/const/oneOf)", k)
		}
	}
	if c, ok := schema["const"]; ok {
		if !jsonEqual(v, c) {
			return fmt.Errorf("value does not equal const")
		}
	}
	if e, ok := schema["enum"].([]any); ok {
		found := false
		for _, item := range e {
			if jsonEqual(v, item) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("value not in enum")
		}
	}
	if t, ok := schema["type"].(string); ok {
		if err := checkType(v, t); err != nil {
			return err
		}
	}
	if oneOf, ok := schema["oneOf"].([]any); ok {
		matched := 0
		for _, sub := range oneOf {
			if sm, ok := sub.(map[string]any); ok {
				if ValidateSchema(v, sm) == nil {
					matched++
				}
			}
		}
		if matched != 1 {
			return fmt.Errorf("value matches %d oneOf branches (need exactly 1)", matched)
		}
	}
	if props, ok := schema["properties"].(map[string]any); ok {
		obj, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("properties requires an object value")
		}
		for key, sub := range props {
			if sm, ok := sub.(map[string]any); ok {
				if err := ValidateSchema(obj[key], sm); err != nil {
					return fmt.Errorf("property %q: %w", key, err)
				}
			}
		}
	}
	if req, ok := schema["required"].([]any); ok {
		obj, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("required requires an object value")
		}
		for _, key := range req {
			ks, _ := key.(string)
			if _, ok := obj[ks]; !ok {
				return fmt.Errorf("missing required property %q", ks)
			}
		}
	}
	if ap, ok := schema["additionalProperties"]; ok {
		if apBool, isBool := ap.(bool); isBool && !apBool {
			obj, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("additionalProperties requires an object value")
			}
			props := map[string]bool{}
			if p, ok := schema["properties"].(map[string]any); ok {
				for k := range p {
					props[k] = true
				}
			}
			for k := range obj {
				if !props[k] {
					return fmt.Errorf("additional property %q is not allowed", k)
				}
			}
		}
	}
	if items, ok := schema["items"].(map[string]any); ok {
		arr, ok := v.([]any)
		if !ok {
			return fmt.Errorf("items requires an array value")
		}
		for i, e := range arr {
			if err := ValidateSchema(e, items); err != nil {
				return fmt.Errorf("item %d: %w", i, err)
			}
		}
	}
	return nil
}

func checkType(v any, t string) error {
	switch t {
	case "object":
		if _, ok := v.(map[string]any); !ok {
			return fmt.Errorf("expected object, got %T", v)
		}
	case "array":
		if _, ok := v.([]any); !ok {
			return fmt.Errorf("expected array, got %T", v)
		}
	case "string":
		if _, ok := v.(string); !ok {
			return fmt.Errorf("expected string, got %T", v)
		}
	case "number":
		n, ok := v.(float64)
		if !ok {
			return fmt.Errorf("expected number, got %T", v)
		}
		_ = n
	case "integer":
		n, ok := v.(float64)
		if !ok || n != float64(int64(n)) {
			return fmt.Errorf("expected integer, got %T", v)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("expected boolean, got %T", v)
		}
	case "null":
		if v != nil {
			return fmt.Errorf("expected null, got %T", v)
		}
	default:
		return fmt.Errorf("unsupported schema type %q", t)
	}
	return nil
}

func jsonEqual(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}
