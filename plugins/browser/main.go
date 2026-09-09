// grf-browser：grf-harness 浏览器操作插件（chromedp 纯 Go 实现，工具面
// 参考 playwright-mcp：snapshot → ref → 点击/输入）。
//
// 用法：
//
//	grf-browser                 stdio 模式（本地：grf-harness 直接 spawn）
//	grf-browser --headless=false stdio 模式（带浏览器界面）
//	grf-browser serve --addr 127.0.0.1:7610 [--token xxx] [--path /mcp]
//	                            远程 MCP 模式（Streamable HTTP，远端
//	                            grf-harness 经 settings.json 的 plugins.url 接入）
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"git.gorun.fun/share/grf-harness/plugins/internal/mcpserver"
)

// version 构建期经 -ldflags -X main.version 注入。
var version = "dev"

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "serve" {
		addr, token, path, err := mcpserver.ParseServeArgs(args[1:], "127.0.0.1:7610")
		if err != nil {
			fatal(err)
		}
		b := newBrowser(true, "")
		s := mcpserver.New("grf-browser", version)
		registerBrowserTools(s, b)
		watchSignals(b)
		defer b.shutdown()
		if err := s.ServeHTTP(context.Background(), addr, path, token); err != nil {
			fatal(err)
		}
		return
	}
	fs := flag.NewFlagSet("grf-browser", flag.ExitOnError)
	headless := fs.Bool("headless", true, "无头模式（--headless=false 带浏览器界面）")
	chromePath := fs.String("chrome", "", "chrome 可执行文件路径（缺省自动探测）")
	_ = fs.Parse(args)
	s, b := newServer(*headless, *chromePath)
	// 进程退出（stdin EOF 或信号）时必须显式关闭浏览器，否则 chrome
	// 成为孤儿进程继续驻留
	watchSignals(b)
	defer b.shutdown()
	if err := s.ServeStdio(context.Background()); err != nil {
		fatal(err)
	}
}

// newServer 组装 MCP 服务端与浏览器实例（浏览器懒启动：首个工具调用才
// 拉起 chrome）。返回 browser 供退出路径 shutdown。
func newServer(headless bool, chromePath string) (*mcpserver.Server, *browser) {
	b := newBrowser(headless, chromePath)
	s := mcpserver.New("grf-browser", version)
	registerBrowserTools(s, b)
	return s, b
}

// watchSignals SIGINT/SIGTERM 时先关浏览器再退出（默认信号杀进程不会
// 触发 defer，浏览器连接不会清理）。
func watchSignals(b *browser) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		b.shutdown()
		os.Exit(0)
	}()
	_ = stop
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "grf-browser:", err)
	os.Exit(1)
}
