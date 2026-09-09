// grf-computer：grf-harness 电脑操作插件（截屏/鼠标/键盘/剪贴板，
// 经操作系统自带命令实现，macOS / Linux / Windows）。
//
// 用法：
//
//	grf-computer                 stdio 模式（本地：grf-harness 直接 spawn）
//	grf-computer serve --addr 127.0.0.1:7611 [--token xxx] [--path /mcp]
//	                             远程 MCP 模式（Streamable HTTP）
//
// macOS 需在 系统设置 → 隐私与安全性 给宿主终端授权"辅助功能"（截屏另
// 需"屏幕录制"）；Linux 需 X11 + xdotool（截屏 scrot 等）；Windows 无额外
// 依赖。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"git.gorun.fun/share/grf-harness/plugins/internal/mcpserver"
)

// version 构建期经 -ldflags -X main.version 注入。
var version = "dev"

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "serve" {
		addr, token, path, err := mcpserver.ParseServeArgs(args[1:], "127.0.0.1:7611")
		if err != nil {
			fatal(err)
		}
		if err := newServer().ServeHTTP(context.Background(), addr, path, token); err != nil {
			fatal(err)
		}
		return
	}
	fs := flag.NewFlagSet("grf-computer", flag.ExitOnError)
	_ = fs.Parse(args)
	if err := newServer().ServeStdio(context.Background()); err != nil {
		fatal(err)
	}
}

func newServer() *mcpserver.Server {
	s := mcpserver.New("grf-computer", version)
	registerComputerTools(s, newPlatform())
	return s
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "grf-computer:", err)
	os.Exit(1)
}
