// fs.go 文件内容工具：read / write / edit。
//
//   - read: 行号输出，offset/limit 分页，默认 2000 行，单行超 2000 字符截断
//   - write: 创建/整体替换；默认 fs-observation-policy 要求先 read 过
//     （策略引擎见 fspolicy.go；文件发现类工具见 glob.go / grep.go）
//   - edit: 字面替换，默认唯一匹配，replace_all 全部替换
package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RegisterFs 注册 read/write/edit/glob/grep 五个工具。
func RegisterFs(r *Registry, workspace string, fs *FsPolicy, artifacts *Artifacts) {
	registerRead(r, fs)
	registerWrite(r, fs)
	registerEdit(r, fs)
	registerGlob(r, workspace, fs, artifacts)
	registerGrep(r, workspace, fs, artifacts)
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
		// 读边界校验（policy 的 read-workspace 拒绝工作区外路径），在
		// stat 之前——避免对越界路径泄漏"存在与否"
		absPath, err := fs.CheckRead(a.FilePath)
		if err != nil {
			return Result{}, err
		}
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
		} else if _, werr := fs.CheckWrite(a.FilePath, true); werr != nil {
			// 越权即问：写边界拒绝时挂起征求宿主批准（仅边界拒绝，
			// 观察策略的"先读后写"不问），批准则当次放行
			if !fs.AskAndEscalate("write", abs, "danger-full-access", "写入工作区外: "+a.FilePath) {
				return Result{}, werr
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
			if _, werr := fs.CheckWrite(a.FilePath, true); werr != nil {
				if !fs.AskAndEscalate("edit", abs, "danger-full-access", "修改工作区外: "+a.FilePath) {
					return Result{}, werr
				}
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
