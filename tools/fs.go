// fs.go 文件工具：read / write / edit / glob / grep。
//
//   - read: 行号输出，offset/limit 分页，默认 2000 行，单行超 2000 字符截断
//   - write: 创建/整体替换；默认 fs-observation-policy 要求先 read 过
//   - edit: 字面替换，默认唯一匹配，replace_all 全部替换
//   - glob: mtime 序（新→旧），先 100 条内联；更多存文件报告路径；
//     无 "/" 的模式匹配任意深度的 basename
//   - grep: 正则搜内容，行号分组输出，先 250 条内联；更多存文件
//
// 写入默认限制在工作区内；越界写入返回沙箱拒绝标记，可用 sandbox_permissions
// + justification 按"一次性重试"协议升级（经宿主审批）。
package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// FsPolicy 文件策略：观察策略（写前须读）、工作区写边界与升级审批。
type FsPolicy struct {
	mu        sync.Mutex
	Observe   bool // 写/改已有文件前要求本会话内 read 过
	Workspace string
	Host      Host

	read   map[string]string // abs 路径 -> 内容摘要（写前校验读过）
	denied map[string]string // 升级键(tool:abs) -> 被拒模式，供一次性重试校验
}

func NewFsPolicy(workspace string, observe bool, host Host) *FsPolicy {
	return &FsPolicy{
		Observe:   observe,
		Workspace: absOr(workspace),
		Host:      host,
		read:      map[string]string{},
		denied:    map[string]string{},
	}
}

func absOr(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return a
}

// resolve 把路径解析为绝对路径：相对路径相对工作区（filesystem backend
// 语义），绝对路径原样（符号链接不展开，交给边界判断）。
func (f *FsPolicy) resolve(p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(f.Workspace, p)
	}
	return filepath.Clean(p)
}

// MarkRead 记录一次成功的文件读取。
func (f *FsPolicy) MarkRead(abs string) {
	f.mu.Lock()
	f.read[abs] = "ok"
	f.mu.Unlock()
}

// MarkWritten 记录一次写入（新写入后再次 edit 不需要重新 read）。
func (f *FsPolicy) MarkWritten(abs string) {
	f.mu.Lock()
	f.read[abs] = "ok"
	f.mu.Unlock()
}

// InWorkspace 路径是否在工作区内。
func (f *FsPolicy) InWorkspace(p string) bool {
	abs := absOr(p)
	return abs == f.Workspace || strings.HasPrefix(abs, f.Workspace+string(filepath.Separator))
}

// CheckWrite 写前校验：工作区边界 + 观察策略（覆写已存在文件前要求本会话
// read 过；新建文件不受限）。返回 (abs, error)。
func (f *FsPolicy) CheckWrite(path string, readBeforeEdit bool) (string, error) {
	abs := f.resolve(path)
	f.mu.Lock()
	_, hasRead := f.read[abs]
	f.mu.Unlock()

	if !f.InWorkspace(abs) {
		f.mu.Lock()
		f.denied["write:"+abs] = "workspace-write"
		f.mu.Unlock()
		return abs, fmt.Errorf("[sandbox: file access denied under workspace-write mode] path %q is outside the workspace %q; to write it, retry the exact same call once with sandbox_permissions plus a one-sentence justification", path, f.Workspace)
	}
	if f.Observe && readBeforeEdit && !hasRead {
		if _, err := os.Stat(abs); err == nil {
			return abs, fmt.Errorf("refusing to modify %q: read it first with the read tool (the default fs-observation-policy requires it). If you just created or edited it in this session, that already counts", path)
		}
	}
	return abs, nil
}

