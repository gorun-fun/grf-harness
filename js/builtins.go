// builtins.go 全局函数（Number/String/Boolean/parseInt/parseFloat/isNaN/
// Array.isArray/Object.keys 等）与实例方法（string/array/map 的常用子集）。
package js

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// installGlobals 安装内建全局绑定。
func (ip *Interp) installGlobals() {
	g := ip.Global
	g.declare("JSON", nsJSON)
	g.declare("Math", nsMath)
	g.declare("console", nsConsole)
	g.declare("Number", &Fn{Native: func(ip *Interp, args []Value) (Value, error) {
		if len(args) == 0 {
			return float64(0), nil
		}
		return ToNumber(args[0]), nil
	}})
	g.declare("String", &Fn{Native: func(ip *Interp, args []Value) (Value, error) {
		if len(args) == 0 {
			return "", nil
		}
		return ToString(args[0]), nil
	}})
	g.declare("Boolean", &Fn{Native: func(ip *Interp, args []Value) (Value, error) {
		if len(args) == 0 {
			return false, nil
		}
		return Truthy(args[0]), nil
	}})
	g.declare("isNaN", &Fn{Native: func(ip *Interp, args []Value) (Value, error) {
		n := 0.0
		if len(args) > 0 {
			n = ToNumber(args[0])
		}
		return n != n, nil
	}})
	g.declare("parseInt", &Fn{Native: func(ip *Interp, args []Value) (Value, error) {
		if len(args) == 0 {
			return nil, rtErr("parseInt expects a string")
		}
		s := strings.TrimSpace(ToString(args[0]))
		i := 0
		if strings.HasPrefix(s, "-") || strings.HasPrefix(s, "+") {
			i = 1
		}
		j := i
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j == i {
			return nil, nil
		}
		n, _ := strconv.ParseInt(s[:j], 10, 64)
		return float64(n), nil
	}})
	g.declare("parseFloat", &Fn{Native: func(ip *Interp, args []Value) (Value, error) {
		if len(args) == 0 {
			return nil, rtErr("parseFloat expects a string")
		}
		n, err := strconv.ParseFloat(strings.TrimSpace(ToString(args[0])), 64)
		if err != nil {
			return nil, nil
		}
		return n, nil
	}})
	g.declare("Array", &Fn{Native: func(ip *Interp, args []Value) (Value, error) {
		arr := make([]any, 0, len(args))
		for _, a := range args {
			arr = append(arr, a)
		}
		return arr, nil
	}})
	isArray := &Fn{Native: func(ip *Interp, args []Value) (Value, error) {
		if len(args) == 0 {
			return false, nil
		}
		_, ok := args[0].([]any)
		return ok, nil
	}}
	g.declare("isArray", isArray)
	g.declare("Object", &Fn{Native: func(ip *Interp, args []Value) (Value, error) {
		return map[string]any{}, nil
	}})
	g.declare("Error", &Fn{Native: func(ip *Interp, args []Value) (Value, error) {
		msg := "error"
		if len(args) > 0 {
			msg = ToString(args[0])
		}
		return map[string]any{"message": msg, "toString": msg}, nil
	}})
}

// methodOf 实例方法表：obj 上名为 prop 的方法。
func methodOf(obj Value, prop string) (func(ip *Interp, this Value, args []Value) (Value, error), bool) {
	switch t := obj.(type) {
	case string:
		if m, ok := stringMethod(prop); ok {
			return func(ip *Interp, this Value, args []Value) (Value, error) {
				return m(this.(string), args)
			}, true
		}
	case []any:
		if m, ok := arrayMethod(prop); ok {
			return func(ip *Interp, this Value, args []Value) (Value, error) {
				return m(ip, this.([]any), args)
			}, true
		}
	case map[string]any:
		_ = t
	}
	return nil, false
}

func argStr(args []Value, i int, def string) string {
	if i < len(args) && args[i] != nil {
		return ToString(args[i])
	}
	return def
}

func argNum(args []Value, i int, def float64) float64 {
	if i < len(args) {
		if n, ok := args[i].(float64); ok {
			return n
		}
	}
	return def
}

