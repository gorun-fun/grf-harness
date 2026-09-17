// other.go 非 macOS/Linux/Windows 平台：全部操作返回不支持。
package main

import (
	"context"
	"fmt"
)

type unsupportedPlatform struct{}

func (unsupportedPlatform) Move(context.Context, int, int) error { return errUnsupported }
func (unsupportedPlatform) Click(context.Context, int, int, string, int) error {
	return errUnsupported
}
func (unsupportedPlatform) Drag(context.Context, int, int, int, int) error { return errUnsupported }
func (unsupportedPlatform) Scroll(context.Context, int, int, bool, string, int) error {
	return errUnsupported
}
func (unsupportedPlatform) Type(context.Context, string) error { return errUnsupported }
func (unsupportedPlatform) Key(context.Context, string) error  { return errUnsupported }
func (unsupportedPlatform) ScreenSize(context.Context, *int) (int, int, error) {
	return 0, 0, errUnsupported
}
func (unsupportedPlatform) Screenshot(context.Context, *int) ([]byte, error) {
	return nil, errUnsupported
}
func (unsupportedPlatform) Displays(context.Context) ([]Display, error) {
	return nil, errUnsupported
}
func (unsupportedPlatform) ClipboardGet(context.Context) (string, error) {
	return "", errUnsupported
}
func (unsupportedPlatform) ClipboardSet(context.Context, string) error { return errUnsupported }

var errUnsupported = fmt.Errorf("grf-computer 暂不支持该平台（支持 macOS / Linux / Windows）")
