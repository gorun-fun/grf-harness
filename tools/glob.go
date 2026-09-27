// glob.go 文件发现（glob）：mtime 序（新→旧），先 100 条内联；更多存
// 文件报告路径。无 "/" 的模式匹配任意深度的 basename；含 "/" 时按相对
// 路径分段匹配，支持 ** 跨目录层级；排除 VCS 元数据目录。
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
func GlobFiles(pattern, base string) ([]string, error) {
	type ent struct {
		rel string
		mod time.Time
	}
	var found []ent
	walkFiles(base, func(_, rel string, info os.FileInfo) bool {
		if matchGlob(pattern, rel) {
			found = append(found, ent{rel: rel, mod: info.ModTime()})
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
