// eval.go 树遍历求值器。值模型：nil（null/undefined）、float64、string、
// bool、[]any、map[string]any、*Fn、*Future。异步：宿主异步钩子与 async
// 函数立即返回 *Future（goroutine 执行），await 阻塞当前求值 goroutine。
package js

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Value JS 值。
type Value any

// NewFuture 导出的 Future 构造（宿主钩子用）。
func NewFuture(run func() (Value, error)) *Future { return newFuture(run) }

// RtErr 导出的解释器错误构造（宿主钩子用）。
func RtErr(format string, a ...any) error { return rtErr(format, a...) }

// Arg 导出取参数（越界 nil）。
func Arg(args []Value, i int) Value {
	if i < len(args) {
		return args[i]
	}
	return nil
}

// Fn 函数值：用户函数（AST+闭包）或原生函数。
type Fn struct {
	Params []string
	Body   []Node // 块体（与 Expr 互斥）
	Expr   Node
	Env    *Env
	Async  bool
	Name   string

	Native func(ip *Interp, args []Value) (Value, error)
}

// Future 异步结果。
type Future struct {
	ch chan futResult
}

type futResult struct {
	v   Value
	err error
}

func newFuture(run func() (Value, error)) *Future {
	f := &Future{ch: make(chan futResult, 1)}
	go func() {
		v, err := run()
		f.ch <- futResult{v, err}
	}()
	return f
}

// AwaitValue 阻塞解析 Future（非 Future 原样返回）。
func AwaitValue(v Value) (Value, error) {
	f, ok := v.(*Future)
	if !ok {
		return v, nil
	}
	r := <-f.ch
	return r.v, r.err
}

// Env 作用域链（并发安全：parallel 的 thunk 共享闭包环境）。
type Env struct {
	mu     sync.RWMutex
	vars   map[string]Value
	parent *Env
}

func NewEnv(parent *Env) *Env { return &Env{vars: map[string]Value{}, parent: parent} }

func (e *Env) get(name string) (Value, bool) {
	for cur := e; cur != nil; cur = cur.parent {
		cur.mu.RLock()
		v, ok := cur.vars[name]
		cur.mu.RUnlock()
		if ok {
			return v, true
		}
	}
	return nil, false
}

func (e *Env) set(name string, v Value) {
	for cur := e; cur != nil; cur = cur.parent {
		cur.mu.Lock()
		if _, ok := cur.vars[name]; ok {
			cur.vars[name] = v
			cur.mu.Unlock()
			return
		}
		cur.mu.Unlock()
	}
	e.mu.Lock()
	e.vars[name] = v
	e.mu.Unlock()
}

func (e *Env) declare(name string, v Value) { e.Declare(name, v) }

// Declare 在当前作用域声明变量（宿主注入全局用）。
func (e *Env) Declare(name string, v Value) {
	e.mu.Lock()
	e.vars[name] = v
	e.mu.Unlock()
}

// 控制流哨兵（借 error 通道向上传递）。
type returnSig struct{ v Value }
type breakSig struct{}
type continueSig struct{}

func (r *returnSig) Error() string   { return "return" }
func (b *breakSig) Error() string    { return "break" }
func (c *continueSig) Error() string { return "continue" }

// ThrownError throw 语句抛出的 JS 值。
type ThrownError struct{ V Value }

func (t *ThrownError) Error() string { return ToString(t.V) }

// RuntimeError 解释器错误。
type RuntimeError struct{ Msg string }

func (e *RuntimeError) Error() string { return "js: " + e.Msg }

func rtErr(format string, a ...any) error { return &RuntimeError{Msg: fmt.Sprintf(format, a...)} }

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

// ---- 语句 ----

