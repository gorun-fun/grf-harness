// workflow.go workflow 工具：以文档规定的 JavaScript 脚本编排子代理。
// 脚本体提供 agent/pipeline/parallel/phase/log 钩子与 args 全局，顶层
// await 可用，return 值 JSON 序列化后作为工具结果。agent() 的 opts.schema
// 校验在 schema.go，候选 JSON 提取在 jsonx.go；钩子误用抛错立即终止
// 整个脚本。
package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"git.gorun.fun/share/grf-harness/js"
	"git.gorun.fun/share/grf-harness/tools"
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
			return tools.Result{}, fmt.Errorf("workflow script failed: %w", err)
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
		results := w.runConcurrently(len(items), func(idx int) any {
			it := items[idx]
			prev := it
			for _, st := range stages {
				v, err := ip.CallValue(st, prev, it, float64(idx))
				if err != nil {
					return nil // 普通阶段抛错 → 该 item 置 null
				}
				prev = v
			}
			return prev
		})
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
		results := w.runConcurrently(len(thunks), func(idx int) any {
			v, err := ip.CallValue(thunks[idx])
			if err != nil {
				return nil // thunk 抛错 → 该槽位置 null
			}
			return v
		})
		if err := w.takeFail(); err != nil {
			return nil, err
		}
		return results, nil
	}), nil
}

// runConcurrently hookPipeline/hookParallel 的公共并发骨架：为 n 个工作
// 项各起一个 goroutine，fn(i) 的返回值按序落入结果数组——失败项由 fn
// 自己归为 nil（"阶段抛错置 null"语义保持不变），全部落定后返回。
// 钩子误用（panicWorkflow）不在此层：agent() 的 Future 已把它记入
// w.failErr，调用方在 Wait 后经 takeFail 终止整个脚本。
func (w *workflowCtx) runConcurrently(n int, fn func(i int) any) []any {
	results := make([]any, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = fn(i)
		}()
	}
	wg.Wait()
	return results
}

// takeFail 取出钩子误用错误（一旦发生终止脚本）。
func (w *workflowCtx) takeFail() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.failErr
}
