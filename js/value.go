// value.go 值模型与运算：JS 值（nil/float64/string/bool/[]any/map/*Fn/
// *Future）、作用域链 Env、控制流哨兵、错误类型、类型转换与二元运算。
package js

import (
	"fmt"
	"math"
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

// ---- 运算 ----

// cmpOp 次序比较四则归一：三个直接比较结果按 op 映射为 <、<=、>、>=。
// less/equal/greater 须分别传入——浮点 NaN 下三者全为 false 而四则比较
// 恰均为 false，不能用布尔代数互推。字符串与数字两个分支共用。
func cmpOp(op string, less, equal, greater bool) bool {
	switch op {
	case "<":
		return less
	case "<=":
		return less || equal
	case ">":
		return greater
	default: // ">="
		return greater || equal
	}
}

func binOp(op string, l, r Value) (Value, error) {
	switch op {
	case "+":
		if ls, ok := l.(string); ok {
			out := ls + ToString(r)
			if len(out) > maxStringBytes {
				return nil, rtErr("string concatenation exceeds %d-byte cap (memory guard)", maxStringBytes)
			}
			return out, nil
		}
		if rs, ok := r.(string); ok {
			out := ToString(l) + rs
			if len(out) > maxStringBytes {
				return nil, rtErr("string concatenation exceeds %d-byte cap (memory guard)", maxStringBytes)
			}
			return out, nil
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
				return cmpOp(op, ls < rs, ls == rs, ls > rs), nil
			}
		}
		ln, rn := ToNumber(l), ToNumber(r)
		return cmpOp(op, ln < rn, ln == rn, ln > rn), nil
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

// ---- 类型转换 ----

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
