// grep.go 内容搜索：正则搜文件内容，行号分组输出，先 250 条内联；
// 更多存文件报告路径。include 支持简单 glob（*.ts、*.{js,jsx}）。
package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// grepArgs grep 工具参数（具名类型供 spec 交叉校验测试反射 json tag）。
type grepArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
	Include string `json:"include"`
}

func registerGrep(r *Registry, workspace string, fs *FsPolicy, artifacts *Artifacts) {
	r.Register("grep", func(ctx context.Context, args string) (Result, error) {
		var a grepArgs
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
		// 单趟收集全部匹配（匹配行很小，内存无虞），超 250 条时
		// 内存中截断展示 + 全量落盘，避免超限时重扫一遍文件树
		matches, err := GrepFiles(re, base, includeRe, -1)
		if err != nil {
			return Result{}, err
		}
		if len(matches) == 0 {
			return TextResult("no matches"), nil
		}
		if len(matches) <= 250 {
			return TextResult(strings.Join(matches, "\n")), nil
		}
		path, err := artifacts.Save("grep", strings.Join(matches, "\n")+"\n")
		if err != nil {
			return Result{}, err
		}
		return TextResult(fmt.Sprintf("%s\n\n(result capped at 250 matches; the complete match list was saved to %s)",
			strings.Join(matches[:250], "\n"), path)), nil
	})
}

// GrepFiles 逐文件搜正则：输出 "文件" 标题 + "行号:内容" 行。cap>0 时
// 最多返回 cap 条（文件标题不计入）。遍历骨架与 glob 共用（walkFiles），
// 这里贡献 include 过滤、8MB/非常规文件与二进制跳过。
func GrepFiles(re *regexp.Regexp, base string, include *regexp.Regexp, capLimit int) ([]string, error) {
	var out []string
	count := 0
	walkFiles(base, func(p, rel string, info os.FileInfo) bool {
		if include != nil && !include.MatchString(filepath.Base(p)) {
			return true
		}
		if capLimit > 0 && count >= capLimit {
			return false // 截断：结束整趟遍历
		}
		if !info.Mode().IsRegular() || info.Size() > 8<<20 {
			return true
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return true
		}
		// 跳过二进制文件：开头 1MB 内含 NUL 即视为二进制
		if bytes.IndexByte(data[:min(len(data), 1<<20)], 0) >= 0 {
			return true
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
		return true
	})
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