func (ip *Interp) evalStmt(st Node, env *Env) (Value, error) {
	switch s := st.(type) {
	case *VarDecl:
		var last Value
		for _, init := range s.Inits {
			var v Value
			if init.Val != nil {
				var err error
				v, err = ip.evalExpr(init.Val, env)
				if err != nil {
					return nil, err
				}
			}
			env.declare(init.Name, v)
			last = v
		}
		return last, nil
	case *FuncDecl:
		env.declare(s.Name, &Fn{Params: s.Params, Body: s.Body, Env: env, Name: s.Name})
		return nil, nil
	case *IfStmt:
		cond, err := ip.evalExpr(s.Cond, env)
		if err != nil {
			return nil, err
		}
		if Truthy(cond) {
			return ip.execBody(s.Then, env)
		} else if s.Else != nil {
			return ip.execBody(s.Else, env)
		}
		return nil, nil
	case *ForStmt:
		child := NewEnv(env)
		if s.Init != nil {
			if _, err := ip.evalStmt(s.Init, child); err != nil {
				return nil, err
			}
		}
		for {
			if s.Cond != nil {
				c, err := ip.evalExpr(s.Cond, child)
				if err != nil {
					return nil, err
				}
				if !Truthy(c) {
					break
				}
			}
			if _, err := ip.execBody(s.Body, child); err != nil {
				if _, ok := err.(*breakSig); ok {
					break
				}
				if _, ok := err.(*continueSig); ok {
					// 落到 update
				} else {
					return nil, err
				}
			}
			if s.Update != nil {
				if _, err := ip.evalExpr(s.Update, child); err != nil {
					return nil, err
				}
			}
		}
		return nil, nil
	case *ForOfStmt:
		iter, err := ip.evalExpr(s.Iter, env)
		if err != nil {
			return nil, err
		}
		items, err := iterate(iter)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			child := NewEnv(env)
			child.declare(s.Name, item)
			if _, err := ip.execBody(s.Body, child); err != nil {
				if _, ok := err.(*breakSig); ok {
					break
				}
				if _, ok := err.(*continueSig); ok {
					continue
				}
				return nil, err
			}
		}
		return nil, nil
	case *WhileStmt:
		for {
			c, err := ip.evalExpr(s.Cond, env)
			if err != nil {
				return nil, err
			}
			if !Truthy(c) {
				break
			}
			if _, err := ip.execBody(s.Body, env); err != nil {
				if _, ok := err.(*breakSig); ok {
					break
				}
				if _, ok := err.(*continueSig); ok {
					continue
				}
				return nil, err
			}
		}
		return nil, nil
	case *ReturnStmt:
		if s.Val == nil {
			return nil, &returnSig{}
		}
		v, err := ip.evalExpr(s.Val, env)
		if err != nil {
			return nil, err
		}
		return nil, &returnSig{v: v}
	case *BreakStmt:
		return nil, &breakSig{}
	case *ContinueStmt:
		return nil, &continueSig{}
	case *ThrowStmt:
		v, err := ip.evalExpr(s.Val, env)
		if err != nil {
			return nil, err
		}
		return nil, &ThrownError{V: v}
	case *BlockStmt:
		return ip.execBody(s.Body, NewEnv(env))
	case *TryStmt:
		v, err := ip.execBody(s.Body, NewEnv(env))
		if err != nil {
			if _, isCtrl := err.(*returnSig); isCtrl {
				return v, err // return 穿透 try
			}
			if _, isCtrl := err.(*breakSig); isCtrl {
				return v, err
			}
			if _, isCtrl := err.(*continueSig); isCtrl {
				return v, err
			}
			if s.Catch != nil {
				child := NewEnv(env)
				if s.CatchVar != "" {
					msg := err.Error()
					child.declare(s.CatchVar, msg)
					child.declare("message", msg)
				}
				v2, cerr := ip.execBody(s.Catch, child)
				if cerr != nil {
					err = cerr
				} else {
					err = nil
					v = v2
				}
			}
			if s.Finally != nil {
				if _, ferr := ip.execBody(s.Finally, NewEnv(env)); ferr != nil {
					return nil, ferr
				}
			}
			return v, err
		}
		if s.Finally != nil {
			if _, ferr := ip.execBody(s.Finally, NewEnv(env)); ferr != nil {
				return nil, ferr
			}
		}
		return v, nil
	case *ExprStmt:
		return ip.evalExpr(s.X, env)
	}
	return nil, rtErr("unsupported statement %T", st)
}

// execBody 执行语句序列，控制流信号向上抛。
func (ip *Interp) execBody(body []Node, env *Env) (Value, error) {
	var last Value
	for _, st := range body {
		v, err := ip.evalStmt(st, env)
		if err != nil {
			return nil, err
		}
		last = v
	}
	return last, nil
}

