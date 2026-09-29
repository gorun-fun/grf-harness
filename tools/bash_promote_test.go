// bash_promote_test.go 前台命令超时晋升（promoteOnTimeout）回归：
// 到期不杀进程、转后台 job、一次性带回已产出输出、job_output 接续。
package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestBashPromoteOnTimeout(t *testing.T) {
	ws := t.TempDir()
	jobs := NewJobManager()
	r := NewRegistry()
	RegisterBash(r, BashDeps{Workspace: ws, Jobs: jobs})

	res := r.Execute(context.Background(), "bash", mustJSON(t, map[string]any{
		"command": "echo EARLY; sleep 1; echo LATE", "description": "t", "timeoutMs": 300,
	}))
	if res.Err != "" {
		t.Fatalf("promotion must not be an error: %s", res.Err)
	}
	if !strings.Contains(res.Text, "moved to background job job_") {
		t.Fatalf("expected promotion handoff, got: %s", res.Text)
	}
	if !strings.Contains(res.Text, "EARLY") {
		t.Fatalf("handoff must carry the output produced so far: %s", res.Text)
	}
	if strings.Contains(res.Text, "LATE") {
		t.Fatalf("handoff must not wait for the command to finish: %s", res.Text)
	}
	id := res.Text[strings.Index(res.Text, "job_"):]
	if i := strings.IndexAny(id, "] \n,"); i > 0 {
		id = id[:i]
	}

	out, err := jobs.Output(context.Background(), id, true, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "LATE") || !strings.Contains(out, "[status: done]") {
		t.Fatalf("job must settle done with the remaining output, got: %s", out)
	}
	if strings.Contains(out, "EARLY") {
		t.Fatalf("job_output must continue after the consumed prefix, not repeat it: %s", out)
	}
}

func TestBashForegroundCompletesWithinTimeout(t *testing.T) {
	ws := t.TempDir()
	jobs := NewJobManager()
	r := NewRegistry()
	RegisterBash(r, BashDeps{Workspace: ws, Jobs: jobs})

	res := r.Execute(context.Background(), "bash", mustJSON(t, map[string]any{
		"command": "echo hi", "description": "t",
	}))
	if !strings.Contains(res.Text, "hi") || !strings.Contains(res.Text, "[exit code: 0]") {
		t.Fatalf("fast command must return normally, got: %s", res.Text)
	}
}

func TestBashTimeoutWithoutJobManager(t *testing.T) {
	// 无 JobManager 可晋升时退回旧语义：到期终止并报超时
	ws := t.TempDir()
	r := NewRegistry()
	RegisterBash(r, BashDeps{Workspace: ws})

	res := r.Execute(context.Background(), "bash", mustJSON(t, map[string]any{
		"command": "sleep 2", "description": "t", "timeoutMs": 300,
	}))
	if res.Err == "" || !strings.Contains(res.Err, "timed out") {
		t.Fatalf("expected timeout error without promotion, got err=%q text=%q", res.Err, res.Text)
	}
}
