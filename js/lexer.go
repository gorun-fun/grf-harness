// Package js 一个面向 workflow 工具的迷你 JavaScript 子集解释器，
// 纯标准库实现。支持 workflow 脚本的典型形态：
//
//   - const/let/var、函数声明、箭头函数（表达式体与块体）、闭包
//   - await / async（宿主钩子返回 Future，goroutine 执行）、顶层 await
//   - 对象/数组字面量、成员访问、下标、可选链 ?.、调用
//   - 模板字符串（含 ${} 插值）、字符串/数组/对象常用方法、JSON、Math
//   - if/else、for、for..of、while、break/continue、try/catch/finally、throw
//   - ==/===/!=/!==、<><=>=、+-*/%、&&/||/??/!、三元、赋值与 += -=
//
// 语法或宿主误用抛出的错误一律向上传播终止整个脚本（符合 workflow 工具
// 文档语义）。
package js

import (
	"fmt"
	"strconv"
	"strings"
)

// TokType 令牌类别。
type TokType int

const (
	TokEOF TokType = iota
	TokNum
	TokStr      // '...' 或 "..."
	TokTemplate // `...`（含 ${} 部分，原始文本，解析期展开）
	TokIdent
	TokPunct
	TokKeyword
)

// Tok 单个令牌。
type Tok struct {
	Type TokType
	Val  string // punct/ident/keyword 原文；num 十进制文本；str 已解码文本
	Line int
}

var keywords = map[string]bool{
	"const": true, "let": true, "var": true, "function": true, "return": true,
	"if": true, "else": true, "for": true, "while": true, "break": true,
	"continue": true, "true": true, "false": true, "null": true, "undefined": true,
	"async": true, "await": true, "new": true, "throw": true, "try": true,
	"catch": true, "finally": true, "typeof": true, "of": true, "in": true,
}

// puncts 多字符优先的运算符表。
var puncts = []string{
	"===", "!==", "**", "=>", "==", "!=", "<=", ">=", "&&", "||", "??", "?.",
	"+=", "-=", "*=", "/=", "++", "--",
	"(", ")", "[", "]", "{", "}", ",", ";", ":", "?", ".", "+", "-", "*", "/",
	"%", "!", "<", ">", "=", "&", "|", "^", "~",
}

// Lex 把源码切分为令牌序列。模板字符串作为单个 TokTemplate 令牌返回，
// ${} 内嵌表达式在解析期二次 tokenize。
func Lex(src string) ([]Tok, error) {
	l := &lexer{src: []rune(src), line: 1}
	var toks []Tok
	for {
		t, err := l.next()
		if err != nil {
			return nil, err
		}
		toks = append(toks, t)
		if t.Type == TokEOF {
			return toks, nil
		}
	}
}

type lexer struct {
	src  []rune
	pos  int
	line int
}

func (l *lexer) peek(off int) rune {
	if l.pos+off < len(l.src) {
		return l.src[l.pos+off]
	}
	return 0
}

func (l *lexer) errf(format string, a ...any) error {
	return fmt.Errorf("js: line %d: %s", l.line, fmt.Sprintf(format, a...))
}

func (l *lexer) next() (Tok, error) {
	// 跳过空白与注释
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == '\n' {
			l.line++
			l.pos++
			continue
		}
		if c == ' ' || c == '\t' || c == '\r' {
			l.pos++
			continue
		}
		if c == '/' && l.peek(1) == '/' {
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.pos++
			}
			continue
		}
		if c == '/' && l.peek(1) == '*' {
			l.pos += 2
			for l.pos < len(l.src) && !(l.src[l.pos] == '*' && l.peek(1) == '/') {
				if l.src[l.pos] == '\n' {
					l.line++
				}
				l.pos++
			}
			l.pos += 2
			continue
		}
		break
	}
	if l.pos >= len(l.src) {
		return Tok{Type: TokEOF, Line: l.line}, nil
	}

	startLine := l.line
	c := l.src[l.pos]

	// 数字
	if c >= '0' && c <= '9' || c == '.' && isDigit(l.peek(1)) {
		start := l.pos
		for l.pos < len(l.src) && (isDigit(l.src[l.pos]) || l.src[l.pos] == '.' ||
			l.src[l.pos] == 'e' || l.src[l.pos] == 'E' ||
			((l.src[l.pos] == '+' || l.src[l.pos] == '-') && l.pos > start &&
				(l.src[l.pos-1] == 'e' || l.src[l.pos-1] == 'E'))) {
			l.pos++
		}
		text := string(l.src[start:l.pos])
		if _, err := strconv.ParseFloat(text, 64); err != nil {
			return Tok{}, l.errf("bad number %q", text)
		}
		return Tok{Type: TokNum, Val: text, Line: startLine}, nil
	}

	// 标识符 / 关键字
	if isIdentStart(c) {
		start := l.pos
		for l.pos < len(l.src) && isIdentPart(l.src[l.pos]) {
			l.pos++
		}
		word := string(l.src[start:l.pos])
		if keywords[word] {
			return Tok{Type: TokKeyword, Val: word, Line: startLine}, nil
		}
		return Tok{Type: TokIdent, Val: word, Line: startLine}, nil
	}

	// 字符串
	if c == '\'' || c == '"' {
		return l.lexString(c)
	}
	// 模板字符串
	if c == '`' {
		return l.lexTemplate()
	}

	// 运算符
	for _, p := range puncts {
		if l.match(p) {
			return Tok{Type: TokPunct, Val: p, Line: startLine}, nil
		}
	}
	return Tok{}, l.errf("unexpected character %q", string(c))
}

