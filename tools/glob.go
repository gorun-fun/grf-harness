// glob.go 文件发现（glob）：mtime 序（新→旧），先 100 条内联；更多存
// 文件报告路径。无 "/" 的模式匹配任意深度的 basename；含 "/" 时按相对
// 路径分段匹配，支持 ** 跨目录层级与 {a,b} 花括号交替（与 grep include
// 语义一致）；排除 VCS 元数据目录。
package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// globArgs glob 工具参数（具名类型供 spec 交叉校验测试反射 json tag）。
type globArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

func registerGlob(r *Registry, workspace string, fs *FsPolicy, artifacts *Artifacts) {
	r.Register("glob", func(ctx context.Context, args string) (Result, error) {
		var a globArgs
		if err := decodeArgs(args, &a); err != nil {
			return Result{}, err
		}
		if a.Pattern == "" {
			return Result{}, fmt.Errorf("pattern is required")
		}
		base := workspace
		if a.Path != "" {
			// 显式指定搜索根时做读边界校验（默认根就是工作区，无须校验）
			if _, err := fs.CheckRead(a.Path); err != nil {
				return Result{}, err
			}
			base = a.Path
		}
		matches, err := GlobFiles(a.Pattern, base)
		if err != nil {
			return Result{}, err
		}
		if len(matches) == 0 {
			return TextResult("no files matched"), nil
		}
		if len(matches) <= 100 {
			return TextResult(strings.Join(matches, "\n") +
				fmt.Sprintf("\n(%d path%s, modification-time order)", len(matches), plural(len(matches)))), nil
		}
		path, err := artifacts.Save("glob", strings.Join(matches, "\n")+"\n")
		if err != nil {
			return Result{}, err
		}
		return TextResult(fmt.Sprintf("%d paths matched (modification-time order); first 100:\n%s\n\nthe complete sorted list was saved to %s",
			len(matches), strings.Join(matches[:100], "\n"), path)), nil
	})
}

// walkFiles glob/grep 共用的目录遍历骨架：绝对化搜索根，忽略不可读项，
// 跳过 VCS 元数据目录（.git/.hg/.svn，搜索根本身除外），d.Info() 失败或
// Rel 换算失败的文件跳过，其余以 (path, rel, info) 回调 visit；visit 返回
// false 时提前结束整趟遍历（grep 的 cap 截断用）。不区分文件类型，
// 是否只要普通文件由 visit 自行判断（grep 过滤非常规文件，glob 不限）。
func walkFiles(base string, visit func(path, rel string, info os.FileInfo) bool) {
	baseAbs := absOr(base)
	_ = filepath.WalkDir(baseAbs, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 不可读的目录直接跳过
		}
		if d.IsDir() {
			name := d.Name()
			if p != baseAbs && (name == ".git" || name == ".hg" || name == ".svn") {
				return filepath.SkipDir
			}
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		rel, rerr := filepath.Rel(baseAbs, p)
		if rerr != nil {
			return nil
		}
		if !visit(p, rel, info) {
			return filepath.SkipAll
		}
		return nil
	})
}

// GlobFiles 实现 glob 语义：返回按修改时间新→旧排序的匹配文件（仅文件）。
// 模式先做花括号展开（**/*.{html,md} = **/*.html 或 **/*.md），任一展开
// 形式命中即匹配。
func GlobFiles(pattern, base string) ([]string, error) {
	pats := expandBraces(pattern)
	type ent struct {
		rel string
		mod time.Time
	}
	var found []ent
	walkFiles(base, func(_, rel string, info os.FileInfo) bool {
		for _, p := range pats {
			if matchGlob(p, rel) {
				found = append(found, ent{rel: rel, mod: info.ModTime()})
				break
			}
		}
		return true
	})
	sort.Slice(found, func(i, j int) bool {
		if found[i].mod.Equal(found[j].mod) {
			return found[i].rel < found[j].rel
		}
		return found[i].mod.After(found[j].mod)
	})
	out := make([]string, 0, len(found))
	for _, e := range found {
		out = append(out, e.rel)
	}
	return out, nil
}

// matchGlob 匹配模式与相对路径：无 "/" 的模式匹配 basename；否则按段
// 匹配，** 可跨任意层级。
func matchGlob(pattern, rel string) bool {
	rel = filepath.ToSlash(rel)
	if !strings.Contains(pattern, "/") {
		ok, err := matchSeg(pattern, pathBase(rel))
		return err == nil && ok
	}
	segs := strings.Split(rel, "/")
	return matchSegs(strings.Split(pattern, "/"), segs)
}