// wantFn 取数组方法的回调参数（第 0 参须为函数）；name 为方法名（错误
// 文案用，如 ".map"）。
func wantFn(args []Value, name string) (*Fn, error) {
	fn, ok := Arg(args, 0).(*Fn)
	if !ok {
		return nil, rtErr("%s expects a function", name)
	}
	return fn, nil
}

// ---- string 方法 ----

func stringMethod(prop string) (func(s string, args []Value) (Value, error), bool) {
	switch prop {
	case "toUpperCase":
		return func(s string, args []Value) (Value, error) { return strings.ToUpper(s), nil }, true
	case "toLowerCase":
		return func(s string, args []Value) (Value, error) { return strings.ToLower(s), nil }, true
	case "trim":
		return func(s string, args []Value) (Value, error) { return strings.TrimSpace(s), nil }, true
	case "includes":
		return func(s string, args []Value) (Value, error) {
			return strings.Contains(s, argStr(args, 0, "")), nil
		}, true
	case "startsWith":
		return func(s string, args []Value) (Value, error) {
			return strings.HasPrefix(s, argStr(args, 0, "")), nil
		}, true
	case "endsWith":
		return func(s string, args []Value) (Value, error) {
			return strings.HasSuffix(s, argStr(args, 0, "")), nil
		}, true
	case "indexOf":
		return func(s string, args []Value) (Value, error) {
			return float64(strings.Index(s, argStr(args, 0, ""))), nil
		}, true
	case "slice":
		return func(s string, args []Value) (Value, error) {
			r := []rune(s)
			start, end := clampIdx(int(argNum(args, 0, 0)), len(r)), len(r)
			if a := Arg(args, 1); a != nil {
				end = clampIdx(int(ToNumber(a)), len(r))
			}
			if start > end {
				return "", nil
			}
			return string(r[start:end]), nil
		}, true
	case "split":
		return func(s string, args []Value) (Value, error) {
			sep := argStr(args, 0, ",")
			if sep == "" {
				var out []any
				for _, r := range s {
					out = append(out, string(r))
				}
				return out, nil
			}
			parts := strings.Split(s, sep)
			out := make([]any, 0, len(parts))
			for _, p := range parts {
				out = append(out, p)
			}
			return out, nil
		}, true
	case "replace":
		return func(s string, args []Value) (Value, error) {
			return strings.Replace(s, argStr(args, 0, ""), argStr(args, 1, ""), 1), nil
		}, true
	case "replaceAll":
		return func(s string, args []Value) (Value, error) {
			return strings.ReplaceAll(s, argStr(args, 0, ""), argStr(args, 1, "")), nil
		}, true
	case "padEnd":
		return func(s string, args []Value) (Value, error) {
			n := int(argNum(args, 0, 0))
			pad := argStr(args, 1, " ")
			for len([]rune(s)) < n && pad != "" {
				s += pad
			}
			return s, nil
		}, true
	case "repeat":
		return func(s string, args []Value) (Value, error) {
			return strings.Repeat(s, int(argNum(args, 0, 0))), nil
		}, true
	case "toString":
		return func(s string, args []Value) (Value, error) { return s, nil }, true
	}
	return nil, false
}

func clampIdx(i, n int) int {
	if i < 0 {
		i += n
	}
	if i < 0 {
		return 0
	}
	if i > n {
		return n
	}
	return i
}

// ---- array 方法 ----