func (l *lexer) match(p string) bool {
	r := []rune(p)
	if l.pos+len(r) > len(l.src) {
		return false
	}
	for i, c := range r {
		if l.src[l.pos+i] != c {
			return false
		}
	}
	l.pos += len(r)
	return true
}

func (l *lexer) lexString(quote rune) (Tok, error) {
	line := l.line
	l.pos++ // 开引号
	var b strings.Builder
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == quote {
			l.pos++
			return Tok{Type: TokStr, Val: b.String(), Line: line}, nil
		}
		if c == '\n' {
			return Tok{}, l.errf("unterminated string")
		}
		if c == '\\' {
			l.pos++
			if l.pos >= len(l.src) {
				return Tok{}, l.errf("unterminated escape")
			}
			e := l.src[l.pos]
			switch e {
			case 'n':
				b.WriteRune('\n')
			case 't':
				b.WriteRune('\t')
			case 'r':
				b.WriteRune('\r')
			case '\\', '\'', '"', '`', '$':
				b.WriteRune(e)
			case '\n':
				l.line++
			default:
				b.WriteRune(e)
			}
			l.pos++
			continue
		}
		b.WriteRune(c)
		l.pos++
	}
	return Tok{}, l.errf("unterminated string")
}

// lexTemplate 读取整个模板字符串原文（含 ${}），Val 为原始内容（不含反引号）。
// 解析期用 SplitTemplate 拆分静态段与表达式段。
func (l *lexer) lexTemplate() (Tok, error) {
	line := l.line
	l.pos++ // `
	depth := 0
	var b strings.Builder
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == '`' && depth == 0 {
			l.pos++
			return Tok{Type: TokTemplate, Val: b.String(), Line: line}, nil
		}
		if c == '\\' {
			b.WriteRune(c)
			if l.pos+1 < len(l.src) {
				b.WriteRune(l.src[l.pos+1])
				if l.src[l.pos+1] == '\n' {
					l.line++
				}
			}
			l.pos += 2
			continue
		}
		if c == '$' && l.peek(1) == '{' {
			depth++
			b.WriteString("${")
			l.pos += 2
			continue
		}
		if c == '}' && depth > 0 {
			depth--
			b.WriteRune('}')
			l.pos++
			continue
		}
		if c == '{' {
			depth++
		}
		if c == '\n' {
			l.line++
		}
		b.WriteRune(c)
		l.pos++
	}
	return Tok{}, l.errf("unterminated template literal")
}

// SplitTemplate 把模板原文拆为交替的 (静态段, 表达式源码) 列表。
// 返回 quasis[i] 与 exprs[i]：文本、表达式、文本、……、文本。
func SplitTemplate(raw string) (quasis []string, exprs []string, err error) {
	var cur strings.Builder
	for i := 0; i < len(raw); {
		c := raw[i]
		if c == '$' && i+1 < len(raw) && raw[i+1] == '{' {
			quasis = append(quasis, cur.String())
			cur.Reset()
			// 找匹配的 }
			depth := 1
			j := i + 2
			var expr strings.Builder
			for j < len(raw) && depth > 0 {
				switch raw[j] {
				case '{':
					depth++
				case '}':
					depth--
					if depth == 0 {
						j++
						continue
					}
				}
				expr.WriteByte(raw[j])
				j++
			}
			if depth > 0 {
				return nil, nil, fmt.Errorf("js: unterminated ${ in template")
			}
			exprs = append(exprs, expr.String())
			i = j
			continue
		}
		if c == '\\' && i+1 < len(raw) {
			// 模板转义：仅处理常见项
			switch raw[i+1] {
			case 'n':
				cur.WriteByte('\n')
			case 't':
				cur.WriteByte('\t')
			default:
				cur.WriteByte(raw[i+1])
			}
			i += 2
			continue
		}
		cur.WriteByte(c)
		i++
	}
	quasis = append(quasis, cur.String())
	return quasis, exprs, nil
}

func isDigit(c rune) bool { return c >= '0' && c <= '9' }

func isIdentStart(c rune) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c > 127
}

func isIdentPart(c rune) bool { return isIdentStart(c) || isDigit(c) }
