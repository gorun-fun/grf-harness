package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"git.gorun.fun/share/grf-harness/llm"
)

func newTestPolicy(t *testing.T, workspace string) *FsPolicy {
	t.Helper()
	return NewFsPolicy(workspace, true, false, &NoopHost{})
}

func TestSpecCoversAllTools(t *testing.T) {
	names := SpecNames()
	if len(names) != 28 {
		t.Fatalf("spec should define 28 tools, got %d: %v", len(names), names)
	}
	for _, n := range names {
		if _, ok := SpecDef(n); !ok {
			t.Fatalf("SpecDef missing %s", n)
		}
	}
}

func TestReadWriteEdit(t *testing.T) {
	dir := t.TempDir()
	fs := newTestPolicy(t, dir)
	r := NewRegistry()
	art, _ := NewArtifacts()
	RegisterFs(r, dir, fs, art)

	// write 新文件
	res := r.Execute(context.Background(), "write", mustJSON(t, map[string]any{"file_path": "a.txt", "content": "hello\nworld"}))
	if !strings.Contains(res.Text, "wrote") {
		t.Fatal(res.Text)
	}
	// read 带行号
	res = r.Execute(context.Background(), "read", mustJSON(t, map[string]any{"file_path": filepath.Join(dir, "a.txt")}))
	if !strings.Contains(res.Text, "1\thello") || !strings.Contains(res.Text, "2\tworld") {
		t.Fatal(res.Text)
	}
	// edit 唯一匹配
	res = r.Execute(context.Background(), "edit", mustJSON(t, map[string]any{"file_path": "a.txt", "old_string": "world", "new_string": "世界"}))
	if !strings.Contains(res.Text, "edited") {
		t.Fatal(res.Text)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(data) != "hello\n世界" {
		t.Fatalf("content: %q", data)
	}
	// 未读过的既有文件不可直接写（观察策略）
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("x"), 0o644)
	fs2 := NewFsPolicy(dir, true, false, &NoopHost{})
	r2 := NewRegistry()
	RegisterFs(r2, dir, fs2, art)
	res = r2.Execute(context.Background(), "write", mustJSON(t, map[string]any{"file_path": "b.txt", "content": "y"}))
	if !strings.Contains(res.Text, "read it first") {
		t.Fatalf("observation policy should block, got: %s", res.Text)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestGlobOrderAndBasename(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "old.go"), []byte("1"), 0o644)
	os.WriteFile(filepath.Join(dir, "sub", "new.go"), []byte("1"), 0o644)
	os.WriteFile(filepath.Join(dir, "sub", "skip.txt"), []byte("1"), 0o644)
	// 部分 linux 文件系统 mtime 粒度粗（同刻写入分不出先后），显式回拨
	// old.go 保证排序确定，勿依赖写入顺序
	os.Chtimes(filepath.Join(dir, "old.go"), time.Now().Add(-time.Hour), time.Now().Add(-time.Hour))
	// 无 "/" 模式匹配任意深度 basename
	m, err := GlobFiles("*.go", dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 2 {
		t.Fatalf("want 2, got %v", m)
	}
	// mtime 序（新→旧）：new.go 在前
	if !strings.HasSuffix(m[0], "new.go") {
		t.Fatalf("newest first expected, got %v", m)
	}
	// 带 / 的模式按路径匹配
	m, _ = GlobFiles("sub/*.go", dir)
	if len(m) != 1 || !strings.HasSuffix(m[0], "new.go") {
		t.Fatalf("path pattern: %v", m)
	}
}

func TestGlobBraceExpansion(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "docs", "deep"), 0o755)
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("1"), 0o644)
	os.WriteFile(filepath.Join(dir, "docs", "readme.md"), []byte("1"), 0o644)
	os.WriteFile(filepath.Join(dir, "docs", "deep", "note.txt"), []byte("1"), 0o644)
	os.WriteFile(filepath.Join(dir, "app.ts"), []byte("1"), 0o644)
	os.WriteFile(filepath.Join(dir, "lit{a,b}.go"), []byte("1"), 0o644)

	// 花括号交替：**/*.{html,md,txt} 命中三种扩展名（含深层）
	m, err := GlobFiles("**/*.{html,md,txt}", dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 3 {
		t.Fatalf("brace expansion want 3, got %v", m)
	}
	joined := strings.Join(m, "\n")
	for _, want := range []string{"index.html", "readme.md", "note.txt"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %s in %v", want, m)
		}
	}
	if strings.Contains(joined, "app.ts") || strings.Contains(joined, "lit{a,b}.go") {
		t.Fatalf("unexpected matches: %v", m)
	}
	// 花括号在段中间：{docs,other}/*.md
	m, _ = GlobFiles("{docs,other}/*.md", dir)
	if len(m) != 1 || !strings.HasSuffix(m[0], "readme.md") {
		t.Fatalf("mid-segment brace: %v", m)
	}
}

