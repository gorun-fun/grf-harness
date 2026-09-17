// main_test.go grf-computer 装配自检：工具全部注册且命名合法。
// 平台实现按 GOOS 选定（本测试不触发真实屏幕操作）。
package main

import (
	"regexp"
	"testing"

	"git.gorun.fun/share/grf-harness/plugins/internal/mcpserver"
)

var nameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func TestComputerToolsRegistered(t *testing.T) {
	s := mcpserver.New("grf-computer", "test")
	registerComputerTools(s, newPlatform())
	names := s.Names()
	want := map[string]bool{
		"computer_screenshot": true, "computer_screen_size": true, "computer_click": true,
		"computer_list_displays": true,
		"computer_move": true, "computer_drag": true, "computer_scroll": true,
		"computer_type": true, "computer_key": true, "computer_clipboard": true,
		"computer_wait": true,
	}
	if len(names) != len(want) {
		t.Fatalf("工具数 %d，want %d: %v", len(names), len(want), names)
	}
	for _, n := range names {
		if !want[n] {
			t.Fatalf("意外工具 %q", n)
		}
		if !nameRe.MatchString(n) {
			t.Fatalf("工具名不合法: %q", n)
		}
	}
}

func TestComputerPlatformSmoke(t *testing.T) {
	if newPlatform() == nil {
		t.Fatal("platform 不应为 nil")
	}
}
