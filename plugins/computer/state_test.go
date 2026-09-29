// state_test.go 状态缓存/渲染/TSV 解析/签名校验的单元测试（不触发真实
// 屏幕操作）。
package main

import (
	"strings"
	"testing"
	"time"
)

func TestStateStorePutGet(t *testing.T) {
	st := &stateStore{m: map[string]*State{}}
	s1 := st.put(100, "App", "", []*Node{{Depth: 0, Path: "1", Role: "AXWindow"}})
	s2 := st.put(200, "Other", "", []*Node{{Depth: 0, Path: "1", Role: "AXWindow"}})
	if got, ok := st.get(s1.ID); !ok || got.PID != 100 {
		t.Fatalf("get(s1) = %v, %v", got, ok)
	}
	if _, ok := st.get("不存在"); ok {
		t.Fatal("未知 state_id 不应命中")
	}
	_ = s2
	// 过期淘汰
	st.m[s1.ID].At = time.Now().Add(-2 * stateTTL)
	if _, ok := st.get(s1.ID); ok {
		t.Fatal("过期快照不应命中")
	}
}

func TestStateStoreEviction(t *testing.T) {
	st := &stateStore{m: map[string]*State{}}
	var first string
	for i := 0; i < 10; i++ {
		s := st.put(i, "App", "", nil)
		if i == 0 {
			first = s.ID
		}
	}
	if _, ok := st.get(first); ok {
		t.Fatal("超过 8 个快照后最老的应被淘汰")
	}
	if len(st.m) > 8 {
		t.Fatalf("缓存大小 %d 超上限", len(st.m))
	}
}

func TestRenderState(t *testing.T) {
	nodes := []*Node{
		{Depth: 0, Path: "1", Role: "AXWindow", Name: "主窗口", X: 10, Y: 20, W: 800, H: 600, WindowID: 42},
		{Depth: 1, Path: "1.1", Role: "AXButton", Name: "确定", Enabled: false},
		{Depth: 1, Path: "1.2", Role: "AXTextField", Name: "搜索", Value: "grf"},
	}
	out := renderState(&State{ID: "s1-1", PID: 7, App: "Test", Nodes: nodes})
	for _, want := range []string{"state_id=s1-1", "[0] AXWindow", "[1] AXButton \"确定\"", "✗", "[2] AXTextField", "=\"grf\"", "window_id=42"} {
		if !strings.Contains(out, want) {
			t.Fatalf("渲染缺 %q:\n%s", want, out)
		}
	}
}

func TestParseTSVNodes(t *testing.T) {
	out := "0|1|AXWindow|Notes|||100|50|800|600|1|77\n1|1.1|AXButton|新建||备注|110|60|80|24|1\nbad line\n"
	nodes := parseTSVNodes(out)
	if len(nodes) != 2 {
		t.Fatalf("应解析 2 个节点（坏行跳过），got %d", len(nodes))
	}
	if nodes[0].Role != "AXWindow" || nodes[0].WindowID != 77 || !nodes[0].Enabled {
		t.Fatalf("窗口节点解析错误: %+v", nodes[0])
	}
	if nodes[1].Path != "1.1" || nodes[1].Desc != "备注" || nodes[1].Y != 60 {
		t.Fatalf("子节点解析错误: %+v", nodes[1])
	}
}

func TestChangedError(t *testing.T) {
	want := &Node{Role: "AXButton", Name: "确定"}
	if err := changedError(1, want, &Node{Role: "AXButton", Name: "确定"}); err != nil {
		t.Fatalf("签名一致不应报错: %v", err)
	}
	err := changedError(1, want, &Node{Role: "AXTextField", Name: "确定"})
	if _, ok := err.(*errChanged); !ok {
		t.Fatalf("角色变化应返回 *errChanged，got %T", err)
	}
	// 快照名称为空（匿名容器）时名称变化不比对
	if err := changedError(1, &Node{Role: "AXGroup"}, &Node{Role: "AXGroup", Name: "新名"}); err != nil {
		t.Fatalf("匿名容器不应报错: %v", err)
	}
}

func TestTrunc(t *testing.T) {
	if got := trunc("abc", 5); got != "abc" {
		t.Fatalf("短串不应截断: %q", got)
	}
	if got := trunc("你好世界文本", 3); got != "你好世…" {
		t.Fatalf("长串截断错误: %q", got)
	}
}
