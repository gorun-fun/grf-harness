// server_cmd.go gh server 子命令：启动 Responses API 接入服务（ORS 兼容）。
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
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
//	-tls-cert <file>    TLS 证书 PEM（与 -tls-key 成对启用 HTTPS；集群
//	                    跨网部署建议 TLS + cluster.nodes[].pin 指纹锁定）
//	-tls-key <file>     TLS 私钥 PEM
func runServerCmd(args []string) {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:7400", "监听地址(默认仅环回)")
	data := fs.String("data", "", "数据目录(默认 ~/.grf/server)")
	token := fs.String("token", "", "Bearer 令牌(默认读/生成 <data>/token)")
	tlsCert := fs.String("tls-cert", "", "TLS 证书 PEM(启用 HTTPS)")
	tlsKey := fs.String("tls-key", "", "TLS 私钥 PEM(启用 HTTPS)")
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "用法: gh server [-addr 127.0.0.1:7400] [-data 目录] [-token 令牌] [-tls-cert 证书 -tls-key 私钥]")
		os.Exit(2)
	}
	if (*tlsCert == "") != (*tlsKey == "") {
		fatal(fmt.Errorf("-tls-cert 与 -tls-key 必须成对使用"))
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
	// 注册/心跳 + 收件箱指令通道（随 ctx 退出停止）。须在 Handler() 构建
	// 之前（路由表按启用状态装配）。
	if cfg, err := config.LoadFileForUpdate(); err == nil {
		if ccfg := config.ClusterRuntime(cfg); ccfg != nil {
			router := cluster.NewRouter(ccfg)
			srv.EnableCluster(router, ccfg)
			role := ccfg.Role
			if role == "" {
				role = "node"
			}
			sync := cluster.SyncMode(ccfg.Sync)
			if sync == "" {
				sync = "off"
			}
			fmt.Printf("   集群: 已启用（角色 %s，节点表 %d 个，同步 %s）\n", role, len(ccfg.Nodes), sync)
			if ccfg.CommanderURL != "" {
				self, _ := cluster.LoadIdentity()
				if ccfg.Name != "" {
					self.Name = ccfg.Name // settings 显式名 > cluster.json/hostname（与能力清单同名）
				}
				go cluster.CallHome(ctx, ccfg.CommanderURL, ccfg.Token, ccfg.InviteCode, self, srv.ClusterCaps, srv.RunningTurns)
				go cluster.InboxLoop(ctx, ccfg.CommanderURL, ccfg.Token, self, srv.ExecuteClusterCommand)
				fmt.Printf("   集群: call-home 指挥官 %s（节点名 %s，指令收件箱中继已挂）\n", ccfg.CommanderURL, self.Name)
			}
			// mDNS 局域网发现（cluster.discover = "lan"）：双向通告；指挥官
			// 侧浏览并入成员表（能力清单尽力拉取——token 不同/不可达即只有
			// 存活性，无能力）。组播不可用只降级告警（其余通道不受影响）。
			if strings.EqualFold(strings.TrimSpace(ccfg.Discover), "lan") {
				port := serverPortOf(*addr)
				browse := ccfg.IsCommander()
				go func() {
					if err := cluster.RunDiscovery(ctx, router.Self(), port, browse, func(p cluster.Peer) {
						router.Table().Announce(p.ID, p.Name, p.Addr)
						if caps, err := cluster.NewNodeClient(p.Addr, ccfg.Token).Capabilities(context.Background()); err == nil {
							router.Table().SetCaps(p.Name, caps)
						}
					}); err != nil {
						fmt.Fprintf(os.Stderr, "   集群: mDNS 发现不可用（降级为静态表 + call-home）: %v\n", err)
					}
				}()
				fmt.Printf("   集群: mDNS 局域网发现已启用（%s）\n", map[bool]string{true: "通告 + 浏览", false: "通告"}[browse])
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
	scheme := "http"
	listen := func() error { return httpSrv.ListenAndServe() }
	if *tlsCert != "" {
		scheme = "https"
		cert, key := *tlsCert, *tlsKey
		listen = func() error { return httpSrv.ListenAndServeTLS(cert, key) }
	}
	fmt.Printf("🚀 gh server 已启动: %s://%s/v1/responses\n", scheme, *addr)
	fmt.Printf("   令牌: %s\n", srv.Token())
	fmt.Printf("   数据: %s(会话/存档/快照)\n", srv.DataDir())
	fmt.Printf("   接入: OpenAI Responses 形状,ORS 2026-04-24 合规;gh 扩展见 docs/server.md\n")
	if err := listen(); err != nil && err != http.ErrServerClosed {
		fatal(err)
	}
	// 停止接客后等在跑回合落定（含 background 回合——其 ctx 不随监听
	// 退出取消，不等会把执行中的回合连同终态快照一起带走，客户端只能
	// 从 retrieve 的"蒸发 404"推断）。
	srv.WaitSettled(30 * time.Second)
	fmt.Println("gh server 已停止")
}

// serverPortOf 监听地址 → 端口号（mDNS SRV 记录用；解析失败 7400 兜底）。
func serverPortOf(addr string) int {
	if _, port, err := net.SplitHostPort(addr); err == nil {
		if n, err := strconv.Atoi(port); err == nil && n > 0 && n < 65536 {
			return n
		}
	}
	return 7400
}