// Escalate 校验并执行一次性升级：tool 为 "write"/"edit"/"bash"，key 为
// 被拒资源（路径或命令）。无此前拒绝记录时按投机升级拒绝。
func (f *FsPolicy) Escalate(tool, key, mode, justification string) (bool, error) {
	if strings.TrimSpace(justification) == "" {
		return false, fmt.Errorf("sandbox_permissions requires a one-sentence justification")
	}
	f.mu.Lock()
	deniedMode, ok := f.denied[tool+":"+key]
	f.mu.Unlock()
	if !ok {
		return false, fmt.Errorf("sandbox_permissions is only valid as a one-shot retry of an operation the sandbox just denied; there is no recorded denial for this exact %s call — run it normally first and read the denial marker", tool)
	}
	if f.Host == nil {
		return false, fmt.Errorf("no host available to approve the escalation")
	}
	if !f.Host.ApproveEscalation(contextBG(), tool, key, mode, justification) {
		return false, fmt.Errorf("the user rejected the escalation for this %s call — stop and explain, never work around it (attempting or escalating other calls later is still allowed)", tool)
	}
	// 一次性：消费掉拒绝记录
	f.mu.Lock()
	delete(f.denied, tool+":"+key)
	f.mu.Unlock()
	_ = deniedMode
	return true, nil
}

// RegisterFs 注册 read/write/edit/glob/grep 五个工具。
func RegisterFs(r *Registry, workspace string, fs *FsPolicy, artifacts *Artifacts) {
	registerRead(r, fs)
	registerWrite(r, fs)
	registerEdit(r, fs)
	registerGlob(r, workspace, artifacts)
	registerGrep(r, workspace, artifacts)
}

// ---- read ----

func registerRead(r *Registry, fs *FsPolicy) {
	r.Register("read", func(ctx context.Context, args string) (Result, error) {
		var a struct {
			FilePath string `json:"file_path"`
			Offset   int    `json:"offset"`
			Limit    int    `json:"limit"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return Result{}, err
		}
		if a.FilePath == "" {
			return Result{}, fmt.Errorf("file_path is required")
		}
		if a.Offset < 1 {
			a.Offset = 1
		}
		if a.Limit <= 0 {
			a.Limit = 2000
		}
		if a.Limit > 5000 {
			a.Limit = 5000
		}
		// stat 解析后的路径：相对路径相对工作区（与下方 ReadFile 一致），
		// 直接 stat 原始路径会相对进程 CWD，工作区文件被误判不存在
		absPath := fs.resolve(a.FilePath)
		info, err := os.Stat(absPath)
		if err != nil {
			return Result{}, err
		}
		if info.IsDir() {
			return Result{}, fmt.Errorf("%q is a directory; use glob to discover files", a.FilePath)
		}
		data, err := os.ReadFile(absPath)
		if err != nil {
			return Result{}, err
		}
		lines := strings.Split(string(data), "\n")
		total := len(lines)
		if total > 0 && lines[total-1] == "" {
			total-- // 末尾换行不算一行
		}
		start := a.Offset - 1
		if start >= total {
			return TextResult(fmt.Sprintf("(offset %d beyond end of file, %d lines total)", a.Offset, total)), nil
		}
		end := start + a.Limit
		truncated := false
		if end > total {
			end = total
		} else if end < total {
			truncated = true
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s (%d lines total, showing %d-%d):\n", a.FilePath, total, start+1, end)
		for i := start; i < end; i++ {
			line := lines[i]
			if len([]rune(line)) > 2000 {
				r := []rune(line)
				line = string(r[:2000]) + "…"
			}
			fmt.Fprintf(&b, "%d\t%s\n", i+1, line)
		}
		if truncated {
			fmt.Fprintf(&b, "(%d more lines below; continue with offset=%d)", total-end, end+1)
		}
		fs.MarkRead(absPath)
		return TextResult(b.String()), nil
	})
}

// ---- write ----

func registerWrite(r *Registry, fs *FsPolicy) {
	r.Register("write", func(ctx context.Context, args string) (Result, error) {
		var a struct {
			FilePath           string `json:"file_path"`
			Content            string `json:"content"`
			SandboxPermissions string `json:"sandbox_permissions"`
			Justification      string `json:"justification"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return Result{}, err
		}
		if a.FilePath == "" {
			return Result{}, fmt.Errorf("file_path is required")
		}
		abs := fs.resolve(a.FilePath)
		if a.SandboxPermissions != "" {
			ok, err := fs.Escalate("write", abs, a.SandboxPermissions, a.Justification)
			if err != nil || !ok {
				if err != nil {
					return Result{}, err
				}
				return Result{}, fmt.Errorf("escalation did not grant access")
			}
			// 升级通过：跳过边界与观察策略
		} else {
			_, err := fs.CheckWrite(a.FilePath, true)
			if err != nil {
				return Result{}, err
			}
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return Result{}, err
		}
		if err := os.WriteFile(abs, []byte(a.Content), 0o644); err != nil {
			return Result{}, err
		}
		fs.MarkWritten(abs)
		return TextResult(fmt.Sprintf("wrote %d bytes to %s", len(a.Content), a.FilePath)), nil
	})
}

