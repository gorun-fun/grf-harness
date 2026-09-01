// readpolicy_test.go 读边界（read_policy=workspace）测试：read/glob/grep/
// read_image 越界硬拒绝、界内放行、off 不限、与升级协议无关。
package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newReadLimitPolicy 构建开启读边界的策略。
func newReadLimitPolicy(t *testing.T, workspace string) *FsPolicy {
	t.Helper()
	return NewFsPolicy(workspace, true, true, &NoopHost{})
}

// TestCheckReadBoundary CheckRead 单元：越界拒、界内放行、off 不限。
func TestCheckReadBoundary(t *testing.T) {
	dir := t.TempDir()
	fs := newReadLimitPolicy(t, dir)

	if _, err := fs.CheckRead(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatalf("in-workspace read should pass: %v", err)
	}
	if _, err := fs.CheckRead("sub/a.txt"); err != nil {
		t.Fatalf("relative in-workspace read should pass: %v", err)
	}
	outside := filepath.Join(dir, "..", "elsewhere.txt")
	_, err := fs.CheckRead(outside)
	if err == nil || !strings.Contains(err.Error(), "[read-policy:") {
		t.Fatalf("out-of-workspace read should be denied, got: %v", err)
	}

	// off 模式不限读
	off := NewFsPolicy(dir, true, false, &NoopHost{})
	if _, err := off.CheckRead(outside); err != nil {
		t.Fatalf("read_policy=off should not limit: %v", err)
	}
}

// TestReadToolBoundary read 工具越界拒绝（且不泄漏存在性）、界内放行。
func TestReadToolBoundary(t *testing.T) {
	dir := t.TempDir()
	fs := newReadLimitPolicy(t, dir)
	r := NewRegistry()
	art, _ := NewArtifacts()
	RegisterFs(r, dir, fs, art)

	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0o644)
	os.WriteFile("/tmp/grf-readpolicy-outside.txt", []byte("secret"), 0o644)
	defer os.Remove("/tmp/grf-readpolicy-outside.txt")

	res := r.Execute(context.Background(), "read", mustJSON(t, map[string]any{"file_path": "a.txt"}))
	if !strings.Contains(res.Text, "hi") {
		t.Fatalf("in-workspace read failed: %s", res.Text)
	}
	res = r.Execute(context.Background(), "read", mustJSON(t, map[string]any{"file_path": "/tmp/grf-readpolicy-outside.txt"}))
	if !strings.Contains(res.Text, "[read-policy:") {
		t.Fatalf("out-of-workspace read should be denied, got: %s", res.Text)
	}
}

// TestGlobGrepBoundary glob/grep 显式 path 越界拒绝；默认根不受影响。
func TestGlobGrepBoundary(t *testing.T) {
	dir := t.TempDir()
	fs := newReadLimitPolicy(t, dir)
	r := NewRegistry()
	art, _ := NewArtifacts()
	RegisterFs(r, dir, fs, art)

	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("needle"), 0o644)

	// 默认根（工作区）正常
	res := r.Execute(context.Background(), "glob", mustJSON(t, map[string]any{"pattern": "*.txt"}))
	if !strings.Contains(res.Text, "a.txt") {
		t.Fatalf("default-root glob failed: %s", res.Text)
	}
	res = r.Execute(context.Background(), "grep", mustJSON(t, map[string]any{"pattern": "needle"}))
	if !strings.Contains(res.Text, "a.txt") {
		t.Fatalf("default-root grep failed: %s", res.Text)
	}
	// 显式越界根拒绝
	for _, c := range []struct{ tool, pattern string }{{"glob", "*"}, {"grep", "."}} {
		res = r.Execute(context.Background(), c.tool, mustJSON(t, map[string]any{"pattern": c.pattern, "path": "/tmp"}))
		if !strings.Contains(res.Text, "[read-policy:") {
			t.Fatalf("%s with out-of-workspace path should be denied, got: %s", c.tool, res.Text)
		}
	}
}

