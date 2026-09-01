// esc_test.go bash 一次性升级链路：被拒 → 带 sandbox_permissions 重试 →
// 必须调用 Host.ApproveEscalation，批准后放行。
package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type escHost struct {
	NoopHost
	called    int
	allow     bool
	grantDir  string // 非空 = 批准并授权该目录
}

func (h *escHost) ApproveEscalation(ctx context.Context, tool, key, mode, justification string) EscalationDecision {
	h.called++
	return EscalationDecision{Approved: h.allow, GrantDir: h.grantDir}
}

func TestBashEscalationCallsHost(t *testing.T) {
	if sandboxExecPath == "" {
		t.Skip("sandbox-exec unavailable")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	ws := filepath.Join(home, ".grf-esc-ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(ws)
	secret := filepath.Join(home, ".grf-esc-secret.txt")
	os.WriteFile(secret, []byte("secret"), 0o644)
	defer os.Remove(secret)

	host := &escHost{allow: true}
	r := NewRegistry()
	RegisterBash(r, BashDeps{Workspace: ws, Host: func() Host { return host },
		SandboxOn: true, ReadLimit: true, Jobs: NewJobManager()})
	cmd := "cat " + shellQ(secret)

	// 越权即问：第一次调用即被拒 → 挂起问宿主 → 批准 → 脱沙箱重跑成功
	res := r.Execute(context.Background(), "bash", mustJSON(t, map[string]any{"command": cmd, "description": "t"}))
	if host.called != 1 {
		t.Fatalf("ApproveEscalation should be called once, got %d (result: %s)", host.called, res.Text)
	}
	if !strings.Contains(res.Text, "secret") {
		t.Fatalf("approved rerun should succeed, got: %s", res.Text)
	}

	// 拒绝场景：宿主不同意 → 终局拒绝，重试也不再询问
	rhost := &escHost{allow: false}
	r2 := NewRegistry()
	RegisterBash(r2, BashDeps{Workspace: ws, Host: func() Host { return rhost },
		SandboxOn: true, ReadLimit: true, Jobs: NewJobManager()})
	res = r2.Execute(context.Background(), "bash", mustJSON(t, map[string]any{"command": cmd, "description": "t"}))
	if !strings.Contains(res.Text, deniedMarker) || !strings.Contains(res.Text, "final") {
		t.Fatalf("rejected should be final denial, got: %s", res.Text)
	}
	// 拒绝后带 sandbox_permissions 重试：无记录，直接报错（不二次打扰用户）
	res = r2.Execute(context.Background(), "bash", mustJSON(t, map[string]any{
		"command": cmd, "description": "t",
		"sandbox_permissions": "danger-full-access", "justification": "再试试"}))
	if !strings.Contains(res.Text, "no recorded denial") {
		t.Fatalf("post-rejection retry should fail without re-asking, got: %s", res.Text)
	}
	if rhost.called != 1 {
		t.Fatalf("host should be asked exactly once, got %d", rhost.called)
	}
}

// grantHost 批准并授权目录的宿主。
type grantHost struct {
	NoopHost
	dir string
}

func (h *grantHost) ApproveEscalation(ctx context.Context, tool, key, mode, justification string) EscalationDecision {
	return EscalationDecision{Approved: true, GrantDir: h.dir}
}

// TestGrantDirPersistedInWorkspace 授权目录持久化：审批带 GrantDir → 落盘
// 工作区 .grf/grants.json → 同工作区后续写该目录不再询问（host 不再被调），
// 且只影响该工作区。
func TestGrantDirPersistedInWorkspace(t *testing.T) {
	ws, _ := os.MkdirTemp("", "grf-grant-ws-*")
	defer os.RemoveAll(ws)
	outside, _ := os.MkdirTemp("", "grf-grant-out-*")
	defer os.RemoveAll(outside)
	target := filepath.Join(outside, "a.txt")

	// 第一次：越权写 → 宿主批准并授权 target 所在目录
	fs := NewFsPolicy(ws, false, false, &grantHost{dir: outside})
	r := NewRegistry()
	art, _ := NewArtifacts()
	RegisterFs(r, ws, fs, art)
	res := r.Execute(context.Background(), "write", mustJSON(t, map[string]any{"file_path": target, "content": "x"}))
	if !strings.Contains(res.Text, "wrote") {
		t.Fatalf("granted write should succeed, got: %s", res.Text)
	}
	// 落盘校验
	data, err := os.ReadFile(filepath.Join(ws, ".grf", "grants.json"))
	if err != nil || !strings.Contains(string(data), outside) {
		t.Fatalf("grants.json should record %s: %v %s", outside, err, data)
	}

	// 同工作区重建策略（模拟新会话）：授权目录生效，不再询问（host=nil
	// 也能过——不经过审批通道）
	fs2 := NewFsPolicy(ws, false, false, nil)
	r2 := NewRegistry()
	RegisterFs(r2, ws, fs2, art)
	res = r2.Execute(context.Background(), "write", mustJSON(t, map[string]any{"file_path": filepath.Join(outside, "b.txt"), "content": "y"}))
	if !strings.Contains(res.Text, "wrote") {
		t.Fatalf("granted dir should allow without asking, got: %s", res.Text)
	}

	// 其他工作区不受影响：仍拒绝
	fs3 := NewFsPolicy(t.TempDir(), false, false, nil)
	r3 := NewRegistry()
	RegisterFs(r3, t.TempDir(), fs3, art)
	res = r3.Execute(context.Background(), "write", mustJSON(t, map[string]any{"file_path": filepath.Join(outside, "c.txt"), "content": "z"}))
	if !strings.Contains(res.Text, "[sandbox:") {
		t.Fatalf("other workspace must not inherit grants, got: %s", res.Text)
	}
}
