// grep.go 内容搜索：正则搜文件内容，行号分组输出，先 250 条内联；
// 更多存文件报告路径。include 支持简单 glob（*.ts、*.{js,jsx}）。
package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func registerGrep(r *Registry, workspace string, fs *FsPolicy, artifacts *Artifacts) {
	r.Register("grep", func(ctx context.Context, args string) (Result, error) {
		var a struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
			Include string `json:"include"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return Result{}, err
		}
		if a.Pattern == "" {
			return Result{}, fmt.Errorf("pattern is required")
		}
		re, err := regexp.Compile(a.Pattern)
		if err != nil {
			return Result{}, fmt.Errorf("invalid regex: %v", err)
		}
		base := workspace
		if a.Path != "" {
			// 显式指定搜索根时做读边界校验（默认根就是工作区，无须校验）
			if _, err := fs.CheckRead(a.Path); err != nil {
				return Result{}, err
			}
			base = a.Path
		}
		var includeRe *regexp.Regexp
		if a.Include != "" {
			inc := globToRegex(a.Include)
			includeRe, err = regexp.Compile(inc)
			if err != nil {
				return Result{}, fmt.Errorf("invalid include glob: %v", err)
			}
		}
		matches, err := GrepFiles(re, base, includeRe, 251)
		if err != nil {
			return Result{}, err
		}
		if len(matches) == 0 {
			return TextResult("no matches"), nil
		}
		if len(matches) <= 250 {
			return TextResult(strings.Join(matches, "\n")), nil
		}
		all, _ := GrepFiles(re, base, includeRe, -1)
		path, err := artifacts.Save("grep", strings.Join(all, "\n")+"\n")
		if err != nil {
			return Result{}, err
		}
		return TextResult(fmt.Sprintf("%s\n\n(result capped at 250 matches; the complete match list was saved to %s)",
			strings.Join(matches[:250], "\n"), path)), nil
	})
}

// GrepFiles 逐文件搜正则：输出 "文件" 标题 + "行号:内容" 行。cap>0 时
// 最多返回 cap 条（文件标题不计入）。
func GrepFiles(re *regexp.Regexp, base string, include *regexp.Regexp, capLimit int) ([]string, error) {
	baseAbs := absOr(base)
	var out []string
	count := 0
	err := filepath.WalkDir(baseAbs, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if p != baseAbs && (name == ".git" || name == ".hg" || name == ".svn") {
				return filepath.SkipDir
			}
			return nil
		}
		if include != nil && !include.MatchString(d.Name()) {
			return nil
		}
		if capLimit > 0 && count >= capLimit {
			return filepath.SkipAll
		}
		info, ierr := d.Info()
		if ierr != nil || !info.Mode().IsRegular() || info.Size() > 8<<20 {
			return nil
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		// 跳过二进制文件
		for i, b := range data {
			if b == 0 {
				_ = i
				return nil
			}
			if i > 1<<20 {
				break
			}
		}
		rel, rerr := filepath.Rel(baseAbs, p)
		if rerr != nil {
			return nil
		}
		var fileLines []string
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				fileLines = append(fileLines, fmt.Sprintf("%d:%s", i+1, strings.TrimRight(line, "\r")))
				count++
				if capLimit > 0 && count >= capLimit {
					break
				}
			}
		}
		if len(fileLines) > 0 {
			out = append(out, rel)
			out = append(out, fileLines...)
		}
		return nil
	})
	_ = err // SkipAll 截断或个别不可读项均按正常结束处理
	return out, nil
}

// globToRegex 把 include glob（如 *.ts、*.{js,jsx}）转为正则。
func globToRegex(g string) string {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(g); i++ {
		c := g[i]
		switch c {
		case '*':
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		case '{':
			// 简单 brace 展开 {a,b,c}
			j := strings.IndexByte(g[i:], '}')
			if j > 0 {
				alts := strings.Split(g[i+1:i+j], ",")
				for k := range alts {
					alts[k] = regexp.QuoteMeta(alts[k])
				}
				b.WriteString("(?:" + strings.Join(alts, "|") + ")")
				i += j
			} else {
				b.WriteString(regexp.QuoteMeta(string(c)))
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return b.String()
}
