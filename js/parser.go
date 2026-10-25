// parser.go 语法分析：语句 + Pratt 表达式。AST 以接口 + 结构体节点表示。
package js

import (
	"fmt"
	"strconv"
	"strings"
)

// Node AST 节点。
type Node interface{}

type Program struct{ Stmts []Node }

// ---- 语句 ----

type VarDecl struct {
	Kind  string // const/let/var
	Inits []InitDecl
}
type InitDecl struct {
	Name string
	Val  Node
}

type FuncDecl struct {
	Name   string
	Params []string
	Body   []Node
	Async  bool
}

type IfStmt struct {
	Cond Node
	Then []Node
	Else []Node // nil 表示无 else
}

type ForStmt struct {
	Init   Node // *VarDecl 或赋值表达式或 nil
	Cond   Node
	Update Node
	Body   []Node
}

type ForOfStmt struct {
	Name string
	Iter Node
	Body []Node
}

type WhileStmt struct {
	Cond Node
	Body []Node
}

type ReturnStmt struct{ Val Node }
type BreakStmt struct{}
type ContinueStmt struct{}
type ThrowStmt struct{ Val Node }
type BlockStmt struct{ Body []Node }

type TryStmt struct {
	Body     []Node
	CatchVar string
	Catch    []Node
	Finally  []Node
}

type ExprStmt struct{ X Node }

// ---- 表达式 ----

type NumLit struct{ V float64 }
type StrLit struct{ V string }
type BoolLit struct{ V bool }
type NullLit struct{}

type Ident struct{ Name string }

type TemplateLit struct {
	Quasis []string
	Exprs  []Node
}

type ArrayLit struct{ Elems []Node }

type ObjectLit struct {
	Keys []string
	Vals []Node
}

type UnaryExpr struct {
	Op string // ! - + typeof
	X  Node
}

type BinaryExpr struct {
	Op   string
	L, R Node
}

type LogicalExpr struct {
	Op   string // && || ??
	L, R Node
}

type AssignExpr struct {
	Op   string // = += -= *= /=
	Name string
	X    Node
}

type CondExpr struct{ Cond, Then, Else Node }

type CallExpr struct {
	Fn   Node
	Args []Node
}

type MemberExpr struct {
	Obj  Node
	Prop string
	Opt  bool // ?.
}

type IndexExpr struct {
	Obj Node
	Idx Node
	Opt bool
}

type ArrowFn struct {
	Params []string
	Body   []Node // 块体
	Expr   Node   // 表达式体（与 Body 互斥）
	Async  bool
}

type AwaitExpr struct{ X Node }

// ---- 解析器 ----

type parser struct {
	toks  []Tok
	pos   int
	depth int // 语法嵌套深度（expr/unary/primary 三处递归入口共用计数）
}

// maxParseDepth 语法嵌套深度上限：递归下降解析器自身也消耗 Go 栈，
// 超长嵌套表达式（数 MB 的左括号/数组字面量）会把解析打爆成不可恢复
// 的栈溢出 fatal，超限按语法错误终止。
const maxParseDepth = 10_000

// enter 语法递归入口（expr/unary/primary）配对计数，超深即语法错误。
func (p *parser) enter() error {
	p.depth++
	if p.depth > maxParseDepth {
		return p.errf("syntax nesting too deep (>%d)", maxParseDepth)
	}
	return nil
}

func (p *parser) exit() { p.depth-- }

// Parse 解析整段程序。
func Parse(src string) (*Program, error) {
	toks, err := Lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	prog := &Program{}
	for !p.atEOF() {
		st, err := p.statement()
		if err != nil {
			return nil, err
		}
		if st != nil {
			prog.Stmts = append(prog.Stmts, st)
		}
	}
	return prog, nil
}

func (p *parser) cur() Tok    { return p.toks[p.pos] }
func (p *parser) next() Tok   { t := p.toks[p.pos]; p.pos++; return t }
func (p *parser) atEOF() bool { return p.cur().Type == TokEOF }

func (p *parser) isPunct(v string) bool {
	t := p.cur()
	return t.Type == TokPunct && t.Val == v
}

