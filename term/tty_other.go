//go:build !(darwin || linux)

package term

import (
	"errors"
	"os"
)

// 非 unix 平台的回退实现：无 raw 模式，尺寸固定 80x24（全屏交互不可用，
// 上层应回退纯文本模式）。

func Size(fd int) (int, int) { return 80, 24 }

func IsTTY(fd int) bool { return false }

// resizeSignal 非 unix 平台无窗口尺寸变化信号（Screen 的重绘监听退化为
// 仅等待关闭）。
func resizeSignal() os.Signal { return nil }

type Raw struct{}

func OpenRaw(fd int) (*Raw, error) { return nil, errors.New("raw mode unsupported on this platform") }
func (r *Raw) Close()              {}

func StdoutSize() (int, int) { return 80, 24 }