// ---- 表达式 ----

func (ip *Interp) evalExpr(x Node, env *Env) (Value, error) {
	switch e := x.(type) {
	case *NumLit:
		return e.V, nil
	case *StrLit:
		return e.V, nil
	case *BoolLit:
		return e.V, nil
	case *NullLit:
		return nil, nil
	case *Ident:
		if v, ok := env.get(e.Name); ok {
			return v, nil
		}
		if v, ok := ip.Global.get(e.Name); ok {
			return v, nil
		}
		if h, ok := ip.Host[e.Name]; ok {
			return &Fn{Native: h, Name: e.Name}, nil
		}
		return nil, rtErr("%s is not defined", e.Name)
	case *TemplateLit:
		var b strings.Builder
		for i, q := range e.Quasis {
			b.WriteString(q)
			if i < len(e.Exprs) {
				v, err := ip.evalExpr(e.Exprs[i], env)
				if err != nil {
					return nil, err
				}
				v, err = AwaitValue(v)
				if err != nil {
					return nil, err
				}
				b.WriteString(ToString(v))
			}
		}
		return b.String(), nil
	case *ArrayLit:
		arr := make([]any, 0, len(e.Elems))
		for _, el := range e.Elems {
			v, err := ip.evalExpr(el, env)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		return arr, nil
	case *ObjectLit:
		obj := make(map[string]any, len(e.Keys))
		for i, k := range e.Keys {
			v, err := ip.evalExpr(e.Vals[i], env)
			if err != nil {
				return nil, err
			}
			obj[k] = v
		}
		return obj, nil
	case *ArrowFn:
		return &Fn{Params: e.Params, Body: e.Body, Expr: e.Expr, Env: env, Async: e.Async}, nil
	case *UnaryExpr:
		v, err := ip.evalExpr(e.X, env)
		if err != nil {
			return nil, err
		}
		switch e.Op {
		case "!":
			return !Truthy(v), nil
		case "-":
			n, ok := v.(float64)
			if !ok {
				return nil, rtErr("unary - expects a number, got %s", TypeOf(v))
			}
			return -n, nil
		case "+":
			if n, ok := v.(float64); ok {
				return n, nil
			}
			return ToNumber(v), nil
		case "~":
			n, ok := v.(float64)
			if !ok {
				return nil, rtErr("~ expects a number")
			}
			return float64(^int64(n)), nil
		case "typeof":
			return TypeOf(v), nil
		}
		return nil, rtErr("unsupported unary %q", e.Op)
	case *AwaitExpr:
		v, err := ip.evalExpr(e.X, env)
		if err != nil {
			return nil, err
		}
		return AwaitValue(v)
	case *LogicalExpr:
		l, err := ip.evalExpr(e.L, env)
		if err != nil {
			return nil, err
		}
		switch e.Op {
		case "&&":
			if !Truthy(l) {
				return l, nil
			}
		case "||":
			if Truthy(l) {
				return l, nil
			}
		case "??":
			if l != nil {
				return l, nil
			}
		}
		return ip.evalExpr(e.R, env)
	case *BinaryExpr:
		l, err := ip.evalExpr(e.L, env)
		if err != nil {
			return nil, err
		}
		r, err := ip.evalExpr(e.R, env)
		if err != nil {
			return nil, err
		}
		return binOp(e.Op, l, r)
	case *CondExpr:
		c, err := ip.evalExpr(e.Cond, env)
		if err != nil {
			return nil, err
		}
		if Truthy(c) {
			return ip.evalExpr(e.Then, env)
		}
		return ip.evalExpr(e.Else, env)
	case *AssignExpr:
		v, err := ip.evalExpr(e.X, env)
		if err != nil {
			return nil, err
		}
		if e.Op != "=" {
			old, ok := env.get(e.Name)
			if !ok {
				return nil, rtErr("%s is not defined", e.Name)
			}
			nv, err := binOp(strings.TrimSuffix(e.Op, "="), old, v)
			if err != nil {
				return nil, err
			}
			env.set(e.Name, nv)
			return nv, nil
		}
		env.set(e.Name, v)
		return v, nil
	case *MemberExpr:
		obj, err := ip.evalExpr(e.Obj, env)
		if err != nil {
			return nil, err
		}
		return ip.getMember(obj, e.Prop, e.Opt)
	case *IndexExpr:
		obj, err := ip.evalExpr(e.Obj, env)
		if err != nil {
			return nil, err
		}
		idx, err := ip.evalExpr(e.Idx, env)
		if err != nil {
			return nil, err
		}
		return ip.getIndex(obj, idx)
	case *CallExpr:
		return ip.evalCall(e, env)
	}
	return nil, rtErr("unsupported expression %T", x)
}

func (ip *Interp) evalCall(e *CallExpr, env *Env) (Value, error) {
	// 方法调用：Fn 为 MemberExpr 时以 obj 为 this 分派内建方法
	if m, ok := e.Fn.(*MemberExpr); ok && !m.Opt {
		obj, err := ip.evalExpr(m.Obj, env)
		if err != nil {
			return nil, err
		}
		if m.Prop == "" { // ?.( 语法
			f, err := ip.evalExpr(m.Obj, env)
			if err != nil {
				return nil, err
			}
			fn, ok := f.(*Fn)
			if !ok {
				return nil, rtErr("value is not a function")
			}
			return ip.invoke(fn, e, env)
		}
		if method, ok := methodOf(obj, m.Prop); ok {
			args := make([]Value, 0, len(e.Args))
			for _, a := range e.Args {
				v, err := ip.evalExpr(a, env)
				if err != nil {
					return nil, err
				}
				v, err = AwaitValue(v)
				if err != nil {
					return nil, err
				}
				args = append(args, v)
			}
			return method(ip, obj, args)
		}
		// 对象成员作为函数值（如 obj.callback）
		mv, err := ip.getMember(obj, m.Prop, false)
		if err != nil {
			return nil, err
		}
		fn, ok := mv.(*Fn)
		if !ok {
			return nil, rtErr("%s is not a function (on %s)", m.Prop, TypeOf(obj))
		}
		return ip.invoke(fn, e, env)
	}
	f, err := ip.evalExpr(e.Fn, env)
	if err != nil {
		return nil, err
	}
	fn, ok := f.(*Fn)
	if !ok {
		return nil, rtErr("value of type %s is not callable", TypeOf(f))
	}
	return ip.invoke(fn, e, env)
}

func (ip *Interp) invoke(fn *Fn, e *CallExpr, env *Env) (Value, error) {
	args := make([]Value, 0, len(e.Args))
	for _, a := range e.Args {
		v, err := ip.evalExpr(a, env)
		if err != nil {
			return nil, err
		}
		args = append(args, v) // 实参中的 Future 保留给 await/宿主解析
	}
	return ip.callFn(fn, args)
}

// getMember 成员访问：map 键、.length、宿主对象（JSON/Math/console）。
func (ip *Interp) getMember(obj Value, prop string, opt bool) (Value, error) {
	switch o := obj.(type) {
	case nil:
		if opt {
			return nil, nil
		}
		return nil, rtErr("cannot read %q of null/undefined", prop)
	case map[string]any:
		return o[prop], nil
	case []any:
		if prop == "length" {
			return float64(len(o)), nil
		}
	case string:
		if prop == "length" {
			return float64(len([]rune(o))), nil
		}
	}
	if opt {
		return nil, nil
	}
	// 宿主对象按名字空间访问：JSON.xxx / Math.xxx
	switch obj {
	case nsJSON:
		return jsonMembers(prop)
	case nsMath:
		return mathMembers(prop)
	case nsConsole:
		if prop == "log" {
			return &Fn{Native: func(ip *Interp, args []Value) (Value, error) {
				var parts []string
				for _, a := range args {
					parts = append(parts, ToString(a))
				}
				if ip.LogOut != nil {
					ip.LogOut(strings.Join(parts, " "))
				}
				return nil, nil
			}, Name: "console.log"}, nil
		}
	}
	return nil, rtErr("property %q not found on %s", prop, TypeOf(obj))
}

func (ip *Interp) getIndex(obj Value, idx Value) (Value, error) {
	switch o := obj.(type) {
	case []any:
		i, ok := idx.(float64)
		if !ok {
			return nil, rtErr("array index must be a number")
		}
		ii := int(i)
		if ii < 0 || ii >= len(o) {
			return nil, nil
		}
		return o[ii], nil
	case map[string]any:
		return o[ToString(idx)], nil
	case string:
		i, ok := idx.(float64)
		if !ok {
			return nil, rtErr("string index must be a number")
		}
		r := []rune(o)
		ii := int(i)
		if ii < 0 || ii >= len(r) {
			return nil, nil
		}
		return string(r[ii]), nil
	case nil:
		return nil, rtErr("cannot index null/undefined")
	}
	return nil, rtErr("cannot index %s", TypeOf(obj))
}

// ---- 运算 ----

func binOp(op string, l, r Value) (Value, error) {
	switch op {
	case "+":
		if ls, ok := l.(string); ok {
			return ls + ToString(r), nil
		}
		if rs, ok := r.(string); ok {
			return ToString(l) + rs, nil
		}
		return ToNumber(l) + ToNumber(r), nil
	case "-":
		return ToNumber(l) - ToNumber(r), nil
	case "*":
		return ToNumber(l) * ToNumber(r), nil
	case "/":
		if ToNumber(r) == 0 {
			return math.NaN(), nil
		}
		return ToNumber(l) / ToNumber(r), nil
	case "%":
		if ToNumber(r) == 0 {
			return math.NaN(), nil
		}
		return math.Mod(ToNumber(l), ToNumber(r)), nil
	case "**":
		return math.Pow(ToNumber(l), ToNumber(r)), nil
	case "==":
		return looseEq(l, r), nil
	case "!=":
		return !looseEq(l, r), nil
	case "===":
		return strictEq(l, r), nil
	case "!==":
		return !strictEq(l, r), nil
	case "<", ">", "<=", ">=":
		if ls, ok := l.(string); ok {
			if rs, ok := r.(string); ok {
				switch op {
				case "<":
					return ls < rs, nil
				case ">":
					return ls > rs, nil
				case "<=":
					return ls <= rs, nil
				default:
					return ls >= rs, nil
				}
			}
		}
		ln, rn := ToNumber(l), ToNumber(r)
		switch op {
		case "<":
			return ln < rn, nil
		case ">":
			return ln > rn, nil
		case "<=":
			return ln <= rn, nil
		default:
			return ln >= rn, nil
		}
	}
	return nil, rtErr("unsupported operator %q", op)
}

func looseEq(l, r Value) bool {
	if l == nil || r == nil {
		return l == nil && r == nil
	}
	if strictEq(l, r) {
		return true
	}
	if lb, ok := l.(bool); ok {
		return looseEq(map[bool]float64{true: 1, false: 0}[lb], r)
	}
	if rb, ok := r.(bool); ok {
		return looseEq(l, map[bool]float64{true: 1, false: 0}[rb])
	}
	if _, ok := l.(float64); ok {
		return ToNumber(r) == ToNumber(l)
	}
	if _, ok := r.(float64); ok {
		return ToNumber(l) == ToNumber(r)
	}
	if _, ok := l.(string); ok {
		if _, ok := r.(string); !ok {
			return ToString(l) == ToString(r)
		}
	}
	return false
}

func strictEq(l, r Value) bool {
	switch a := l.(type) {
	case nil:
		return r == nil
	case float64:
		b, ok := r.(float64)
		return ok && a == b
	case string:
		b, ok := r.(string)
		return ok && a == b
	case bool:
		b, ok := r.(bool)
		return ok && a == b
	}
	return false // 对象/数组/函数按引用相等，简化为 false
}

// Truthy JS 真值。
func Truthy(v Value) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0 && !math.IsNaN(t)
	case string:
		return t != ""
	}
	return true
}

