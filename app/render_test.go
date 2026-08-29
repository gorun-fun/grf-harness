// render_test.go stdoutUI 尾部换行挂账：正文尾部连续换行延迟输出，
// 与收尾/事件行叠加时不产生多余空行。
package app

import (
	"bytes"
	"strings"
	"testing"
)

func TestStdoutUIPendingNewlines(t *testing.T) {
	var buf bytes.Buffer
	s := &stdoutUI{w: &buf, tty: false}
	s.content("abc\n")
	if buf.String() != "abc" {
		t.Fatalf("正文应立即输出、仅尾换行挂账: %q", buf.String())
	}
	s.content("\n")
	if buf.String() != "abc" {
		t.Fatalf("尾部换行应挂账不立即输出: %q", buf.String())
	}
	s.content("def")
	if buf.String() != "abc\n\ndef" {
		t.Fatalf("正文内部换行应原样保留: %q", buf.String())
	}
	s.printEvent("tool", "read x")
	if got := buf.String(); !strings.HasSuffix(got, "def\n[tool] read x\n") {
		t.Fatalf("事件行前只补一个换行，挂账尾换行应丢弃: %q", got)
	}
}
