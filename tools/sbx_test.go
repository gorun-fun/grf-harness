// sbx_test.go bash OS 级沙箱回归（仅 macOS sandbox-exec 可用环境）：
// 工作区外写/删必须被拒、工作区内正常。
package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func shellQ(p string) string { return "'" + strings.ReplaceAll(p, "'", "'\\''") + "'" }

func TestSandboxBlocksOutsideWriteAndDelete(t *testing.T) {
	if sandboxExecPath == "" {
		t.Skip("sandbox-exec unavailable")
	}
	ws := t.TempDir()
	inside := filepath.Join(ws, "in.txt")
	os.WriteFile(inside, []byte("x"), 0o644)
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "out.txt")
	os.WriteFile(outside, []byte("x"), 0o644)

	fs := NewFsPolicy(ws, false, false, &NoopHost{})
	r := NewRegistry()
	RegisterBash(r, BashDeps{Workspace: ws, Host: func() Host { return &NoopHost{} }, Fs: fs,
		SandboxOn: true, Jobs: NewJobManager()})

	run := func(cmd string) string {
		res := r.Execute(context.Background(), "bash", mustJSON(t, map[string]any{"command": cmd, "description": "t"}))
		return res.Text
	}
	// 工作区内写/删正常
	if out := run(fmt.Sprintf("touch %s && rm %s && echo OK_IN", shellQ(inside), shellQ(inside))); !strings.Contains(out, "OK_IN") {
		t.Fatalf("in-workspace write/rm failed: %s", out)
	}
	// 工作区外写被拒
	if out := run("touch " + shellQ(filepath.Join(outsideDir, "new.txt"))); !strings.Contains(out, deniedMarker) {
		t.Fatalf("outside write should be denied, got: %s", out)
	}
	// 工作区外删被拒（本次修复的回归点）
	if out := run("rm " + shellQ(outside)); !strings.Contains(out, deniedMarker) {
		t.Fatalf("outside rm should be denied, got: %s", out)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("outside file must survive: %v", err)
	}
	// 家目录写也被拒（旧 profile 的漏洞场景）
	if home, err := os.UserHomeDir(); err == nil {
		if out := run("touch " + shellQ(filepath.Join(home, ".grf-sbx-probe"))); !strings.Contains(out, deniedMarker) {
			t.Fatalf("home write should be denied, got: %s", out)
		}
		os.Remove(filepath.Join(home, ".grf-sbx-probe"))
	}
}

func TestSandboxReadLimitBlocksOutsideRead(t *testing.T) {
	if sandboxExecPath == "" {
		t.Skip("sandbox-exec unavailable")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	ws := filepath.Join(home, ".grf-sbx-ws")
	os.MkdirAll(ws, 0o755)
	defer os.RemoveAll(ws)
	os.WriteFile(filepath.Join(ws, "in.txt"), []byte("hello"), 0o644)
	secret := filepath.Join(home, ".grf-sbx-secret.txt")
	os.WriteFile(secret, []byte("secret"), 0o644)
	defer os.Remove(secret)

	r := NewRegistry()
	RegisterBash(r, BashDeps{Workspace: ws, Host: func() Host { return &NoopHost{} },
		SandboxOn: true, ReadLimit: true, Jobs: NewJobManager()})
	run := func(cmd string) string {
		res := r.Execute(context.Background(), "bash", mustJSON(t, map[string]any{"command": cmd, "description": "t"}))
		return res.Text
	}
	// 工作区内读正常（家目录内的工作区被 allow 覆盖）
	if out := run("cat " + shellQ(filepath.Join(ws, "in.txt"))); !strings.Contains(out, "hello") {
		t.Fatalf("in-workspace read failed: %s", out)
	}
	// 系统文件可读（命令能正常跑）
	if out := run("cat /etc/hosts"); !strings.Contains(out, "localhost") {
		t.Fatalf("system read failed: %s", out)
	}
	// 家目录下工作区外读被拒（本次修复点）
	if out := run("cat " + shellQ(secret)); !strings.Contains(out, deniedMarker) {
		t.Fatalf("outside-home read should be denied, got: %s", out)
	}
}
