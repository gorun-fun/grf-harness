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
	f := newAskForm(qs, func(a map[string]any, err error) { got, asErr = a, err })
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
	f := newAskForm(qs, func(a map[string]any, err error) { got, asErr = a, err })
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
	f := newAskForm(qs, func(map[string]any, error) {})
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
