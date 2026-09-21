//go:build darwin || linux

package term

import (
	"os"
	"os/signal"
	"syscall"
)

// watchResize SIGWINCH 驱动的尺寸变化监听，closeCh 关闭即退出。
func watchResize(closeCh <-chan struct{}, fire func()) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGWINCH)
	defer signal.Stop(sig)
	for {
		select {
		case <-sig:
			fire()
		case <-closeCh:
			return
		}
	}
}