func TestExpandBraces(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"**/*.{html,md,txt}", []string{"**/*.html", "**/*.md", "**/*.txt"}},
		{"{a,b}/x.ts", []string{"a/x.ts", "b/x.ts"}},
		// 嵌套：外层无顶层逗号时展开内层，迭代展开到底
		{"a{b{1,2}}", []string{"ab1", "ab2"}},
		{"a{b,c{1,2}}", []string{"ab", "ac1", "ac2"}},
		// 无逗号组展开为组内容（同 bash/grep）；不配对 / 字符类 / 转义保持字面量
		{"{ab}", []string{"ab"}},
		{"a{b}", []string{"ab"}},
		{"{a,b", []string{"{a,b"}},
		{"[{a,b}]", []string{"[{a,b}]"}},
		{`\{a,b\}`, []string{`\{a,b\}`}},
		{"plain/*.ts", []string{"plain/*.ts"}},
		// 空替代与多组组合
		{"x{a,,c}", []string{"xa", "x", "xc"}},
		{"{a,b}{1,2}", []string{"a1", "a2", "b1", "b2"}},
	}
	for _, c := range cases {
		got := expandBraces(c.in)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("expandBraces(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	// 病态展开防爆炸：组合数超上限回退为字面量原样
	big := strings.Repeat("{1,2}", 9) // 2^9=512 > 128
	if got := expandBraces(big); len(got) != 1 || got[0] != big {
		t.Errorf("pathological expansion should fall back to literal, got %v", got)
	}
}

func TestGrepCapAndInclude(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("needle\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "a.ts"), []byte("keep\nneedle here\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("needle\n"), 0o644)
	re := regexp.MustCompile("needle")
	m, err := GrepFiles(re, dir, regexp.MustCompile(`\.ts$`), 250)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(m, "\n")
	if !strings.Contains(joined, "a.ts") || strings.Contains(joined, "b.txt") || strings.Contains(joined, ".git") {
		t.Fatalf("grep result wrong: %v", m)
	}
	// cap 生效
	m, _ = GrepFiles(re, dir, nil, 1)
	count := 0
	for _, ln := range m {
		if !strings.HasSuffix(ln, "ts") && !strings.HasSuffix(ln, "txt") && !strings.HasSuffix(ln, "config") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("cap should limit matches to 1, got %d: %v", count, m)
	}
}

func TestJobManager(t *testing.T) {
	m := NewJobManager()
	id := m.StartStream("stream", "test", func(ctx context.Context, emit func(string)) error {
		emit("part1\n")
		emit("part2\n")
		return nil
	})
	out, err := m.Output(context.Background(), id, true, 2e9)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "part1") || !strings.Contains(out, "[status: done]") {
		t.Fatalf("output: %s", out)
	}
	// 第二次读：增量（无新内容）
	out2, _ := m.Output(context.Background(), id, false, 0)
	if strings.Contains(out2, "part1") || !strings.Contains(out2, "[status: done]") {
		t.Fatalf("second read should be empty delta: %q", out2)
	}
	if m.ActiveCount() != 0 {
		t.Fatal("job should be done")
	}
}

// TestJobOutputCtxCancel 回合取消必须能打断阻塞中的 job_output 等待：
// 旧实现无视 ctx，cancel 端点等不到终态只能谎报已取消，会话进入
// 僵尸运行态（running 恒真），后续请求全部 409。
func TestJobOutputCtxCancel(t *testing.T) {
	m := NewJobManager()
	started := make(chan struct{})
	id := m.StartStream("stream", "hang", func(ctx context.Context, emit func(string)) error {
		close(started)
		<-ctx.Done() // 模拟长作业：直到被杀才结束
		return ctx.Err()
	})
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	done := make(chan error, 1)
	go func() {
		_, err := m.Output(ctx, id, true, time.Minute)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled ctx should abort the wait with an error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Output wait ignored ctx cancellation (zombie turn)")
	}
	// 收尾：杀掉作业，避免泄漏 goroutine
	_ = m.Kill(id, "test")
}

// TestJobKillCancelsExecutor job_kill 必须把取消传导给执行器 ctx：
// 旧实现传 context.Background()，Kill 后执行器永不结束（僵尸作业）。
func TestJobKillCancelsExecutor(t *testing.T) {
	m := NewJobManager()
	finished := make(chan struct{})
	id := m.StartStream("stream", "hang", func(ctx context.Context, emit func(string)) error {
		<-ctx.Done()
		close(finished)
		return ctx.Err()
	})
	if err := m.Kill(id, "test"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("Kill did not cancel the executor ctx (zombie job)")
	}
	out, err := m.Output(context.Background(), id, false, 0)
	if err != nil || !strings.Contains(out, "[status: killed]") {
		t.Fatalf("job should settle as killed, got %q err=%v", out, err)
	}
}

