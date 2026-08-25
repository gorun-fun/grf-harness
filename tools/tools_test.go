package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func newTestPolicy(t *testing.T, workspace string) *FsPolicy {
	t.Helper()
	return NewFsPolicy(workspace, true, &NoopHost{})
}

func TestSpecCovers25Tools(t *testing.T) {
	names := SpecNames()
	if len(names) != 25 {
		t.Fatalf("spec should define 25 tools, got %d: %v", len(names), names)
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
	fs2 := NewFsPolicy(dir, true, &NoopHost{})
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
	out, err := m.Output(id, true, 2e9)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "part1") || !strings.Contains(out, "[status: done]") {
		t.Fatalf("output: %s", out)
	}
	// 第二次读：增量（无新内容）
	out2, _ := m.Output(id, false, 0)
	if strings.Contains(out2, "part1") || !strings.Contains(out2, "[status: done]") {
		t.Fatalf("second read should be empty delta: %q", out2)
	}
	if m.ActiveCount() != 0 {
		t.Fatal("job should be done")
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
