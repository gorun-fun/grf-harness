// input.go 终端输入解析：把原始字节流解析为按键 / 粘贴 / 鼠标事件。
// 支持方向键、翻页、Home/End、Delete、Ctrl 组合、SS3 方向键、bracketed
// paste（\x1b[200~..201~）与 SGR 鼠标（\x1b[<b;x;yM/m，滚轮/按下/拖动/释放）。
package term

import (
	"io"
	"strings"
	"sync"
)

// Event 终端输入事件。
type Event interface{}

// KeyEv 按键事件：Name 为规范化名（up/enter/ctrl+c/…），Rune 非零表示
// 可打印字符（含中文）。
type KeyEv struct {
	Name string
	Rune rune
}

// PasteEv bracketed paste 粘贴内容。
type PasteEv struct{ Text string }

// MouseEv 鼠标事件。Type: press / motion / release / wheel。
// Button: 0=左 1=中 2=右；滚轮为 0(上)/1(下)。
type MouseEv struct {
	Type   string
	Button int
	X, Y   int
}

// Reader 后台读取并解析输入。
type Reader struct {
	ch   chan Event
	done chan struct{}
	once sync.Once
}

// Events 返回事件通道。
func (r *Reader) Events() <-chan Event { return r.ch }

// Close 停止读取 goroutine。
func (r *Reader) Close() { r.once.Do(func() { close(r.done) }) }

// ReadInput 启动一个 goroutine 持续读取 r 并解析为事件。
func ReadInput(r io.Reader) *Reader {
	rd := &Reader{ch: make(chan Event, 64), done: make(chan struct{})}
	go rd.loop(r)
	return rd
}

func (rd *Reader) loop(r io.Reader) {
	buf := make([]byte, 4096)
	var pending []byte
	for {
		n, err := r.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			pending = rd.dispatch(pending)
		}
		if err != nil {
			close(rd.ch)
			return
		}
		select {
		case <-rd.done:
			close(rd.ch)
			return
		default:
		}
	}
}

// dispatch 尽可能多地消费 pending 中的完整事件，返回剩余字节。
func (rd *Reader) dispatch(pending []byte) []byte {
	for len(pending) > 0 {
		ev, used := parseEvent(pending)
		if used == 0 {
			break // 序列不完整，等待更多字节
		}
		pending = pending[used:]
		if ev != nil {
			select {
			case rd.ch <- ev:
			case <-rd.done:
				return nil
			}
		}
	}
	// 单独一个 ESC 且读缓冲已空：视为 Esc 键（大多数本地终端序列一次性到达，
	// 到达 \x1b 后无后续字节即用户按下 Esc）。dispatch 无法感知"读空"，由
	// parseEvent 对裸 \x1b 返回 esc，若实际是序列前半部分，后续字节到达时
	// 会作为独立按键解析——终端场景下可接受。
	return pending
}

// parseEvent 解析 pending 开头的一个事件，返回 (事件, 消费字节数)。
// used==0 表示序列不完整。
func parseEvent(b []byte) (Event, int) {
	c := b[0]
	if c == 0x1b {
		if len(b) == 1 {
			return KeyEv{Name: KeyEsc}, 1
		}
		switch b[1] {
		case '[':
			return parseCSI(b)
		case 'O':
			if len(b) < 3 {
				return nil, 0
			}
			switch b[2] {
			case 'A':
				return KeyEv{Name: KeyUp}, 3
			case 'B':
				return KeyEv{Name: KeyDown}, 3
			case 'C':
				return KeyEv{Name: KeyRight}, 3
			case 'D':
				return KeyEv{Name: KeyLeft}, 3
			case 'H':
				return KeyEv{Name: KeyHome}, 3
			case 'F':
				return KeyEv{Name: KeyEnd}, 3
			case 'P':
				return KeyEv{Name: "f1"}, 3
			case 'Q':
				return KeyEv{Name: "f2"}, 3
			case 'R':
				return KeyEv{Name: "f3"}, 3
			case 'S':
				return KeyEv{Name: "f4"}, 3
			}
			return KeyEv{Name: KeyEsc}, 1
		default:
			// Alt+key：按 ESC 后跟字符处理，丢弃 Alt 语义返回字符本身
			if ev, used := parseEvent(b[1:]); used > 0 {
				if k, ok := ev.(KeyEv); ok && k.Rune != 0 {
					return k, 1 + used
				}
			}
			return KeyEv{Name: KeyEsc}, 1
		}
	}
	// 控制字符
	if name := CtrlName(c); name != "" {
		if name == "enter" {
			return KeyEv{Name: KeyEnter}, 1
		}
		if name == "tab" {
			return KeyEv{Name: KeyTab}, 1
		}
		if name == "backspace" {
			return KeyEv{Name: KeyBackspace}, 1
		}
		return KeyEv{Name: name}, 1
	}
	if c == ' ' {
		return KeyEv{Rune: ' ', Name: KeySpace}, 1
	}
	// UTF-8 多字节 rune
	if c < 0x80 {
		return KeyEv{Rune: rune(c), Name: string(c)}, 1
	}
	// 解码 UTF-8（输入已由终端保证为合法 UTF-8；非法字节按单字节丢弃）
	r, size := decodeRune(b)
	if size == 0 {
		return nil, 1
	}
	return KeyEv{Rune: r, Name: string(r)}, size
}

