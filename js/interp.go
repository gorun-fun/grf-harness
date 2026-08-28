// interp.go 解释器实例与程序入口：宿主钩子装配、Run 执行程序、函数
// 调用分派（原生/用户/async→Future）。求值本体在 eval.go。
package js

// HostFn 宿主钩子。
type HostFn func(ip *Interp, args []Value) (Value, error)

// Interp 解释器实例。
type Interp struct {
	Global *Env
	Host   map[string]HostFn // agent / pipeline / parallel / phase / log
	LogOut func(string)      // console.log 输出
}

// New 创建解释器并安装内建全局。
func New(host map[string]HostFn) *Interp {
	ip := &Interp{Global: NewEnv(nil), Host: host}
	ip.installGlobals()
	return ip
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