// ToNumber 数值转换。
func ToNumber(v Value) float64 {
	switch t := v.(type) {
	case nil:
		return 0
	case float64:
		return t
	case bool:
		if t {
			return 1
		}
		return 0
	case string:
		n, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return math.NaN()
		}
		return n
	}
	return math.NaN()
}

// ToString 字符串化（拼接与错误展示用）。
func ToString(v Value) string {
	switch t := v.(type) {
	case nil:
		return "undefined"
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1e15 {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case []any:
		var parts []string
		for _, e := range t {
			parts = append(parts, ToString(e))
		}
		return strings.Join(parts, ",")
	}
	return fmt.Sprintf("[%s]", TypeOf(v))
}

// TypeOf typeof 结果。
func TypeOf(v Value) string {
	switch v.(type) {
	case nil:
		return "undefined"
	case float64:
		return "number"
	case string:
		return "string"
	case bool:
		return "boolean"
	case []any, map[string]any:
		return "object"
	case *Fn, *Future:
		return "function"
	}
	return "object"
}

func iterate(v Value) ([]Value, error) {
	switch t := v.(type) {
	case []any:
		out := make([]Value, 0, len(t))
		for _, e := range t {
			out = append(out, e)
		}
		return out, nil
	case string:
		var out []Value
		for _, r := range t {
			out = append(out, string(r))
		}
		return out, nil
	case nil:
		return nil, rtErr("cannot iterate null/undefined")
	}
	return nil, rtErr("for..of expects an array or string, got %s", TypeOf(v))
}

// ---- 命名空间对象（JSON / Math / console）----

type namespace string

const (
	nsJSON    = namespace("JSON")
	nsMath    = namespace("Math")
	nsConsole = namespace("console")
)

func jsonMembers(prop string) (Value, error) {
	switch prop {
	case "stringify":
		return &Fn{Native: func(ip *Interp, args []Value) (Value, error) {
			if len(args) == 0 {
				return nil, rtErr("JSON.stringify expects a value")
			}
			return JSONStringify(args[0]), nil
		}, Name: "JSON.stringify"}, nil
	case "parse":
		return &Fn{Native: func(ip *Interp, args []Value) (Value, error) {
			if len(args) == 0 {
				return nil, rtErr("JSON.parse expects a string")
			}
			v, err := JSONParse(ToString(args[0]))
			if err != nil {
				return nil, &ThrownError{V: err.Error()}
			}
			return v, nil
		}, Name: "JSON.parse"}, nil
	}
	return nil, rtErr("JSON.%s is not supported", prop)
}

func mathMembers(prop string) (Value, error) {
	one := func(f func(float64) float64) Value {
		return &Fn{Native: func(ip *Interp, args []Value) (Value, error) {
			if len(args) == 0 {
				return nil, rtErr("Math expects a number argument")
			}
			return f(ToNumber(args[0])), nil
		}}
	}
	two := func(f func(a, b float64) float64) Value {
		return &Fn{Native: func(ip *Interp, args []Value) (Value, error) {
			if len(args) < 2 {
				return nil, rtErr("Math expects two number arguments")
			}
			return f(ToNumber(args[0]), ToNumber(args[1])), nil
		}}
	}
	switch prop {
	case "floor":
		return one(math.Floor), nil
	case "ceil":
		return one(math.Ceil), nil
	case "round":
		return one(math.Round), nil
	case "abs":
		return one(math.Abs), nil
	case "sqrt":
		return one(math.Sqrt), nil
	case "min":
		return two(func(a, b float64) float64 { return math.Min(a, b) }), nil
	case "max":
		return two(func(a, b float64) float64 { return math.Max(a, b) }), nil
	case "PI":
		return math.Pi, nil
	}
	return nil, rtErr("Math.%s is not supported", prop)
}

// JSONStringify 序列化（无缩进，函数/undefined 跳过）。
func JSONStringify(v Value) string {
	var b strings.Builder
	writeJSON(&b, v)
	return b.String()
}

func writeJSON(b *strings.Builder, v Value) {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case float64:
		b.WriteString(ToString(t))
	case string:
		b.WriteString(strconv.Quote(t))
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSON(b, e)
		}
		b.WriteByte(']')
	case map[string]any:
		b.WriteByte('{')
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(strconv.Quote(k))
			b.WriteByte(':')
			writeJSON(b, t[k])
		}
		b.WriteByte('}')
	default:
		b.WriteString("null")
	}
}
