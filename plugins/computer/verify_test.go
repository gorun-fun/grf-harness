// verify_test.go set_value 回读校验与终端应用识别的单元测试。
package main

import (
	"strings"
	"testing"
)

func TestVerifySetValue(t *testing.T) {
	// 回读一致
	if v, err := verifySetValue("abc", "set:abc"); !v || err != nil {
		t.Fatalf("一致应通过: v=%v err=%v", v, err)
	}
	// 数值等价（"0.5" vs "0.50"）
	if v, err := verifySetValue("0.5", "set:0.50"); !v || err != nil {
		t.Fatalf("数值等价应通过: v=%v err=%v", v, err)
	}
	// 回读为空：无法确认，不报错
	if v, err := verifySetValue("secret", "set:"); v || err != nil {
		t.Fatalf("空回读应 verified=false 无错: v=%v err=%v", v, err)
	}
	// 平台未实现回读（裸 "set"）：无法确认，不报错
	if v, err := verifySetValue("x", "set"); v || err != nil {
		t.Fatalf("无回读前缀应 verified=false 无错: v=%v err=%v", v, err)
	}
	// 回读不符（滑杆钳制 0.5 → 0.0）：必须报错，不假成功
	v, err := verifySetValue("0.5", "set:0.0")
	if v || err == nil || !strings.Contains(err.Error(), "设置未生效") {
		t.Fatalf("回读不符应报错: v=%v err=%v", v, err)
	}
}

func TestIsTerminalApp(t *testing.T) {
	cases := []struct {
		name, bundle string
		want         bool
	}{
		{"终端", "", true},                 // macOS 中文系统 Terminal
		{"Terminal", "", true},           // 英文名
		{"", "com.apple.Terminal", true}, // bundle 命中
		{"", "com.googlecode.iterm2", true},
		{"iTerm2", "", true},
		{"gnome-terminal-server", "", true},
		{"Safari", "", false},
		{"访达", "", false},
		{"", "com.apple.Safari", false},
		{"WindowsTerminal.exe", "", true},
	}
	for _, c := range cases {
		if got := isTerminalApp(c.name, c.bundle); got != c.want {
			t.Errorf("isTerminalApp(%q, %q) = %v, want %v", c.name, c.bundle, got, c.want)
		}
	}
}

func TestLooksLikeIME(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"com.sogou.inputmethod.sogou.pinyin", true},
		{"com.apple.inputmethod.SCIM.ITABC", true},
		{"com.apple.keylayout.ABC", false},
		{"", false},
	}
	for _, c := range cases {
		if got := looksLikeIME(c.in); got != c.want {
			t.Errorf("looksLikeIME(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestSnapWindowContaining(t *testing.T) {
	nodes := []*Node{
		{Depth: 0, Path: "1", Role: "AXWindow", X: 2379, Y: 426, W: 800, H: 600},
		{Depth: 1, Path: "1.1", Role: "AXGroup"},
		{Depth: 0, Path: "2", Role: "AXWindow", X: 0, Y: 0, W: 500, H: 400},
	}
	// 命中第 1 个窗口（含 8px 容差）
	n, ord, ok := snapWindowContaining(nodes, 2500, 600)
	if !ok || ord != 1 || n.X != 2379 {
		t.Fatalf("want window 1, got ord=%d ok=%v", ord, ok)
	}
	// 命中第 2 个
	if _, ord, ok = snapWindowContaining(nodes, 100, 100); !ok || ord != 2 {
		t.Fatalf("want window 2, got ord=%d ok=%v", ord, ok)
	}
	// 不在任何窗口
	if _, _, ok = snapWindowContaining(nodes, 5000, 5000); ok {
		t.Fatal("out-of-window coord should miss")
	}
	// 无几何的窗口层节点不参与
	if _, _, ok = snapWindowContaining([]*Node{{Depth: 0, Role: "AXWindow"}}, 0, 0); ok {
		t.Fatal("zero-size window should not match")
	}
}

func TestFrameStale(t *testing.T) {
	snap := &Node{X: 2379, Y: 426}
	if frameStale(snap, 2380, 430, 30) {
		t.Fatal("10px 漂移在容差内不应判过期")
	}
	if !frameStale(snap, 2733, 153, 30) {
		t.Fatal("窗口大幅挪动应判过期（D8 场景：2379,426 → 2733,153）")
	}
}
