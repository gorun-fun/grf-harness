// ask_test.go ask 表单：选项题的"✎ 自定义输入"行与手输作答。
package tui

import (
	"strings"
	"testing"

	"git.gorun.fun/share/grf-harness/tools"
)

// drive 用按键序列驱动 ask 表单（rune 按一次一个字符）。
func drive(c *Chat, keys ...termKey) {
	for _, k := range keys {
		c.askKey(k)
	}
}

func runeKeys(s string) []termKey {
	ks := make([]termKey, 0, len(s))
	for _, r := range s {
		ks = append(ks, termKey{Name: "rune", Rune: r})
	}
	return ks
}

func TestAskFormCustomAnswer(t *testing.T) {
	qs := []tools.Question{{ID: "q1", Question: "选哪个", Options: []tools.Option{{Label: "A"}, {Label: "B"}}}}
	var got map[string]any
	var asErr error
	f := newAskForm(qs, func(a map[string]any, _ map[string]int, err error) { got, asErr = a, err })
	c := &Chat{ask: f}

	// ↓↓ 移到 ✎ 自定义输入 行 → 回车进入手输 → 打字 → 回车提交
	drive(c, append([]termKey{{Name: "down"}, {Name: "down"}, {Name: "enter"},
		{Name: "space"}}, runeKeys("就选 C 吧")...)...)
	drive(c, termKey{Name: "enter"})

	if asErr != nil {
		t.Fatalf("unexpected err: %v", asErr)
	}
	if got["q1"] != "就选 C 吧" {
		t.Fatalf("answer = %v, want 自定义文本", got["q1"])
	}
	if c.ask != nil {
		t.Fatal("form should be closed after finishing all questions")
	}
}

func TestAskFormCustomEscBackToOptions(t *testing.T) {
	qs := []tools.Question{{ID: "q1", Question: "选哪个", Options: []tools.Option{{Label: "A"}}}}
	var got map[string]any
	var asErr error
	f := newAskForm(qs, func(a map[string]any, _ map[string]int, err error) { got, asErr = a, err })
	c := &Chat{ask: f}

	// 进入手输 → Esc 应回到选项列表（而非取消全部），再选 A 提交
	drive(c, termKey{Name: "down"}, termKey{Name: "enter"})
	drive(c, runeKeys("临时")...)
	drive(c, termKey{Name: "esc"}, termKey{Name: "up"}, termKey{Name: "enter"})

	if asErr != nil {
		t.Fatalf("esc in custom mode should not cancel, err: %v", asErr)
	}
	if got["q1"] != "A" {
		t.Fatalf("answer = %v, want A", got["q1"])
	}
}

func TestAskFormCustomRowRendered(t *testing.T) {
	qs := []tools.Question{{ID: "q1", Question: "选哪个", Options: []tools.Option{{Label: "A"}}}}
	f := newAskForm(qs, func(map[string]any, map[string]int, error) {})
	lines := f.render(60)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "✎ 自定义输入") {
		t.Fatalf("custom row missing in render:\n%s", joined)
	}
	// 手输模式下渲染编辑框与返回提示
	f.custom = true
	lines = f.render(60)
	joined = strings.Join(lines, "\n")
	if !strings.Contains(joined, "Esc 返回选项") {
		t.Fatalf("custom-mode hint missing in render:\n%s", joined)
	}
}

// TestAskFormPickedIndex 作答来源下标：选项作答记选中下标；自定义输入
// （即使文本恰与选项文案相同）记 -1——host.go 的审批分支据此按下标
// 判定，不受文案碰撞/文案改动影响。
func TestAskFormPickedIndex(t *testing.T) {
	qs := []tools.Question{{ID: "q1", Question: "选哪个", Options: []tools.Option{{Label: "甲"}, {Label: "乙"}}}}
	var picked map[string]int
	f := newAskForm(qs, func(_ map[string]any, p map[string]int, _ error) { picked = p })
	c := &Chat{ask: f}

	// 默认选中第 0 项提交
	drive(c, termKey{Name: "enter"})
	if got := picked["q1"]; got != 0 {
		t.Fatalf("选项作答 picked = %d, want 0", got)
	}

	// 第二项提交
	f2 := newAskForm(qs, func(_ map[string]any, p map[string]int, _ error) { picked = p })
	c.ask = f2
	drive(c, termKey{Name: "down"}, termKey{Name: "enter"})
	if got := picked["q1"]; got != 1 {
		t.Fatalf("第二项作答 picked = %d, want 1", got)
	}

	// ✎ 自定义输入手输与选项同文案，仍应记 -1
	f3 := newAskForm(qs, func(_ map[string]any, p map[string]int, _ error) { picked = p })
	c.ask = f3
	drive(c, termKey{Name: "down"}, termKey{Name: "down"}, termKey{Name: "enter"}) // 移到自定义行并进入手输
	drive(c, runeKeys("甲")...)
	drive(c, termKey{Name: "enter"})
	if got := picked["q1"]; got != -1 {
		t.Fatalf("自定义作答 picked = %d, want -1", got)
	}
}
