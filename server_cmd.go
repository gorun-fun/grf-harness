// server_cmd.go gh server 子命令：启动 Responses API 接入服务（ORS 兼容）。
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/server"
)

// runServerCmd 可选项：
//
//	-addr <host:port>   监听地址（默认 127.0.0.1:7400；服务会执行 bash 与
//	                    文件工具，绑非环回地址等于暴露本机 RCE，谨慎）
//	-data <dir>         数据目录（默认 ~/.grf/server）
//	-token <token>      Bearer 令牌（默认读取/自动生成 <data>/token）
func runServerCmd(args []string) {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:7400", "监听地址(默认仅环回)")
	data := fs.String("data", "", "数据目录(默认 ~/.grf/server)")
	token := fs.String("token", "", "Bearer 令牌(默认读/生成 <data>/token)")
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "用法: gh server [-addr 127.0.0.1:7400] [-data 目录] [-token 令牌]")
		os.Exit(2)
	}
	if *data == "" {
		*data = filepath.Join(config.Home(), "server")
	}
	srv, err := server.New(*data, *token)
	if err != nil {
		fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()
	fmt.Printf("🚀 gh server 已启动: http://%s/v1/responses\n", *addr)
	fmt.Printf("   令牌: %s\n", srv.Token())
	fmt.Printf("   数据: %s(会话/存档/快照)\n", srv.DataDir())
	fmt.Printf("   接入: OpenAI Responses 形状,ORS 2026-04-24 合规;gh 扩展见 docs/server.md\n")
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fatal(err)
	}
	fmt.Println("gh server 已停止")
}