// TestReadImageBoundary read_image 越界拒绝。
func TestReadImageBoundary(t *testing.T) {
	dir := t.TempDir()
	fs := newReadLimitPolicy(t, dir)
	r := NewRegistry()
	RegisterReadImage(r, fs)

	res := r.Execute(context.Background(), "read_image", mustJSON(t, map[string]any{"file_path": "/etc/hosts"}))
	if !strings.Contains(res.Text, "[read-policy:") {
		t.Fatalf("out-of-workspace read_image should be denied, got: %s", res.Text)
	}

	// nil 策略（未接线）不限
	r2 := NewRegistry()
	RegisterReadImage(r2, nil)
	res = r2.Execute(context.Background(), "read_image", mustJSON(t, map[string]any{"file_path": "/etc/hosts"}))
	if strings.Contains(res.Text, "[read-policy:") {
		t.Fatalf("nil policy should not limit, got: %s", res.Text)
	}
}

// TestReadDenialNotEscalatable 读拒绝不走升级协议：denied 表无 read 键，
// 带 sandbox_permissions 的读重试同样被拒。
func TestReadDenialNotEscalatable(t *testing.T) {
	dir := t.TempDir()
	fs := newReadLimitPolicy(t, dir)

	fs.mu.Lock()
	n := len(fs.denied)
	fs.mu.Unlock()

	outside := filepath.Join(dir, "..", "secret.txt")
	_, err := fs.CheckRead(outside)
	if err == nil {
		t.Fatal("expected denial")
	}
	fs.mu.Lock()
	n2 := len(fs.denied)
	fs.mu.Unlock()
	if n2 != n {
		t.Fatalf("read denial must not enter the escalation table: %d -> %d", n, n2)
	}

	// 升级入口对 read 不可达
	if _, err := fs.Escalate("read", outside, "read-policy", "need it"); err == nil {
		t.Fatal("escalation for reads should fail")
	}
}

// TestSymlinkTraversalBlocked 符号链接穿越防护：工作区内指向外部的链接，
// 读边界与写边界都按展开后的真实路径判定为越界。
func TestSymlinkTraversalBlocked(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir() // 模拟其他用户/系统目录
	secret := filepath.Join(outside, "secret.txt")
	os.WriteFile(secret, []byte("secret"), 0o644)
	if err := os.Symlink(outside, filepath.Join(ws, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	// 读边界：经链接读外部文件必须拒绝
	fs := newReadLimitPolicy(t, ws)
	if _, err := fs.CheckRead(filepath.Join(ws, "link", "secret.txt")); err == nil {
		t.Fatal("read through symlink escaping the workspace should be denied")
	}
	// 写边界（无条件生效）：经链接写外部路径必须拒绝
	if _, err := fs.CheckWrite(filepath.Join(ws, "link", "evil.txt"), false); err == nil {
		t.Fatal("write through symlink escaping the workspace should be denied")
	}
	// 不开读边界的策略同样拦写穿越
	fs2 := NewFsPolicy(ws, false, false, &NoopHost{})
	if _, err := fs2.CheckWrite(filepath.Join(ws, "link", "evil.txt"), false); err == nil {
		t.Fatal("write traversal should be denied regardless of read policy")
	}
	// 链接指向工作区内部不算穿越
	if err := os.Symlink(filepath.Join(ws, "sub"), filepath.Join(ws, "inlink")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	os.MkdirAll(filepath.Join(ws, "sub"), 0o755)
	if _, err := fs.CheckRead(filepath.Join(ws, "inlink", "a.txt")); err != nil {
		t.Fatalf("internal symlink should pass: %v", err)
	}
}

// TestEditEscalationAfterDenial edit 升级键修复：CheckWrite 记 "write:" 前缀，
// Escalate("edit") 能匹配到记录，宿主批准后放行。
func TestEditEscalationAfterDenial(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "t.txt")
	os.WriteFile(target, []byte("hello world"), 0o644)

	fs := NewFsPolicy(ws, false, false, &NoopHost{AutoApprove: true})
	r := NewRegistry()
	art, _ := NewArtifacts()
	RegisterFs(r, ws, fs, art)

	// 越权即问：第一次越权 edit 即挂起问宿主（AutoApprove 批准）→ 放行并完成编辑
	res := r.Execute(context.Background(), "edit", mustJSON(t, map[string]any{"file_path": target, "old_string": "world", "new_string": "世界"}))
	if !strings.Contains(res.Text, "edited") {
		t.Fatalf("auto-approved edit should succeed, got: %s", res.Text)
	}
	data, _ := os.ReadFile(target)
	if string(data) != "hello 世界" {
		t.Fatalf("content: %q", data)
	}
}
