// interp.go 解释器实例与程序入口：宿主钩子装配、Run 执行程序、函数
// 调用分派（原生/用户/async→Future）。求值本体在 eval.go。
package js

import (
	"context"
	"sync"
)

// HostFn 宿主钩子。
type HostFn func(ip *Interp, args []Value) (Value, error)

// 资源护栏：脚本来自模型输出（半受信，可被提示注入引导），无护栏的
// 失控递归/死循环会杀死整个进程（Go 栈溢出是不可恢复的 fatal error）
// 或永久占住回合 goroutine（纯自旋的解释器循环连 ctx 取消都感知不到）。
const (
	// maxNestDepth 表达式求值与用户函数帧的嵌套深度上限（两者都消耗
	// Go 栈；超限按运行时错误终止，而不是让 runtime 栈溢出崩溃）。
	maxNestDepth = 10_000
	// maxSteps 单次 Run 的步数预算（语句/循环迭代/调用各计一步）。
	// 正常 workflow 脚本在数千步内；预算只拦失控自旋，不约束时长——
	// agent() 等宿主钩子的耗时发生在解释器之外，不占步数。
	maxSteps = 20_000_000
	// maxStringBytes 单个字符串操作的结果上限（repeat/padEnd/拼接），
	// 防内存炸弹（一次性多 GB 分配会直接 OOM，recover 救不了）。
	maxStringBytes = 16 << 20
)

// Interp 解释器实例。
type Interp struct {
	Global *Env
	Host   map[string]HostFn // agent / pipeline / parallel / phase / log
	LogOut func(string)      // console.log 输出

	mu    sync.Mutex
	depth int             // 当前嵌套深度（表达式求值 + 用户函数帧，共享计数）
	steps int64           // 已执行步数
	ctx   context.Context // 取消观察（RunWithCtx 设置；nil = 不观察）
}

// New 创建解释器并安装内建全局。
func New(host map[string]HostFn) *Interp {
	ip := &Interp{Global: NewEnv(nil), Host: host}
	ip.installGlobals()
	return ip
}

// enter 进入一层嵌套（表达式/用户函数帧）：超深即错误，防 Go 栈溢出。
func (ip *Interp) enter() error {
	ip.mu.Lock()
	ip.depth++
	d := ip.depth
	ip.mu.Unlock()
	if d > maxNestDepth {
		return rtErr("nesting depth exceeds %d (runaway recursion or expression chain)", maxNestDepth)
	}
	return nil
}

// leave 退出一层嵌套（与 enter 配对）。
func (ip *Interp) leave() {
	ip.mu.Lock()
	ip.depth--
	ip.mu.Unlock()
}

// tick 每条语句/循环迭代/调用记一步：步数预算拦自旋，每 1024 步顺带
// 观察一次 ctx 取消（死循环因此可被回合取消打断）。
func (ip *Interp) tick() error {
	ip.mu.Lock()
	ip.steps++
	s := ip.steps
	ctx := ip.ctx
	ip.mu.Unlock()
	if s > maxSteps {
		return rtErr("script step budget exceeded (%d steps); likely an infinite loop", maxSteps)
	}
	if s&1023 == 0 && ctx != nil {
		if err := ctx.Err(); err != nil {
			return rtErr("script cancelled: %v", err)
		}
	}
	return nil
}

// RunWithCtx 执行程序并观察 ctx 取消（重置步数计数）。
func (ip *Interp) RunWithCtx(ctx context.Context, prog *Program) (Value, error) {
	ip.mu.Lock()
	ip.ctx = ctx
	ip.steps = 0
	ip.mu.Unlock()
	return ip.Run(prog)
}

// Run 执行程序：顶层 return（workflow 脚本约定）返回其值，否则返回最后
// 一条语句的值。
func (ip *Interp) Run(prog *Program) (Value, error) {
	var last Value
	for _, st := range prog.Stmts {
		v, err := ip.evalStmt(st, ip.Global)
		if err != nil {
			if rs, ok := err.(*returnSig); ok {
				if rs.v == nil {
					return nil, nil
				}
				return AwaitValue(rs.v)
			}
			return nil, err
		}
		last = v
	}
	return last, nil
}

// CallValue 调用一个函数值（宿主回调 thunk 用），自动等待 Future。
func (ip *Interp) CallValue(fn Value, args ...Value) (Value, error) {
	f, ok := fn.(*Fn)
	if !ok {
		return nil, rtErr("value is not a function")
	}
	v, err := ip.callFn(f, args)
	if err != nil {
		return nil, err
	}
	return AwaitValue(v)
}

func (ip *Interp) callFn(f *Fn, args []Value) (Value, error) {
	if f.Native != nil {
		return f.Native(ip, args)
	}
	if f.Async {
		return newFuture(func() (Value, error) { return ip.execUserFn(f, args) }), nil
	}
	return ip.execUserFn(f, args)
}

func (ip *Interp) execUserFn(f *Fn, args []Value) (Value, error) {
	if err := ip.enter(); err != nil {
		return nil, err
	}
	defer ip.leave()
	env := NewEnv(f.Env)
	for i, p := range f.Params {
		var v Value
		if i < len(args) {
			v = args[i]
		}
		env.declare(p, v)
	}
	if f.Expr != nil {
		v, err := ip.evalExpr(f.Expr, env)
		if err != nil {
			return nil, err
		}
		return AwaitValue(v)
	}
	for _, st := range f.Body {
		if _, err := ip.evalStmt(st, env); err != nil {
			if rs, ok := err.(*returnSig); ok {
				if rs.v == nil {
					return nil, nil
				}
				av, aerr := AwaitValue(rs.v)
				if aerr != nil {
					return nil, aerr
				}
				return av, nil
			}
			return nil, err
		}
	}
	return nil, nil
}
