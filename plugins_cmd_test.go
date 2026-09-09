// plugins_cmd_test.go reorderFlags 表驱动：标志可写在位置参数前后。
package main

import (
	"reflect"
	"testing"
)

func TestReorderFlags(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"无标志", []string{"browser"}, []string{"browser"}},
		{"标志在前", []string{"-version", "1.0", "browser"}, []string{"-version", "1.0", "browser"}},
		{"标志在后", []string{"browser", "-version", "1.0"}, []string{"-version", "1.0", "browser"}},
		{"等号形式", []string{"browser", "--version=1.0"}, []string{"--version=1.0", "browser"}},
		{"标志收尾缺值", []string{"browser", "-version"}, []string{"-version", "browser"}},
		{"多名称", []string{"a", "b", "-version", "1", "c"}, []string{"-version", "1", "a", "b", "c"}},
		{"无参数", nil, nil},
	}
	for _, c := range cases {
		if got := reorderFlags(c.in, "version"); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: reorderFlags(%v) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
}
