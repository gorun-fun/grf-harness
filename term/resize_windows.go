//go:build windows

package term

import (
	"syscall"
	"time"
)

// watchResize Windows 无 SIGWINCH 等价信号，轮询控制台可见窗口尺寸，
// 变化时回调 fire。250ms 间隔对手动拖拽窗口足够跟手。
func watchResize(closeCh <-chan struct{}, fire func()) {
	out, err := syscall.GetStdHandle(syscall.STD_OUTPUT_HANDLE)
	if err != nil || out == 0 {
		<-closeCh
		return
	}
	lastW, lastH := sizeHandle(out)
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if w, h := sizeHandle(out); w != lastW || h != lastH {
				lastW, lastH = w, h
				fire()
			}
		case <-closeCh:
			return
		}
	}
}