// ---- edit ----

func registerEdit(r *Registry, fs *FsPolicy) {
	r.Register("edit", func(ctx context.Context, args string) (Result, error) {
		var a struct {
			FilePath           string `json:"file_path"`
			OldString          string `json:"old_string"`
			NewString          string `json:"new_string"`
			ReplaceAll         bool   `json:"replace_all"`
			SandboxPermissions string `json:"sandbox_permissions"`
			Justification      string `json:"justification"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return Result{}, err
		}
		if a.FilePath == "" {
			return Result{}, fmt.Errorf("file_path is required")
		}
		if a.OldString == "" {
			return Result{}, fmt.Errorf("old_string is required (use new_string=\"\" deletion with a non-empty old_string)")
		}
		abs := fs.resolve(a.FilePath)
		if a.SandboxPermissions != "" {
			ok, err := fs.Escalate("edit", abs, a.SandboxPermissions, a.Justification)
			if err != nil || !ok {
				if err != nil {
					return Result{}, err
				}
				return Result{}, fmt.Errorf("escalation did not grant access")
			}
		} else {
			if _, err := fs.CheckWrite(a.FilePath, true); err != nil {
				return Result{}, err
			}
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return Result{}, err
		}
		text := string(data)
		n := strings.Count(text, a.OldString)
		switch {
		case n == 0:
			return Result{}, fmt.Errorf("old_string not found in %s", a.FilePath)
		case n > 1 && !a.ReplaceAll:
			return Result{}, fmt.Errorf("old_string appears %d times in %s; provide a more specific old_string or set replace_all=true", n, a.FilePath)
		}
		updated := strings.Replace(text, a.OldString, a.NewString, 1)
		if a.ReplaceAll {
			updated = strings.ReplaceAll(text, a.OldString, a.NewString)
		}
		if err := os.WriteFile(abs, []byte(updated), 0o644); err != nil {
			return Result{}, err
		}
		fs.MarkWritten(abs)
		return TextResult(fmt.Sprintf("edited %s (%d replacement%s)", a.FilePath, n, plural(n))), nil
	})
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// ---- glob ----

func registerGlob(r *Registry, workspace string, artifacts *Artifacts) {
	r.Register("glob", func(ctx context.Context, args string) (Result, error) {
		var a struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return Result{}, err
		}
		if a.Pattern == "" {
			return Result{}, fmt.Errorf("pattern is required")
		}
		base := workspace
		if a.Path != "" {
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

// GlobFiles 实现 glob 语义：返回按修改时间新→旧排序的匹配文件（仅文件）。
// 模式无 "/" 时对任意深度的 basename 匹配；含 "/" 时按相对路径匹配，
// 支持 ** 跨目录层级。排除 VCS 元数据目录。
func GlobFiles(pattern, base string) ([]string, error) {
	baseAbs := absOr(base)
	type ent struct {
		rel string
		mod time.Time
		abs string
	}
	var found []ent
	err := filepath.WalkDir(baseAbs, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 不可读的目录直接跳过
		}
		if d.IsDir() {
			name := d.Name()
			if path != baseAbs && (name == ".git" || name == ".hg" || name == ".svn") {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(baseAbs, path)
		if rerr != nil {
			return nil
		}
		if !matchGlob(pattern, rel) {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		found = append(found, ent{rel: rel, mod: info.ModTime(), abs: path})
		return nil
	})
	if err != nil {
		return nil, err
	}
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

// ---- grep ----

func registerGrep(r *Registry, workspace string, artifacts *Artifacts) {
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

// contextBG Escalate 审批回调用的 context（审批属 UI 交互，不随工具调用
// 取消——保留 ctx 参数由宿主自行决定）。
func contextBG() context.Context { return context.Background() }
