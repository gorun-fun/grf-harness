package tui

import "testing"

func TestListWindow(t *testing.T) {
	// 列表不超出可用行数时全量显示
	s, e, up, down := listWindow(3, 0, 10)
	if s != 0 || e != 3 || up || down {
		t.Fatalf("短列表应全量显示: %d %d %v %v", s, e, up, down)
	}
	// 超长列表：选中项在窗口内且窗口不越界
	for sel := 0; sel < 100; sel++ {
		s, e, up, down := listWindow(100, sel, 10)
		if e-s != 10 || s < 0 || e > 100 {
			t.Fatalf("sel=%d 窗口非法: [%d,%d)", sel, s, e)
		}
		if sel < s || sel >= e {
			t.Fatalf("sel=%d 不在窗口 [%d,%d) 内", sel, s, e)
		}
		if up != (s > 0) || down != (e < 100) {
			t.Fatalf("sel=%d 提示标记错误", sel)
		}
	}
}
