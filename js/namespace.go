// namespace.go 内建命名空间对象：JSON / Math / console（标记值经
// getMember 按名字分派）与 JSONStringify 序列化。
package js

import (
	"math"
	"sort"
	"strconv"
	"strings"
)

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