func pathBase(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

func matchSegs(pat, segs []string) bool {
	if len(pat) == 0 {
		return len(segs) == 0
	}
	if pat[0] == "**" {
		// ** 匹配 0..n 段
		for skip := 0; skip <= len(segs); skip++ {
			if matchSegs(pat[1:], segs[skip:]) {
				return true
			}
		}
		return false
	}
	if len(segs) == 0 {
		return false
	}
	ok, err := matchSeg(pat[0], segs[0])
	if err != nil || !ok {
		return false
	}
	return matchSegs(pat[1:], segs[1:])
}

// matchSeg 单段匹配：支持 * ? [...]。
func matchSeg(pat, s string) (bool, error) {
	return filepath.Match(pat, s)
}

// expandBraces 花括号交替展开：**/*.{html,md} → **/*.html 与 **/*.md，
// 与 grep include 的 brace 语义一致（配对组无顶层逗号时展开为组内容本身，
// 同 bash：a{b} → ab）；不配对的花括号与 [字符类]、反斜杠转义内的花括号
// 保持字面量；支持嵌套；组合数超上限时整体按原样处理（病态模式防爆炸）。
func expandBraces(pattern string) []string {
	const maxPatterns = 128
	out := []string{pattern}
	for {
		var next []string
		progressed := false
		for _, p := range out {
			lo, hi, alts, ok := firstExpandable(p)
			if !ok {
				next = append(next, p)
				continue
			}
			progressed = true
			for _, a := range alts {
				next = append(next, p[:lo]+a+p[hi+1:])
			}
		}
		if !progressed {
			return out
		}
		if len(next) > maxPatterns {
			return []string{pattern}
		}
		out = next
	}
}

// firstExpandable 找模式中第一个配对的花括号组：返回组的开/闭下标与按
// 顶层逗号切分的替代串（无逗号即单替代=组内容）；无配对组返回 ok=false。
func firstExpandable(p string) (lo, hi int, alts []string, ok bool) {
	for i := 0; i < len(p); i++ {
		switch p[i] {
		case '\\':
			i++ // 转义：花括号/方括号均不参与结构
		case '[':
			i = skipClass(p, i)
		case '{':
			close, _, paired := braceGroup(p, i)
			if !paired {
				continue // 不配对：字面量
			}
			return i, close, splitTopCommas(p[i+1 : close]), true
		}
	}
	return 0, 0, nil, false
}

// braceGroup 从 p[i]=='{' 找配对 '}'：返回闭合下标、首个顶层逗号下标
// （无则 -1）、是否配对。跳过嵌套花括号、[字符类] 与转义。
func braceGroup(p string, i int) (close, comma int, paired bool) {
	comma = -1
	depth, inCls := 1, false
	for j := i + 1; j < len(p); j++ {
		switch p[j] {
		case '\\':
			j++
		case '[':
			inCls = true
		case ']':
			inCls = false
		case '{':
			if !inCls {
				depth++
			}
		case '}':
			if !inCls {
				depth--
				if depth == 0 {
					return j, comma, true
				}
			}
		case ',':
			if !inCls && depth == 1 && comma < 0 {
				comma = j
			}
		}
	}
	return 0, -1, false
}

// splitTopCommas 按顶层逗号切分组内容（嵌套组与字符类内的逗号不算）。
func splitTopCommas(s string) []string {
	var alts []string
	depth, inCls, start := 0, false, 0
	for k := 0; k < len(s); k++ {
		switch s[k] {
		case '\\':
			k++
		case '[':
			inCls = true
		case ']':
			inCls = false
		case '{':
			if !inCls {
				depth++
			}
		case '}':
			if !inCls {
				depth--
			}
		case ',':
			if !inCls && depth == 0 {
				alts = append(alts, s[start:k])
				start = k + 1
			}
		}
	}
	return append(alts, s[start:])
}

// skipClass 从 p[i]=='[' 跳到字符类闭合 ']' 的下标（不配对时返回末下标，
// 外层循环随之结束）。紧随 [ 或 [! 的 ] 是字面量，不算闭合。
func skipClass(p string, i int) int {
	j := i + 1
	if j < len(p) && (p[j] == '!' || p[j] == '^') {
		j++
	}
	if j < len(p) && p[j] == ']' {
		j++
	}
	for j < len(p) && p[j] != ']' {
		if p[j] == '\\' {
			j++
		}
		j++
	}
	if j >= len(p) {
		return len(p) - 1
	}
	return j
}
