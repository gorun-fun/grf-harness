// repl_test.go 行模式 REPL 的公共件：斜杠指令表提示行格式与
// ResumeByIndex 按序号恢复会话。
package app

import (
	"strings"
	"testing"

	"git.gorun.fun/share/grf-harness/llm"
)

// TestSlashHintLine 提示行格式："name alias  desc"（别名与主指令空格
// 分隔——TUI 的 Tab 补全从 fields[0] 取主指令，格式不能变）。
func TestSlashHintLine(t *testing.T) {
	cases := []struct {
		name, alias, desc, want string
	}{
		{"/help", "", "显示本帮助", "/help  显示本帮助"},
		{"/new", "/clear", "保存当前会话并开新会话", "/new /clear  保存当前会话并开新会话"},
	}
	for _, c := range cases {
		if got := SlashHintLine(c.name, c.alias, c.desc); got != c.want {
			t.Errorf("SlashHintLine(%q,%q,%q) = %q, want %q", c.name, c.alias, c.desc, got, c.want)
		}
	}
}

// TestResumeByIndex 按列表序号恢复：越界/非正序号报错，成功即完成
// Restore 并返回会话标题。
func TestResumeByIndex(t *testing.T) {
	isolateConfig(t, "")
	a, err := New(Options{Workspace: t.TempDir(), Client: testClient()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	if _, err := a.ResumeByIndex(1); err == nil || !strings.Contains(err.Error(), "序号超出范围") {
		t.Fatalf("无会话时序号 1 应报越界，得到 %v", err)
	}
	if _, err := a.ResumeByIndex(0); err == nil {
		t.Fatal("序号 0 应报错（下标从 1 起）")
	}

	a.Root().RestoreFull([]llm.Message{{Role: "user", Content: "第一场会话"}}, "")
	if _, err := a.SaveSession(); err != nil {
		t.Fatal(err)
	}
	title, err := a.ResumeByIndex(1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(title, "第一场会话") {
		t.Fatalf("标题应来自会话内容: %q", title)
	}
	if n := len(a.Root().History()); n != 2 { // system + user
		t.Fatalf("恢复后历史长度 = %d, want 2", n)
	}
	if _, err := a.ResumeByIndex(2); err == nil || !strings.Contains(err.Error(), "序号超出范围") {
		t.Fatalf("越界序号应报错，得到 %v", err)
	}
}
