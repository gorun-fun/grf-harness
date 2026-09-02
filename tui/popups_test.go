package tui

import (
	"strings"
	"testing"
)

func TestPopupScrollWindow(t *testing.T) {
	items := make([]string, 50)
	for i := range items {
		items[i] = "m" + string(rune('a'+i%26)) + string(rune('0'+i/10))
	}
	p := &popup{title: "选择模型", items: items, sel: 0}

	// 窗口高度受限：总行数不超 maxH，且含截断提示
	lines := p.render(80, 10)
	if len(lines) > 10 {
		t.Fatalf("行数 %d 超预算 10", len(lines))
	}
	if !strings.Contains(lines[len(lines)-1], "↓") {
		t.Fatalf("底部应有截断提示: %q", lines[len(lines)-1])
	}

	// 选中项滚到底部仍可见
	p.sel = 49
	lines = p.render(80, 10)
	found := false
	for _, ln := range lines {
		if strings.Contains(ln, "❯") {
			found = strings.Contains(ln, items[49])
		}
	}
	if !found {
		t.Fatalf("选中项 49 不可见: %v", lines)
	}
	if !strings.Contains(lines[1], "↑") {
		t.Fatalf("顶部应有截断提示: %q", lines[1])
	}

	// 初始选中项靠后时，首次渲染即滚到可见
	p2 := &popup{title: "t", items: items, sel: 40}
	lines = p2.render(80, 10)
	visible := false
	for _, ln := range lines {
		if strings.Contains(ln, "❯") && strings.Contains(ln, items[40]) {
			visible = true
		}
	}
	if !visible {
		t.Fatalf("初始 sel=40 应立即可见: %v", lines)
	}

	// 短列表不受影响：无提示、全量展示
	p3 := &popup{title: "t", items: items[:3], sel: 1, mark: items[0]}
	lines = p3.render(80, 10)
	if len(lines) != 4 || strings.Contains(strings.Join(lines, "\n"), "还有") {
		t.Fatalf("短列表应全量展示: %v", lines)
	}
}
