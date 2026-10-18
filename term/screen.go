// screen.go 全屏渲染器：进入备用屏幕（alt screen）、开 raw 模式、启用
// bracketed paste 与可选鼠标上报；整帧缓冲重画（光标归位逐行覆写），
// SIGWINCH 触发尺寸同步并通知上层按新尺寸重建整帧——保证终端宽度/
// 高度变化时内容按新宽度软换行重绘、无残留。
package term

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"sync"
)

// Screen 备用屏幕渲染器。
type Screen struct {
	mu     sync.Mutex
	out    *bufio.Writer
	file   *os.File
	raw    *Raw
	Width  int
	Height int

	mouse   bool
	enabled bool

	resizeCh   chan struct{}
	sigClose   chan struct{}
}

// NewScreen 创建面向 stdout 的全屏渲染器（尚未进入全屏）。
func NewScreen() *Screen {
	return &Screen{
		out:      bufio.NewWriterSize(os.Stdout, 256*1024),
		file:     os.Stdout,
		Width:    80,
		Height:   24,
		resizeCh: make(chan struct{}, 1),
		sigClose: make(chan struct{}),
	}
}

// ResizeCh 返回尺寸变化通知通道（合并触发；新尺寸读 Width/Height）。
func (s *Screen) ResizeCh() <-chan struct{} { return s.resizeCh }

// Start 进入全屏：raw 模式 + 备用屏幕 + paste + 清屏。
// mouse 为 true 时启用 SGR 鼠标上报（滚轮/点击/拖动/释放）。
func (s *Screen) Start(mouse bool) error {
	fd := int(s.file.Fd())
	raw, err := OpenRaw(fd)
	if err != nil {
		return err
	}
	s.raw = raw
	s.mouse = mouse
	s.enabled = true
	s.SyncSize()

	var b strings.Builder
	b.WriteString("\x1b[?1049h") // 备用屏幕
	b.WriteString("\x1b[?25l")   // 隐藏光标（渲染时按需再显示）
	b.WriteString("\x1b[2J\x1b[H")
	b.WriteString("\x1b[?2004h") // bracketed paste
	if mouse {
		b.WriteString("\x1b[?1002h\x1b[?1006h") // 鼠标 cell-motion + SGR
	}
	s.out.WriteString(b.String())
	s.out.Flush()

	// 尺寸变化监听 → 同步尺寸并通知重绘（unix 用 SIGWINCH，Windows 轮询
	// 控制台尺寸，其他平台无通知能力，见 resize_*.go）
	go watchResize(s.sigClose, func() {
		s.SyncSize()
		select {
		case s.resizeCh <- struct{}{}:
		default:
		}
	})
	return nil
}

// Stop 退出全屏并恢复终端。
func (s *Screen) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled {
		return
	}
	s.enabled = false
	close(s.sigClose)
	var b strings.Builder
	if s.mouse {
		b.WriteString("\x1b[?1002l\x1b[?1006l")
	}
	b.WriteString("\x1b[?2004l")
	b.WriteString("\x1b[?25h") // 恢复光标
	b.WriteString("\x1b[2J\x1b[H")
	b.WriteString("\x1b[?1049l") // 回主屏幕
	s.out.WriteString(b.String())
	s.out.Flush()
	s.raw.Close()
}

// SetMouse 开关鼠标上报（运行中切换）。
func (s *Screen) SetMouse(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled || on == s.mouse {
		return
	}
	s.mouse = on
	if on {
		s.out.WriteString("\x1b[?1002h\x1b[?1006h")
	} else {
		s.out.WriteString("\x1b[?1002l\x1b[?1006l")
	}
	s.out.Flush()
}

// SyncSize 查询并更新当前终端尺寸（查询失败保持现有值）。
func (s *Screen) SyncSize() {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, h := Size(int(s.file.Fd()))
	if w > 0 && h > 0 {
		s.Width, s.Height = w, h
	}
}

// Size 返回当前 (宽, 高)。
func (s *Screen) Size() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Width, s.Height
}

// Paint 整帧重画：lines 为已按 Width 软换行的帧内容（不超过 Height 行，
// 超出由调用方截断）。cursorRow/cursorCol 为光标显示位置（0 起显示坐标；
// cursorRow<0 表示隐藏光标）。整帧一次写入，避免撕裂。
func (s *Screen) Paint(lines []string, cursorRow, cursorCol int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled {
		return
	}
	var b strings.Builder
	b.WriteString("\x1b[H")
	h := s.Height
	for i := 0; i < h && i < len(lines); i++ {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString(lines[i])
		b.WriteString("\x1b[K") // 清到行尾
	}
	if len(lines) < h {
		b.WriteString("\x1b[J") // 清除下方残留
	}
	if cursorRow >= 0 && cursorRow < h {
		fmt.Fprintf(&b, "\x1b[%d;%dH", cursorRow+1, cursorCol+1)
		b.WriteString("\x1b[?25h")
	} else {
		b.WriteString("\x1b[?25l")
	}
	s.out.WriteString(b.String())
	s.out.Flush()
}
