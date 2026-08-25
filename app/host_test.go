// host_test.go TaskHost 选项题答案解析：序号、自定义文字与多选混合。
package app

import (
	"reflect"
	"testing"

	"grf/tools"
)

func opts(n ...string) []tools.Option {
	var out []tools.Option
	for _, l := range n {
		out = append(out, tools.Option{Label: l})
	}
	return out
}

func TestOptionAnswerSingle(t *testing.T) {
	q := tools.Question{Options: opts("A", "B")}
	cases := []struct {
		in   string
		want any
	}{
		{"1", "A"},
		{" 2 \n", "B"},
		{"B", "B"},           // 序号优先：非数字按自定义文字
		{"就用 B 吧", "就用 B 吧"}, // 自定义文字原样作答
		{"7", "7"},           // 越界序号视作自定义文字
		{"", "A"},            // 空输入默认第一项（原行为）
		{"   \n", "A"},
	}
	for _, c := range cases {
		if got := optionAnswer(q, c.in); got != c.want {
			t.Errorf("optionAnswer(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestOptionAnswerMulti(t *testing.T) {
	q := tools.Question{Options: opts("A", "B", "C"), MultiSelect: true}
	cases := []struct {
		in   string
		want []string
	}{
		{"1,3", []string{"A", "C"}},
		{"1，3", []string{"A", "C"}},                    // 全角逗号
		{"1,C,其余不要", []string{"A", "C", "其余不要"}}, // 序号与自定义混用
		{"只做 A 和 B", []string{"只做 A 和 B"}},         // 全自定义
		{"", []string{"A"}},                           // 空输入默认第一项
	}
	for _, c := range cases {
		got := optionAnswer(q, c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("optionAnswer(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