func (p *parser) isKw(v string) bool {
	t := p.cur()
	return t.Type == TokKeyword && t.Val == v
}

func (p *parser) expectPunct(v string) error {
	if !p.isPunct(v) {
		return p.errf("expected %q, got %q", v, p.cur().Val)
	}
	p.pos++
	return nil
}

func (p *parser) errf(format string, a ...any) error {
	return fmt.Errorf("js: line %d: %s", p.cur().Line, fmt.Sprintf(format, a...))
}

// consumeSemi 消费可选分号。
func (p *parser) consumeSemi() {
	if p.isPunct(";") {
		p.pos++
	}
}

// block 解析 { ... }。
func (p *parser) block() ([]Node, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var body []Node
	for !p.isPunct("}") {
		if p.atEOF() {
			return nil, p.errf("unexpected end of input in block")
		}
		st, err := p.statement()
		if err != nil {
			return nil, err
		}
		if st != nil {
			body = append(body, st)
		}
	}
	p.pos++ // }
	return body, nil
}

func (p *parser) statement() (Node, error) {
	t := p.cur()
	switch {
	case t.Type == TokKeyword:
		switch t.Val {
		case "const", "let", "var":
			p.pos++
			d, err := p.varDeclInit(t.Val, "expected identifier in declaration")
			if err != nil {
				return nil, err
			}
			p.consumeSemi()
			return d, nil
		case "function":
			return p.funcDeclStmt()
		case "if":
			return p.ifStmt()
		case "for":
			return p.forStmt()
		case "while":
			return p.whileStmt()
		case "return":
			return p.returnStmt(t.Line)
		case "break":
			p.pos++
			p.consumeSemi()
			return &BreakStmt{}, nil
		case "continue":
			p.pos++
			p.consumeSemi()
			return &ContinueStmt{}, nil
		case "throw":
			p.pos++
			v, err := p.expr(0)
			if err != nil {
				return nil, err
			}
			p.consumeSemi()
			return &ThrowStmt{Val: v}, nil
		case "try":
			return p.tryStmt()
		}
	case t.Type == TokPunct && t.Val == "{":
		body, err := p.block()
		if err != nil {
			return nil, err
		}
		return &BlockStmt{Body: body}, nil
	}
	// 表达式语句
	x, err := p.expr(0)
	if err != nil {
		return nil, err
	}
	p.consumeSemi()
	return &ExprStmt{X: x}, nil
}

// varDeclInit 解析声明初始化列表："name [= expr] (, name [= expr])*"。
// const/let/var 语句与 for 初始化共用的形态；关键字与收尾分号由调用方
// 处理。identErr 为标识符缺失时的错误文案前缀（两处调用点历史文案
// 不同，保持原样）。
func (p *parser) varDeclInit(kind, identErr string) (*VarDecl, error) {
	d := &VarDecl{Kind: kind}
	for {
		name := p.next()
		if name.Type != TokIdent {
			return nil, p.errf("%s, got %q", identErr, name.Val)
		}
		var val Node
		if p.isPunct("=") {
			p.pos++
			v, err := p.expr(0)
			if err != nil {
				return nil, err
			}
			val = v
		}
		d.Inits = append(d.Inits, InitDecl{Name: name.Val, Val: val})
		if p.isPunct(",") {
			p.pos++
			continue
		}
		break
	}
	return d, nil
}

func (p *parser) tryStmt() (Node, error) {
	p.pos++ // try
	body, err := p.block()
	if err != nil {
		return nil, err
	}
	st := &TryStmt{Body: body}
	if p.isKw("catch") {
		p.pos++
		if p.isPunct("(") {
			p.pos++
			name := p.next()
			if name.Type != TokIdent {
				return nil, p.errf("catch variable expected")
			}
			st.CatchVar = name.Val
			if err := p.expectPunct(")"); err != nil {
				return nil, err
			}
		}
		st.Catch, err = p.block()
		if err != nil {
			return nil, err
		}
	}
	if p.isKw("finally") {
		p.pos++
		st.Finally, err = p.block()
		if err != nil {
			return nil, err
		}
	}
	if st.Catch == nil && st.Finally == nil {
		return nil, p.errf("try requires catch or finally")
	}
	return st, nil
}

