package term

import "testing"

func TestStringWidthCJK(t *testing.T) {
	if got := StringWidth("你好abc"); got != 7 {
		t.Fatalf("want 7 (2+2+3), got %d", got)
	}
	if got := StringWidth("\x1b[2m暗色\x1b[0m"); got != 4 {
		t.Fatalf("ANSI should not count, got %d", got)
	}
	if got := StringWidth("a\u200bb"); got != 2 {
		t.Fatalf("zero-width joiner, got %d", got)
	}
}

func TestWrapCJK(t *testing.T) {
	lines := Wrap("abcdefghij", 4)
	if len(lines) != 3 || lines[0] != "abcd" || lines[2] != "ij" {
		t.Fatalf("ascii wrap: %v", lines)
	}
	lines = Wrap("你好世界测试", 4)
	if len(lines) != 3 || lines[0] != "你好" || lines[1] != "世界" {
		t.Fatalf("cjk wrap: %v", lines)
	}
	// ANSI 样式跨折行延续（每折行段行尾重置、行首重开）
	lines = Wrap("\x1b[2m一二三四五\x1b[0m", 4)
	if len(lines) != 3 {
		t.Fatalf("styled wrap: %v", lines)
	}
	if lines[0] != "\x1b[2m一二\x1b[0m" {
		t.Fatalf("first: %q", lines[0])
	}
	if lines[1] != "\x1b[2m三四\x1b[0m" {
		t.Fatalf("second should re-open style and reset: %q", lines[1])
	}
}

func TestStripANSI(t *testing.T) {
	if got := StripANSI("\x1b[31m红\x1b[0m字"); got != "红字" {
		t.Fatalf("got %q", got)
	}
}

func TestTruncateWidth(t *testing.T) {
	if got := Truncate("你好世界", 5); got != "你好" {
		t.Fatalf("got %q", got)
	}
}

func TestParseKeySequences(t *testing.T) {
	cases := []struct {
		in    string
		name  string
		rune_ rune
	}{
		{"\x1b[A", KeyUp, 0},
		{"\x1b[B", KeyDown, 0},
		{"\x1b[5~", KeyPgUp, 0},
		{"\x1b[3~", KeyDelete, 0},
		{"\x1b[H", KeyHome, 0},
		{"\x1b", KeyEsc, 0},
		{"\r", KeyEnter, 0},
		{"\x7f", KeyBackspace, 0},
		{"\x03", "ctrl+c", 0},
		{"中", "中", '中'},
	}
	for _, c := range cases {
		ev, used := parseEvent([]byte(c.in))
		if used != len(c.in) {
			t.Fatalf("%q consumed %d", c.in, used)
		}
		k, ok := ev.(KeyEv)
		if !ok || k.Name != c.name || k.Rune != c.rune_ {
			t.Fatalf("%q => %#v", c.in, ev)
		}
	}
}

func TestParseMouseSGR(t *testing.T) {
	ev, used := parseEvent([]byte("\x1b[<0;10;5M"))
	if used != len("\x1b[<0;10;5M") {
		t.Fatalf("used %d", used)
	}
	m, ok := ev.(MouseEv)
	if !ok || m.Type != "press" || m.Button != 0 || m.X != 9 || m.Y != 4 {
		t.Fatalf("%#v", ev)
	}
	ev, _ = parseEvent([]byte("\x1b[<64;1;1M"))
	m = ev.(MouseEv)
	if m.Type != "wheel" || m.Button != 0 {
		t.Fatalf("%#v", ev)
	}
}

func TestParsePaste(t *testing.T) {
	in := "\x1b[200~hello\r\nworld\x1b[201~rest"
	ev, used := parseEvent([]byte(in))
	if used != len("\x1b[200~hello\r\nworld\x1b[201~") {
		t.Fatalf("used %d", used)
	}
	p, ok := ev.(PasteEv)
	if !ok || p.Text != "hello\nworld" {
		t.Fatalf("%#v", ev)
	}
}
