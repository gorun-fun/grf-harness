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

	"git.gorun.fun/share/grf-harness/cluster"
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
	// 集群装配（settings cluster.enabled / GRF_CLUSTER_ENABLED）：
	// 路由层 + 控制面端点（任何角色——capabilities 恒可用，注册/心跳/
	// 成员表在指挥官侧生效）。节点角色配了 commander_url 即 call-home
	// 注册/心跳（随 ctx 退出停止）。须在 Handler() 构建之前（路由表按
	// 启用状态装配）。
	if cfg, err := config.LoadFileForUpdate(); err == nil {
		if ccfg := config.ClusterRuntime(cfg); ccfg != nil {
			router := cluster.NewRouter(ccfg)
			srv.EnableCluster(router, ccfg)
			role := ccfg.Role
			if role == "" {
				role = "node"
			}
			fmt.Printf("   集群: 已启用（角色 %s，节点表 %d 个）\n", role, len(ccfg.Nodes))
			if ccfg.CommanderURL != "" {
				self, _ := cluster.LoadIdentity()
				if ccfg.Name != "" {
					self.Name = ccfg.Name // settings 显式名 > cluster.json/hostname（与能力清单同名）
				}
				go cluster.CallHome(ctx, ccfg.CommanderURL, ccfg.Token, self, srv.ClusterCaps, srv.RunningTurns)
				fmt.Printf("   集群: call-home 指挥官 %s（节点名 %s）\n", ccfg.CommanderURL, self.Name)
			}
		}
	}
	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Minute, // 慢速滴漏请求体兜底；SSE 不可设 WriteTimeout
		IdleTimeout:       2 * time.Minute,
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
	// 停止接客后等在跑回合落定（含 background 回合——其 ctx 不随监听
	// 退出取消，不等会把执行中的回合连同终态快照一起带走，客户端只能
	// 从 retrieve 的"蒸发 404"推断）。
	srv.WaitSettled(30 * time.Second)
	fmt.Println("gh server 已停止")
}