// funcDeclStmt 函数声明（function name(params) { ... }）。
func (p *parser) funcDeclStmt() (Node, error) {
	p.pos++ // function
	name := p.next()
	if name.Type != TokIdent {
		return nil, p.errf("function name expected")
	}
	params, err := p.paramList()
	if err != nil {
		return nil, err
	}
	body, err := p.block()
	if err != nil {
		return nil, err
	}
	return &FuncDecl{Name: name.Val, Params: params, Body: body}, nil
}

// ifStmt if 语句（else / else if 链）。
func (p *parser) ifStmt() (Node, error) {
	p.pos++ // if
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	cond, err := p.expr(0)
	if err != nil {
		return nil, err
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	then, err := p.block()
	if err != nil {
		return nil, err
	}
	st := &IfStmt{Cond: cond, Then: then}
	if p.isKw("else") {
		p.pos++
		if p.isKw("if") {
			els, err := p.statement()
			if err != nil {
				return nil, err
			}
			st.Else = []Node{els}
		} else {
			els, err := p.block()
			if err != nil {
				return nil, err
			}
			st.Else = els
		}
	}
	return st, nil
}

// whileStmt while 循环。
func (p *parser) whileStmt() (Node, error) {
	p.pos++ // while
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	cond, err := p.expr(0)
	if err != nil {
		return nil, err
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	body, err := p.block()
	if err != nil {
		return nil, err
	}
	return &WhileStmt{Cond: cond, Body: body}, nil
}

// returnStmt return 语句；line 是 return 关键字所在行（其后换行 =
// 无值 return，undefined）。
func (p *parser) returnStmt(line int) (Node, error) {
	p.pos++ // return
	// return 后换行 → undefined
	if p.isPunct(";") || p.isPunct("}") || p.cur().Line > line {
		p.consumeSemi()
		return &ReturnStmt{}, nil
	}
	v, err := p.expr(0)
	if err != nil {
		return nil, err
	}
	p.consumeSemi()
	return &ReturnStmt{Val: v}, nil
}

func (p *parser) forStmt() (Node, error) {
	p.pos++ // for
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	// for (const x of iter)
	if p.isKw("const") || p.isKw("let") || p.isKw("var") {
		save := p.pos
		p.pos++
		name := p.next()
		if name.Type == TokIdent && p.isKw("of") {
			p.pos++
			iter, err := p.expr(0)
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(")"); err != nil {
				return nil, err
			}
			body, err := p.block()
			if err != nil {
				return nil, err
			}
			return &ForOfStmt{Name: name.Val, Iter: iter, Body: body}, nil
		}
		p.pos = save
	}
	// 经典 for(init;cond;update)
	var init Node
	if !p.isPunct(";") {
		// 仅支持声明或赋值表达式
		if p.isKw("const") || p.isKw("let") || p.isKw("var") {
			p.pos++
			// 声明 kind 历史上固定记 "let"（求值不区分 kind），保持原样
			d, err := p.varDeclInit("let", "expected identifier")
			if err != nil {
				return nil, err
			}
			init = d
		} else {
			e, err := p.expr(0)
			if err != nil {
				return nil, err
			}
			init = &ExprStmt{X: e}
		}
	}
	if err := p.expectPunct(";"); err != nil {
		return nil, err
	}
	var cond Node
	if !p.isPunct(";") {
		c, err := p.expr(0)
		if err != nil {
			return nil, err
		}
		cond = c
	}
	if err := p.expectPunct(";"); err != nil {
		return nil, err
	}
	var update Node
	if !p.isPunct(")") {
		u, err := p.expr(0)
		if err != nil {
			return nil, err
		}
		update = u
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	body, err := p.block()
	if err != nil {
		return nil, err
	}
	return &ForStmt{Init: init, Cond: cond, Update: update, Body: body}, nil
}

// paramList (a, b, c)
func (p *parser) paramList() ([]string, error) {
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	var params []string
	for !p.isPunct(")") {
		t := p.next()
		if t.Type != TokIdent {
			return nil, p.errf("parameter name expected, got %q", t.Val)
		}
		params = append(params, t.Val)
		if p.isPunct(",") {
			p.pos++
		}
	}
	p.pos++ // )
	return params, nil
}

// ---- Pratt 表达式 ----

// 二元运算符绑定力。
func binPrec(op string) int {
	switch op {
	case "??":
		return 1
	case "||":
		return 2
	case "&&":
		return 3
	case "==", "!=", "===", "!==":
		return 4
	case "<", ">", "<=", ">=":
		return 5
	case "+", "-":
		return 6
	case "*", "/", "%", "**":
		return 7
	}
	return 0
}

func (p *parser) expr(minPrec int) (Node, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.exit()
	left, err := p.unary()
	if err != nil {
		return nil, err
	}
	// 赋值：仅支持简单变量目标，右结合
	if minPrec == 0 {
		for _, op := range []string{"=", "+=", "-=", "*=", "/="} {
			if p.isPunct(op) {
				id, ok := left.(*Ident)
				if !ok {
					return nil, p.errf("assignment target must be an identifier")
				}
				p.pos++
				right, err := p.expr(0)
				if err != nil {
					return nil, err
				}
				left = &AssignExpr{Op: op, Name: id.Name, X: right}
				continue
			}
		}
	}
	for {
		t := p.cur()
		if t.Type == TokPunct {
			op := t.Val
			if prec := binPrec(op); prec > 0 && prec >= minPrec {
				p.pos++
				right, err := p.expr(prec + 1)
				if err != nil {
					return nil, err
				}
				if op == "&&" || op == "||" || op == "??" {
					left = &LogicalExpr{Op: op, L: left, R: right}
				} else {
					left = &BinaryExpr{Op: op, L: left, R: right}
				}
				continue
			}
			if op == "?" && minPrec <= 1 {
				// 三元
				p.pos++
				then, err := p.expr(0)
				if err != nil {
					return nil, err
				}
				if err := p.expectPunct(":"); err != nil {
					return nil, err
				}
				els, err := p.expr(0)
				if err != nil {
					return nil, err
				}
				left = &CondExpr{Cond: left, Then: then, Else: els}
				continue
			}
		}
		break
	}
	return left, nil
}

// tryArrow 尝试在当前位置解析箭头函数（回退式：失败恢复 save 位置）。
func (p *parser) tryArrow(save int) (Node, bool, error) {
	p.pos = save
	fail := func() (Node, bool, error) { p.pos = save; return nil, false, nil }
	// async (params) => 或 (params) => 或 ident =>
	async := false
	if p.isKw("async") {
		// 仅 async (…) => / async x =>
		lookahead := p.toks[p.pos+1]
		isFn := (lookahead.Type == TokPunct && lookahead.Val == "(") ||
			(lookahead.Type == TokIdent && p.isArrowAt(p.pos+2))
		if !isFn {
			return fail()
		}
		async = true
		p.pos++
	}
	var params []string
	if p.isPunct("(") {
		pl, err := p.paramList()
		if err != nil {
			return fail()
		}
		params = pl
	} else if p.cur().Type == TokIdent {
		params = []string{p.next().Val}
	} else {
		return fail()
	}
	if !p.isPunct("=>") {
		return fail()
	}
	p.pos++ // =>
	fn := &ArrowFn{Params: params, Async: async}
	if p.isPunct("{") {
		body, err := p.block()
		if err != nil {
			return nil, false, err
		}
		fn.Body = body
	} else {
		e, err := p.expr(0)
		if err != nil {
			return nil, false, err
		}
		fn.Expr = e
	}
	return fn, true, nil
}

// tryArrowOr 箭头探测统一入口：done 为 true 表示已有结论（解析成功给出
// 节点，或探测失败给出 err），调用方直接返回 n、err；done 为 false 表示
// 不是箭头函数（位置已复原到 save），回退普通解析。
func (p *parser) tryArrowOr(save int) (n Node, done bool, err error) {
	n, ok, err := p.tryArrow(save)
	if ok || err != nil {
		return n, true, err
	}
	return nil, false, nil
}

// isArrowAt 判断 pos 处是否为 =>。
func (p *parser) isArrowAt(pos int) bool {
	if pos < len(p.toks) {
		t := p.toks[pos]
		return t.Type == TokPunct && t.Val == "=>"
	}
	return false
}

// incDec 把前缀/后缀 ++/-- 归一为 "+= ±1" 赋值节点（两处语法糖共用，
// 求值语义等价）。
func incDec(op, name string) Node {
	delta := 1.0
	if op == "--" {
		delta = -1
	}
	return &AssignExpr{Op: "+=", Name: name, X: &NumLit{V: delta}}
}

func (p *parser) unary() (Node, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.exit()
	t := p.cur()
	if t.Type == TokPunct {
		switch t.Val {
		case "!", "-", "+", "~":
			p.pos++
			x, err := p.unary()
			if err != nil {
				return nil, err
			}
			return &UnaryExpr{Op: t.Val, X: x}, nil
		case "++", "--":
			// 前缀自增：仅支持简单变量
			p.pos++
			if p.cur().Type != TokIdent {
				return nil, p.errf("++/-- expects an identifier")
			}
			name := p.next().Val
			return incDec(t.Val, name), nil
		}
	}
	if t.Type == TokKeyword {
		switch t.Val {
		case "typeof":
			p.pos++
			x, err := p.unary()
			if err != nil {
				return nil, err
			}
			return &UnaryExpr{Op: "typeof", X: x}, nil
		case "await":
			p.pos++
			x, err := p.unary()
			if err != nil {
				return nil, err
			}
			return &AwaitExpr{X: x}, nil
		case "new":
			// new Error(...) / new Map() 等：按普通调用处理
			p.pos++
			return p.unary()
		}
	}
	return p.postfix()
}

// postfix 一元后缀：调用 / 成员 / 下标 / 后缀 ++ --。
func (p *parser) postfix() (Node, error) {
	x, err := p.primary()
	if err != nil {
		return nil, err
	}
	for {
		switch {
		case p.isPunct("."):
			p.pos++
			prop := p.next()
			if prop.Type != TokIdent && prop.Type != TokKeyword {
				return nil, p.errf("property name expected after '.', got %q", prop.Val)
			}
			x = &MemberExpr{Obj: x, Prop: prop.Val}
		case p.isPunct("?."):
			p.pos++
			if p.isPunct("(") {
				args, err := p.args()
				if err != nil {
					return nil, err
				}
				x = &CallExpr{Fn: &MemberExpr{Obj: x, Prop: ""}, Args: args}
				// 简化：?.( 直接按调用
			} else {
				prop := p.next()
				x = &MemberExpr{Obj: x, Prop: prop.Val, Opt: true}
			}
		case p.isPunct("["):
			p.pos++
			idx, err := p.expr(0)
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct("]"); err != nil {
				return nil, err
			}
			x = &IndexExpr{Obj: x, Idx: idx}
		case p.isPunct("("):
			args, err := p.args()
			if err != nil {
				return nil, err
			}
			x = &CallExpr{Fn: x, Args: args}
		case p.isPunct("++") || p.isPunct("--"):
			op := p.next().Val
			id, ok := x.(*Ident)
			if !ok {
				return nil, p.errf("%s expects an identifier", op)
			}
			x = incDec(op, id.Name)
		default:
			return x, nil
		}
	}
}

func (p *parser) args() ([]Node, error) {
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	var out []Node
	for !p.isPunct(")") {
		if p.atEOF() {
			return nil, p.errf("unterminated call arguments")
		}
		a, err := p.expr(0)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
		if p.isPunct(",") {
			p.pos++
		}
	}
	p.pos++ // )
	return out, nil
}

func (p *parser) primary() (Node, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.exit()
	t := p.cur()
	switch t.Type {
	case TokNum:
		p.pos++
		v, err := strconv.ParseFloat(t.Val, 64)
		if err != nil {
			return nil, p.errf("bad number %q", t.Val)
		}
		return &NumLit{V: v}, nil
	case TokStr:
		p.pos++
		return &StrLit{V: t.Val}, nil
	case TokTemplate:
		p.pos++
		return p.template(t.Val)
	case TokIdent:
		// 可能是箭头函数 ident =>
		if p.isArrowAt(p.pos + 1) {
			if n, done, err := p.tryArrowOr(p.pos); done {
				return n, err
			}
		}
		p.pos++
		return &Ident{Name: t.Val}, nil
	case TokKeyword:
		switch t.Val {
		case "true":
			p.pos++
			return &BoolLit{V: true}, nil
		case "false":
			p.pos++
			return &BoolLit{V: false}, nil
		case "null", "undefined":
			p.pos++
			return &NullLit{}, nil
		case "function":
			p.pos++
			params, err := p.paramList()
			if err != nil {
				return nil, err
			}
			body, err := p.block()
			if err != nil {
				return nil, err
			}
			return &ArrowFn{Params: params, Body: body}, nil
		case "async":
			// async 箭头函数尝试
			if n, done, err := p.tryArrowOr(p.pos); done {
				return n, err
			}
			return nil, p.errf("unexpected 'async'")
		}
		return nil, p.errf("unexpected keyword %q", t.Val)
	case TokPunct:
		switch t.Val {
		case "(":
			// 可能是 (params) => 箭头函数
			if n, done, err := p.tryArrowOr(p.pos); done {
				return n, err
			}
			p.pos++
			x, err := p.expr(0)
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(")"); err != nil {
				return nil, err
			}
			return x, nil
		case "[":
			return p.arrayLit()
		case "{":
			return p.objectLit()
		}
	}
	return nil, p.errf("unexpected token %q", t.Val)
}

// arrayLit 数组字面量 [a, b, ...]（尾逗号容忍）。
func (p *parser) arrayLit() (Node, error) {
	p.pos++ // [
	arr := &ArrayLit{}
	for !p.isPunct("]") {
		if p.atEOF() {
			return nil, p.errf("unterminated array literal")
		}
		e, err := p.expr(0)
		if err != nil {
			return nil, err
		}
		arr.Elems = append(arr.Elems, e)
		if p.isPunct(",") {
			p.pos++
		}
	}
	p.pos++
	return arr, nil
}

// objectLit 对象字面量 {key: value, ...}（键为标识符/关键字/字符串）。
func (p *parser) objectLit() (Node, error) {
	p.pos++ // {
	obj := &ObjectLit{}
	for !p.isPunct("}") {
		if p.atEOF() {
			return nil, p.errf("unterminated object literal")
		}
		key := p.next()
		var keyName string
		switch key.Type {
		case TokIdent, TokKeyword:
			keyName = key.Val
		case TokStr:
			keyName = key.Val
		default:
			return nil, p.errf("object key expected, got %q", key.Val)
		}
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		v, err := p.expr(0)
		if err != nil {
			return nil, err
		}
		obj.Keys = append(obj.Keys, keyName)
		obj.Vals = append(obj.Vals, v)
		if p.isPunct(",") {
			p.pos++
		}
	}
	p.pos++
	return obj, nil
}

// template 解析模板字面量为 TemplateLit。
func (p *parser) template(raw string) (Node, error) {
	quasis, exprSrcs, err := SplitTemplate(raw)
	if err != nil {
		return nil, p.errf("%v", err)
	}
	tl := &TemplateLit{Quasis: quasis}
	for _, src := range exprSrcs {
		sub, err := Parse(strings.TrimSpace(src))
		if err != nil {
			return nil, err
		}
		if len(sub.Stmts) == 0 {
			tl.Exprs = append(tl.Exprs, &NullLit{})
			continue
		}
		es, ok := sub.Stmts[0].(*ExprStmt)
		if !ok {
			return nil, p.errf("template expression must be an expression")
		}
		tl.Exprs = append(tl.Exprs, es.X)
	}
	return tl, nil
}
