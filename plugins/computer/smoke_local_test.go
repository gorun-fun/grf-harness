//go:build smoke

// smoke_local_test.go 本机手动冒烟（-tags smoke 才编译）：真实调用 osascript
// 验证 AX 树导出/元素定位/窗口枚举/应用列表。不进 CI。
package main

import (
	"context"
	"testing"
	"time"
)

func TestSmokeDarwinAX(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	d := darwinPlatform{}

	apps, err := d.Apps(ctx)
	if err != nil {
		t.Fatalf("Apps: %v", err)
	}
	t.Logf("apps(%d):", len(apps))
	for i, a := range apps {
		if i > 8 {
			break
		}
		t.Logf("  pid=%d %q front=%v", a.PID, a.Name, a.Frontmost)
	}
	var front *AppInfo
	for i := range apps {
		if apps[i].Frontmost {
			front = &apps[i]
			break
		}
	}
	if front == nil {
		t.Fatal("无前台应用")
	}
	wins, err := d.AppWindows(ctx, front.PID)
	if err != nil {
		t.Fatalf("AppWindows: %v", err)
	}
	t.Logf("windows: %+v", wins)

	nodes, err := d.DumpElements(ctx, front.PID, 8)
	if err != nil {
		t.Fatalf("DumpElements: %v", err)
	}
	st := states.put(front.PID, front.Name, "", nodes)
	t.Log("\n" + renderState(st))

	// 找一个可点的按钮/具名元素做 locate
	for i, n := range nodes {
		if n.Role == "AXButton" && n.Name != "" && i < len(nodes) {
			res, err := d.ElementAction(ctx, front.PID, n.Path, n.Role, n.Name, "locate", "")
			if err != nil {
				t.Fatalf("locate [%d]: %v", i, err)
			}
			t.Logf("locate [%d] %q -> %+v", i, n.Name, res)
			break
		}
	}
}
