// main_test.go grf-browser 装配自检：工具全部注册且命名符合 harness 约定
// （模型可见名 [a-zA-Z0-9_-]{1,64}）。浏览器进程为懒启动，这里只验证
// 注册，不拉起 chrome。
package main

import (
	"regexp"
	"testing"
)

var nameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func TestBrowserToolsRegistered(t *testing.T) {
	s, _ := newServer(true, "")
	names := s.Names()
	want := map[string]bool{
		"browser_navigate": true, "browser_back": true, "browser_forward": true,
		"browser_reload": true, "browser_snapshot": true, "browser_click": true,
		"browser_type": true, "browser_press_key": true, "browser_hover": true,
		"browser_select_option": true, "browser_evaluate": true, "browser_take_screenshot": true,
		"browser_get_text": true, "browser_console_messages": true, "browser_network_requests": true,
		"browser_tabs": true, "browser_close": true,
		"browser_scroll": true, "browser_wait_for": true, "browser_file_upload": true,
		"browser_cookies": true,
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