// TestJobKillStopsProcess 端到端：bash 后台作业被 job_kill 后进程必须
// 真实退出（执行 goroutine 落定），而不是只改状态、sleep 继续跑。
func TestJobKillStopsProcess(t *testing.T) {
	ws := t.TempDir()
	jobs := NewJobManager()
	r := NewRegistry()
	RegisterBash(r, BashDeps{Workspace: ws, Jobs: jobs})
	res := r.Execute(context.Background(), "bash", mustJSON(t, map[string]any{
		"command": "sleep 60", "run_in_background": true}))
	var id string
	for _, f := range strings.Fields(res.Text) {
		if strings.HasPrefix(f, "job_") {
			id = f
			break
		}
	}
	if id == "" {
		t.Fatalf("no job id in %q", res.Text)
	}
	j := jobs.lookup(id)
	if err := jobs.Kill(id, "test"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		j.mu.Lock()
		settled := j.status != JobRunning && j.err != nil
		j.mu.Unlock()
		if settled {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("background process still running 10s after job_kill (cancel chain broken)")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestWebSearchParse(t *testing.T) {
	if testing.Short() {
		t.Skip("network")
	}
	// 仅测 HTML 解析辅助
	u := cleanDDGURL("//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fa&rut=abc")
	if u != "https://example.com/a" {
		t.Fatalf("cleanDDGURL: %q", u)
	}
	if s := htmlText("<b>Hello</b> &amp; &quot;world&quot;"); s != `Hello & "world"` {
		t.Fatalf("htmlText: %q", s)
	}
}

func noopHandler(ctx context.Context, args string) (Result, error) { return Result{}, nil }

func TestRegisterCustom(t *testing.T) {
	r := NewRegistry()
	r.RegisterCustom(llm.Tool{
		Name:        "export_report",
		Description: "导出报表",
		Parameters:  map[string]any{"type": "object"},
	}, func(ctx context.Context, args string) (Result, error) {
		return TextResult("done:" + args), nil
	})
	if !r.Has("export_report") {
		t.Fatal("custom tool not registered")
	}
	names := r.Registered()
	if len(names) != 1 || names[0] != "export_report" {
		t.Fatalf("Registered: %v", names)
	}
	defs := r.Defs()
	if len(defs) != 1 || defs[0].Name != "export_report" || defs[0].Description != "导出报表" {
		t.Fatalf("Defs: %+v", defs)
	}
	res := r.Execute(context.Background(), "export_report", `{"x":1}`)
	if res.Text != `done:{"x":1}` {
		t.Fatalf("Execute: %q", res.Text)
	}
	// 禁用后不再下发、不可执行
	r.Disable("export_report")
	if r.Has("export_report") || len(r.Defs()) != 0 {
		t.Fatalf("disable custom failed: %v", r.Registered())
	}
	if res := r.Execute(context.Background(), "export_report", "{}"); !strings.Contains(res.Text, "unknown tool") {
		t.Fatalf("disabled custom should be unknown: %q", res.Text)
	}
}

func TestRegisterCustomGuards(t *testing.T) {
	r := NewRegistry()
	cases := []struct {
		name string
		def  llm.Tool
	}{
		{"empty name", llm.Tool{Description: "x"}},
		{"spec conflict", llm.Tool{Name: "bash", Description: "x"}},
	}
	for _, c := range cases {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("%s: expected panic", c.name)
				}
			}()
			r.RegisterCustom(c.def, noopHandler)
		}()
	}
	// spec 内名字仍走 Register（且不受 custom 影响）
	r.Register("bash", noopHandler)
	if !r.Has("bash") {
		t.Fatal("spec Register broken")
	}
	if len(r.Defs()) != 1 || r.Defs()[0].Name != "bash" {
		t.Fatalf("Defs after spec Register: %+v", r.Defs())
	}
}

func TestExecuteSetsErr(t *testing.T) {
	r := NewRegistry()
	r.RegisterCustom(llm.Tool{Name: "boom", Description: "总是失败",
		Parameters: map[string]any{"type": "object"}}, func(ctx context.Context, args string) (Result, error) {
		return Result{}, fmt.Errorf("炸了")
	})
	r.RegisterCustom(llm.Tool{Name: "panic_tool", Description: "总是 panic",
		Parameters: map[string]any{"type": "object"}}, func(ctx context.Context, args string) (Result, error) {
		panic("bad state")
	})

	// handler 返回 error
	res := r.Execute(context.Background(), "boom", "{}")
	if res.Err == "" || !strings.Contains(res.Text, "error: 炸了") {
		t.Fatalf("error 路径应同时填 Err 与完整反馈文本: %+v", res)
	}
	// handler panic
	res = r.Execute(context.Background(), "panic_tool", "{}")
	if res.Err == "" || !strings.Contains(res.Text, "panicked") {
		t.Fatalf("panic 路径应填 Err: %+v", res)
	}
	// 未注册工具
	res = r.Execute(context.Background(), "not_registered", "{}")
	if res.Err == "" || !strings.Contains(res.Text, "unknown tool") {
		t.Fatalf("未注册路径应填 Err: %+v", res)
	}
	// 成功路径 Err 为空
	r.RegisterCustom(llm.Tool{Name: "fine", Description: "总是成功",
		Parameters: map[string]any{"type": "object"}}, func(ctx context.Context, args string) (Result, error) {
		return TextResult("ok"), nil
	})
	if res = r.Execute(context.Background(), "fine", "{}"); res.Err != "" || res.Text != "ok" {
		t.Fatalf("成功路径: %+v", res)
	}
}