func arrayMethod(prop string) (func(ip *Interp, arr []any, args []Value) (Value, error), bool) {
	switch prop {
	case "map":
		return func(ip *Interp, arr []any, args []Value) (Value, error) {
			fn, err := wantFn(args, ".map")
			if err != nil {
				return nil, err
			}
			out := make([]any, 0, len(arr))
			for i, e := range arr {
				v, err := ip.CallValue(fn, e, float64(i), arr)
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			}
			return out, nil
		}, true
	case "filter":
		return func(ip *Interp, arr []any, args []Value) (Value, error) {
			fn, err := wantFn(args, ".filter")
			if err != nil {
				return nil, err
			}
			var out []any
			for i, e := range arr {
				v, err := ip.CallValue(fn, e, float64(i), arr)
				if err != nil {
					return nil, err
				}
				if Truthy(v) {
					out = append(out, e)
				}
			}
			return out, nil
		}, true
	case "forEach":
		return func(ip *Interp, arr []any, args []Value) (Value, error) {
			fn, err := wantFn(args, ".forEach")
			if err != nil {
				return nil, err
			}
			for i, e := range arr {
				if _, err := ip.CallValue(fn, e, float64(i), arr); err != nil {
					return nil, err
				}
			}
			return nil, nil
		}, true
	case "join":
		return func(ip *Interp, arr []any, args []Value) (Value, error) {
			sep := argStr(args, 0, ",")
			var parts []string
			for _, e := range arr {
				if e == nil {
					parts = append(parts, "")
					continue
				}
				parts = append(parts, ToString(e))
			}
			return strings.Join(parts, sep), nil
		}, true
	case "slice":
		return func(ip *Interp, arr []any, args []Value) (Value, error) {
			start, end := clampIdx(int(argNum(args, 0, 0)), len(arr)), len(arr)
			if a := Arg(args, 1); a != nil {
				end = clampIdx(int(ToNumber(a)), len(arr))
			}
			if start > end {
				return []any{}, nil
			}
			out := make([]any, end-start)
			copy(out, arr[start:end])
			return out, nil
		}, true
	case "indexOf":
		return func(ip *Interp, arr []any, args []Value) (Value, error) {
			for i, e := range arr {
				if strictEq(e, Arg(args, 0)) {
					return float64(i), nil
				}
			}
			return float64(-1), nil
		}, true
	case "includes":
		return func(ip *Interp, arr []any, args []Value) (Value, error) {
			for _, e := range arr {
				if strictEq(e, Arg(args, 0)) {
					return true, nil
				}
			}
			return false, nil
		}, true
	case "push":
		return func(ip *Interp, arr []any, args []Value) (Value, error) {
			// 数组是值拷贝：返回追加后的新数组（对 const arr=...; arr.push(x)
			// 无法写回，脚本应使用 concat/展开——此限制在错误信息中说明）
			out := append([]any{}, arr...)
			for _, a := range args {
				out = append(out, a)
			}
			return out, nil
		}, true
	case "concat":
		return func(ip *Interp, arr []any, args []Value) (Value, error) {
			out := append([]any{}, arr...)
			for _, a := range args {
				if sub, ok := a.([]any); ok {
					out = append(out, sub...)
				} else {
					out = append(out, a)
				}
			}
			return out, nil
		}, true
	case "some":
		return func(ip *Interp, arr []any, args []Value) (Value, error) {
			fn, err := wantFn(args, ".some")
			if err != nil {
				return nil, err
			}
			for i, e := range arr {
				v, err := ip.CallValue(fn, e, float64(i), arr)
				if err != nil {
					return nil, err
				}
				if Truthy(v) {
					return true, nil
				}
			}
			return false, nil
		}, true
	case "every":
		return func(ip *Interp, arr []any, args []Value) (Value, error) {
			fn, err := wantFn(args, ".every")
			if err != nil {
				return nil, err
			}
			for i, e := range arr {
				v, err := ip.CallValue(fn, e, float64(i), arr)
				if err != nil {
					return nil, err
				}
				if !Truthy(v) {
					return false, nil
				}
			}
			return true, nil
		}, true
	case "find":
		return func(ip *Interp, arr []any, args []Value) (Value, error) {
			fn, err := wantFn(args, ".find")
			if err != nil {
				return nil, err
			}
			for i, e := range arr {
				v, err := ip.CallValue(fn, e, float64(i), arr)
				if err != nil {
					return nil, err
				}
				if Truthy(v) {
					return e, nil
				}
			}
			return nil, nil
		}, true
	}
	return nil, false
}

// RunEval 解析并执行整段脚本。
func (ip *Interp) RunEval(src string) (Value, error) {
	prog, err := Parse(src)
	if err != nil {
		return nil, err
	}
	return ip.Run(prog)
}

// JSONParse 把 JSON 文本解析为 Value。encoding/json 解码出的类型集合
// （bool/float64/string/nil/[]any/map[string]any）本就是 Value 的合法
// 子集，直接返回。
func JSONParse(s string) (Value, error) {
	var out any
	dec := json.NewDecoder(strings.NewReader(s))
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("invalid JSON: %v", err)
	}
	return out, nil
}
