//go:build !(darwin || linux || windows)

package term

// watchResize 其他平台无尺寸变化通知能力，仅等待关闭。
func watchResize(closeCh <-chan struct{}, fire func()) {
	<-closeCh
}
