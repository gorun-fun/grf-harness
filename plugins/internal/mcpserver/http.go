// http.go 远程模式：MCP Streamable HTTP——POST 单条 JSON-RPC，应答为
// application/json（通知 202 无体）。支持 Bearer token 鉴权与宽松 CORS
// （便于接入 Web 类 MCP 客户端）。
package mcpserver

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// ServeHTTP 在 addr 上起 Streamable HTTP 端点（path 默认 /mcp）；token
// 非空时校验 Authorization: Bearer。阻塞直至 ListenAndServe 出错。
func (s *Server) ServeHTTP(ctx context.Context, addr, path, token string) error {
	if path == "" {
		path = "/mcp"
	}
	mux := http.NewServeMux()
	mux.Handle(path, s.handler(token, path))
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	log.Printf("[mcp] %s 监听 http://%s%s（远程模式；token=%t）", s.name, addr, path, token != "")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Handler 暴露 http.Handler（测试与自定义装配用）。
func (s *Server) Handler(token string) http.Handler {
	return s.handler(token, "/mcp")
}

func (s *Server) handler(token, path string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Mcp-Session-Id, MCP-Protocol-Version")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		switch r.Method {
		case http.MethodOptions:
			w.WriteHeader(http.StatusNoContent)
			return
		case http.MethodPost:
			// 其余方法继续
		default:
			http.Error(w, "仅支持 POST（Streamable HTTP）；SSE 长连接端点未提供", http.StatusMethodNotAllowed)
			return
		}
		if token != "" {
			auth := r.Header.Get("Authorization")
			// 常量时间比较，避免逐字节泄露 token
			if subtle.ConstantTimeCompare([]byte(auth), []byte("Bearer "+token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}
		resp := s.Dispatch(r.Context(), body)
		if resp == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(resp)
	})
}

// ParseServeArgs 解析 serve 子命令参数：--addr / --token / --path。
// args 为去掉 "serve" 后的剩余参数。
func ParseServeArgs(args []string, defaultAddr string) (addr, token, path string, err error) {
	addr, token, path = defaultAddr, "", "/mcp"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--addr", "-addr":
			i++
			if i >= len(args) {
				return "", "", "", fmt.Errorf("--addr 缺少值")
			}
			addr = args[i]
		case "--token", "-token":
			i++
			if i >= len(args) {
				return "", "", "", fmt.Errorf("--token 缺少值")
			}
			token = args[i]
		case "--path", "-path":
			i++
			if i >= len(args) {
				return "", "", "", fmt.Errorf("--path 缺少值")
			}
			path = args[i]
			if !strings.HasPrefix(path, "/") {
				path = "/" + path
			}
		default:
			return "", "", "", fmt.Errorf("未知参数 %q（serve 用法: serve --addr 127.0.0.1:7610 --token xxx）", args[i])
		}
	}
	return addr, token, path, nil
}
