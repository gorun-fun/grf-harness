//go:build smoke

// smoke_actions_test.go 本机手动冒烟（-tags smoke）：TextEdit 上验证
// 元素设值/读取/窗口截图的真实链路，结束后不保存关闭。
package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestSmokeDarwinActions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	d := darwinPlatform{}
	// 结束后不保存关闭（防留脏窗口）
	defer func() {
		_, _ = run(ctx, "osascript", "-e",
			`tell application "TextEdit" to activate`) // noop 兜底
		_, _ = run(ctx, "osascript", "-e",
			`tell application "System Events" to tell process "TextEdit" to keystroke "w" using command down`)
		_, _ = run(ctx, "osascript", "-e",
			`tell application "System Events" to tell process "TextEdit" to keystroke return`) // 不保存
	}()

	if err := d.ActivateApp(ctx, "TextEdit"); err != nil {
		t.Fatalf("ActivateApp: %v", err)
	}
	time.Sleep(1200 * time.Millisecond)
	// 经 AppleEvent 直接建文档（不依赖前台/解锁状态）
	if _, err := run(ctx, "osascript", "-e", `tell application id "com.apple.TextEdit" to make new document`); err != nil {
		t.Fatalf("make document: %v", err)
	}
	time.Sleep(800 * time.Millisecond)

	apps, _ := d.Apps(ctx)
	var pid int
	for _, a := range apps {
		if a.Name == "TextEdit" || a.Name == "文本编辑" {
			pid = a.PID
			break
		}
	}
	if pid == 0 {
		t.Fatal("TextEdit 未运行")
	}

	nodes, err := d.DumpElements(ctx, pid, 10)
	if err != nil {
		t.Fatalf("DumpElements: %v", err)
	}
	st := states.put(pid, "文本编辑", nodes)
	t.Log("\n" + renderState(st))

	// 找文本区设值
	var area *Node
	for _, n := range nodes {
		if n.Role == "AXTextArea" {
			area = n
			break
		}
	}
	if area == nil {
		t.Fatal("未找到 AXTextArea")
	}
	if _, err := d.ElementAction(ctx, pid, area.Path, area.Role, area.Name, "set_value", "grf-smoke-测试文本"); err != nil {
		t.Fatalf("set_value: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	// 重读验证值已写入
	nodes2, err := d.DumpElements(ctx, pid, 10)
	if err != nil {
		t.Fatalf("re-dump: %v", err)
	}
	found := false
	for _, n := range nodes2 {
		if n.Role == "AXTextArea" && strings.Contains(n.Value, "grf-smoke-测试文本") {
			found = true
		}
	}
	if !found {
		t.Fatalf("设值后未读到文本：\n%+v", nodes2)
	}
	t.Log("set_value 生效并读回验证通过")

	// 签名防呆：用旧快照的 path/role 但已过期的名称校验应触发 ERR_CHANGED？
	// （此处直接验证 locate 路径））
	res, err := d.ElementAction(ctx, pid, area.Path, area.Role, area.Name, "locate", "")
	if err != nil {
		t.Fatalf("locate: %v", err)
	}
	t.Logf("locate -> %+v", res)

	// 窗口截图
	if len(nodes2) > 0 && nodes2[0].WindowID > 0 {
		data, err := d.ScreenshotWindow(ctx, nodes2[0].WindowID)
		if err != nil {
			t.Fatalf("ScreenshotWindow: %v", err)
		}
		if w, h, ok := pngSize(data); !ok || w == 0 {
			t.Fatalf("窗口截图异常 %dx%d", w, h)
		} else {
			t.Logf("窗口截图 %dx%d 通过", w, h)
		}
	}
}
