// inbox_test.go 收件箱 hub 单元测试：离线排队/在线直投、TTL 丢弃、
// 连接簿记；同步档位（sync.go）归一与 git/full 自动清单。
package cluster

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInboxHubQueueAndDeliver(t *testing.T) {
	h := newInboxHub()
	// 离线：入队
	cmd := InboxCommand{ID: "c1", Type: InboxTask, TaskID: "t1", Input: "x"}
	if online, err := h.enqueue("n1", cmd); err != nil || online {
		t.Fatalf("离线 enqueue = online:%v err:%v", online, err)
	}
	if h.connected("n1") {
		t.Fatal("未订阅不应 connected")
	}
	// 订阅：积压指令应送达
	ch, unsub := h.subscribe("n1")
	defer unsub()
	select {
	case got := <-ch:
		if got.ID != "c1" {
			t.Fatalf("积压指令 = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("积压指令未送达")
	}
	if !h.connected("n1") {
		t.Fatal("订阅后应 connected")
	}
	// 在线：直投通道
	if _, err := h.enqueue("n1", InboxCommand{ID: "c2", Type: InboxTask}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-ch:
		if got.ID != "c2" {
			t.Fatalf("在线指令 = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("在线指令未送达")
	}
	// 注销后再查：断开
	unsub()
	if h.connected("n1") {
		t.Fatal("注销后不应 connected")
	}
}

func TestInboxHubTTLDrop(t *testing.T) {
	h := newInboxHub()
	old := InboxCommand{ID: "stale", Type: InboxTask, TS: time.Now().Add(-2 * inboxTTL).UnixMilli()}
	fresh := InboxCommand{ID: "fresh", Type: InboxTask, TS: time.Now().UnixMilli()}
	h.queue["n1"] = []InboxCommand{old, fresh} // 直接注入队列（绕过 enqueue 的 TS 兜底，手工控制年龄）
	ch, unsub := h.subscribe("n1")
	defer unsub()
	select {
	case got := <-ch:
		if got.ID != "fresh" {
			t.Fatalf("过期指令应被丢弃，收到 %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("fresh 指令未送达")
	}
}

func TestInboxHubQueueCap(t *testing.T) {
	h := newInboxHub()
	for i := 0; i < inboxQueueCap; i++ {
		if _, err := h.enqueue("n1", InboxCommand{ID: "c", Type: InboxTask}); err != nil {
			t.Fatalf("第 %d 个不应报错：%v", i, err)
		}
	}
	if _, err := h.enqueue("n1", InboxCommand{ID: "over", Type: InboxTask}); err == nil {
		t.Fatal("超队列上限应报错")
	}
}

func TestSyncModeNormalize(t *testing.T) {
	cases := map[string]string{
		"":       "",
		"none":   "",
		"NONE":   "",
		"seed":   "seed",
		" Seed ": "seed",
		"git":    "git",
		"Git":    "git",
		"FULL":   "full",
		"weird":  "",
	}
	for in, want := range cases {
		if got := SyncMode(in); got != want {
			t.Errorf("SyncMode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGitTrackedFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git 不可用")
	}
	ws := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", ws}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "ignored.txt"), []byte("i"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "a.txt")
	run("commit", "-q", "-m", "x")
	files, err := GitTrackedFiles(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != "a.txt" {
		t.Fatalf("跟踪文件 = %v, want [a.txt]", files)
	}
	// 非仓库报错（可行动信息）
	if _, err := GitTrackedFiles(t.TempDir()); err == nil || !strings.Contains(err.Error(), "git") {
		t.Fatalf("非 git 仓库应报错： %v", err)
	}
}

func TestAllWorkspaceFilesSkipsDotGit(t *testing.T) {
	ws := t.TempDir()
	for _, p := range []string{"a.txt", "sub/b.txt", ".git/objects/pack.dat", "c.md"} {
		full := filepath.Join(ws, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files, err := AllWorkspaceFiles(ws)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(files, ",")
	if strings.Contains(got, ".git") {
		t.Fatalf("应跳过 .git：%v", files)
	}
	if !strings.Contains(got, "a.txt") || !strings.Contains(got, filepath.Join("sub", "b.txt")) || !strings.Contains(got, "c.md") {
		t.Fatalf("应含全部常规文件：%v", files)
	}
}
