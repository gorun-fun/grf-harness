// eval.go 树遍历求值器：语句与表达式的求值、成员/下标访问、调用分派。
// 值模型与运算见 value.go；JSON/Math/console 命名空间见 namespace.go。
package js

import "strings"

// ---- 语句 ----

// loopSignal 判别循环体内的控制流信号：brk=break、cont=continue；
// 两者皆 false 表示 err 不是循环信号（应向上抛）。
func loopSignal(err error) (brk, cont bool) {
	switch err.(type) {
	case *breakSig:
		return true, false
	case *continueSig:
		return false, true
	}
	return false, false
}

// ctrlSignal 判别控制流信号（return/break/continue）：try 语句中这三类
// 信号必须穿透 catch，不能当异常捕获。
func ctrlSignal(err error) bool {
	switch err.(type) {
	case *returnSig, *breakSig, *continueSig:
		return true
	}
	return false
}

func (ip *Interp) evalStmt(st Node, env *Env) (Value, error) {
	if err := ip.tick(); err != nil {
		return nil, err
	}
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
		return ip.evalFor(s, env)
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
			if err := ip.tick(); err != nil {
				return nil, err
			}
			child := NewEnv(env)
			child.declare(s.Name, item)
			if stop, err := ip.loopBody(s.Body, child); err != nil {
				return nil, err
			} else if stop {
				break
			}
		}
		return nil, nil
	case *WhileStmt:
		for {
			if err := ip.tick(); err != nil {
				return nil, err
			}
			c, err := ip.evalExpr(s.Cond, env)
			if err != nil {
				return nil, err
			}
			if !Truthy(c) {
				break
			}
			if stop, err := ip.loopBody(s.Body, env); err != nil {
				return nil, err
			} else if stop {
				break
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
		return ip.evalTry(s, env)
	case *ExprStmt:
		return ip.evalExpr(s.X, env)
	}
	return nil, rtErr("unsupported statement %T", st)
}

// loopBody 执行循环体的一轮并消化 break/continue 信号：stop=true 表示
// break（外层退出循环）；err 非空表示真实错误（向上抛）；两者皆空表示
// 正常或 continue（外层落到 update / 下一轮）。
func (ip *Interp) loopBody(body []Node, env *Env) (stop bool, err error) {
	if _, err = ip.execBody(body, env); err != nil {
		brk, cont := loopSignal(err)
		if brk {
			return true, nil
		}
		if !cont {
			return false, err
		}
	}
	return false, nil
}

// evalFor 经典三段 for：init 于独立子环境执行，continue 落到 update。
func (ip *Interp) evalFor(s *ForStmt, env *Env) (Value, error) {
	child := NewEnv(env)
	if s.Init != nil {
		if _, err := ip.evalStmt(s.Init, child); err != nil {
			return nil, err
		}
	}
	for {
		if err := ip.tick(); err != nil {
			return nil, err
		}
		if s.Cond != nil {
			c, err := ip.evalExpr(s.Cond, child)
			if err != nil {
				return nil, err
			}
			if !Truthy(c) {
				break
			}
		}
		stop, err := ip.loopBody(s.Body, child)
		if err != nil {
			return nil, err
		}
		if stop {
			break
		}
		// continue/正常：落到 update
		if s.Update != nil {
			if _, err := ip.evalExpr(s.Update, child); err != nil {
				return nil, err
			}
		}
	}
	return nil, nil
}

// evalTry try/catch/finally：return/break/continue 穿透 try 不当异常
// 捕获；catch 捕获后仍出错则以后者为准；finally 出错优先向上。
func (ip *Interp) evalTry(s *TryStmt, env *Env) (Value, error) {
	v, err := ip.execBody(s.Body, NewEnv(env))
	if err != nil {
		// return/break/continue 穿透 try，不当异常捕获
		if ctrlSignal(err) {
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
	}
	if s.Finally != nil {
		if _, ferr := ip.execBody(s.Finally, NewEnv(env)); ferr != nil {
			return nil, ferr
		}
	}
	return v, err
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
	if err := ip.enter(); err != nil {
		return nil, err
	}
	defer ip.leave()
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
	if err := ip.tick(); err != nil {
		return nil, err
	}
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