// parseCSI 解析 \x1b[ 开头的序列。
func parseCSI(b []byte) (Event, int) {
	// 找终字节
	i := 2
	for i < len(b) {
		c := b[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '~' || c == 'M' || c == 'm' {
			break
		}
		i++
	}
	if i >= len(b) {
		return nil, 0 // 不完整
	}
	final := b[i]
	params := string(b[2:i])
	n := i + 1

	switch final {
	case 'A':
		return KeyEv{Name: KeyUp}, n
	case 'B':
		return KeyEv{Name: KeyDown}, n
	case 'C':
		return KeyEv{Name: KeyRight}, n
	case 'D':
		return KeyEv{Name: KeyLeft}, n
	case 'H':
		return KeyEv{Name: KeyHome}, n
	case 'F':
		return KeyEv{Name: KeyEnd}, n
	case 'Z':
		return KeyEv{Name: "shift+tab"}, n
	case 'M', 'm':
		if strings.HasPrefix(params, "<") {
			return parseSGRMouse(params[1:], final), n
		}
		// X10 鼠标（\x1b[M 后跟 3 字节）——本程序启用 1006，不处理
		return nil, n
	case '~':
		switch strings.SplitN(params, ";", 2)[0] {
		case "1", "7":
			return KeyEv{Name: KeyHome}, n
		case "2":
			return KeyEv{Name: "insert"}, n
		case "3":
			return KeyEv{Name: KeyDelete}, n
		case "4", "8":
			return KeyEv{Name: KeyEnd}, n
		case "5":
			return KeyEv{Name: KeyPgUp}, n
		case "6":
			return KeyEv{Name: KeyPgDn}, n
		case "200":
			return parsePaste(b, n)
		}
		return nil, n
	case 'R':
		// 光标位置报告（我们不查询，忽略）
		return nil, n
	}
	return nil, n
}

// parsePaste 从 \x1b[200~ 起解析到 \x1b[201~ 结束；未闭合返回不完整。
func parsePaste(b []byte, used int) (Event, int) {
	end := strings.Index(string(b[used:]), "\x1b[201~")
	if end < 0 {
		return nil, 0
	}
	text := string(b[used : used+end])
	// 规范化粘贴文本中的换行，避免输入框多行错乱
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return PasteEv{Text: text}, used + end + len("\x1b[201~")
}

// parseSGRMouse 解析 SGR 鼠标参数。
func parseSGRMouse(params string, final byte) Event {
	parts := strings.SplitN(params, ";", 3)
	if len(parts) != 3 {
		return nil
	}
	bcode := atoi(parts[0])
	x := atoi(parts[1]) - 1
	y := atoi(parts[2]) - 1
	if bcode >= 64 { // 滚轮
		button := 0
		if bcode >= 65 {
			button = 1
		}
		return MouseEv{Type: "wheel", Button: button, X: x, Y: y}
	}
	typ := "press"
	if final == 'm' {
		typ = "release"
	} else if bcode >= 32 {
		typ = "motion"
		bcode &^= 32
	}
	return MouseEv{Type: typ, Button: bcode, X: x, Y: y}
}

func atoi(s string) int {
	v := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return v
		}
		v = v*10 + int(s[i]-'0')
	}
	return v
}

// decodeRune 解码 UTF-8 首 rune；非法返回 (0,0)。
func decodeRune(b []byte) (rune, int) {
	c := b[0]
	var size int
	switch {
	case c&0xE0 == 0xC0:
		size = 2
	case c&0xF0 == 0xE0:
		size = 3
	case c&0xF8 == 0xF0:
		size = 4
	default:
		return 0, 0
	}
	if len(b) < size {
		return 0, 0 // 不完整（正常不会发生：Read 已给完整块）
	}
	for i := 1; i < size; i++ {
		if b[i]&0xC0 != 0x80 {
			return 0, 0
		}
	}
	switch size {
	case 2:
		return rune(c&0x1F)<<6 | rune(b[1]&0x3F), 2
	case 3:
		return rune(c&0x0F)<<12 | rune(b[1]&0x3F)<<6 | rune(b[2]&0x3F), 3
	default:
		return rune(c&0x07)<<18 | rune(b[1]&0x3F)<<12 | rune(b[2]&0x3F)<<6 | rune(b[3]&0x3F), 4
	}
}
