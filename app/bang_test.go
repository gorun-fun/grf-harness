// bang_test.go "!cmd" 直执行：ExecBang 走 bash 工具、BangOutput 清理
// 面向模型的 exit code 标注。
package app

import (
	"context"
	"strings"
	"testing"
)

func TestExecBang(t *testing.T) {
	a, err := New(Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	res := a.ExecBang(context.Background(), "echo bang-smoke")
	if res.Err != "" {
		t.Fatalf("执行失败: %s", res.Err)
	}
	if got := BangOutput(res.Text); got != "bang-smoke" {
		t.Fatalf("BangOutput 清理结果不符: %q", got)
	}

	// 失败命令：非零退出仍算工具成功（输出带 exit code），错误路径
	// 只覆盖真正跑不起来的情形（如超时/未知工具）。
	res = a.ExecBang(context.Background(), "exit 3")
	if res.Err != "" || !strings.Contains(res.Text, "exit code: 3") {
		t.Fatalf("非零退出命令结果不符: %+v", res)
	}
}

func TestBangOutput(t *testing.T) {
	cases := map[string]string{
		"hi\n[exit code: 0]\n":       "hi",
		"(no output) [exit code: 0]": "",
		"a\nb\n[exit code: 127]":     "a\nb",
		"no marker":                  "no marker",
	}
	for in, want := range cases {
		if got := BangOutput(in); got != want {
			t.Errorf("BangOutput(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRunBang 直执行一步完成（ExecBang + BangOutput + 错误归一）：
// 三表面（单任务直出 / 行模式 REPL / TUI 命令模式）共用的入口。
func TestRunBang(t *testing.T) {
	a, err := New(Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	out, err := a.RunBang(context.Background(), "echo run-bang")
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if out != "run-bang" {
		t.Fatalf("RunBang 输出不符: %q", out)
	}

	// 非零退出仍算工具成功：输出带 exit code 标注，由 BangOutput 清掉
	out, err = a.RunBang(context.Background(), "exit 3")
	if err != nil {
		t.Fatalf("非零退出不应报错: %v", err)
	}
	if out != "" {
		t.Fatalf("exit code 标注应被清理: %q", out)
	}
}
