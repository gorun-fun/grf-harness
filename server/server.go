// server.go gh server：把 harness 的 agent 能力以 OpenAI Responses API
// 形状暴露（ORS 2026-04-24 全量合规 + OpenAI 形状超集）。零第三方依赖，
// 纯 net/http。gh 特有能力走 grf: 冒号前缀扩展（ORS §11）。
//
// 安全模型：默认只绑环回地址；全部端点 Bearer 鉴权——这个服务执行 bash
// 与文件读写（在 <data>/conversations/<sid>/workspace 下），等于受控的
// 本机 RCE 入口，绑非环回地址前请先读 docs/server.md。
package server

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// ORS 错误类型 → HTTP 状态。
const (
	errInvalid  = "invalid_request"
	errNotFound = "not_found"
	errServer   = "server_error"
	errModel    = "model_error"
)

func statusOf(errType string) int {
	switch errType {
	case errInvalid:
		return 400
	case errNotFound:
		return 404
	default:
		return 500
	}
}

// 请求体字节上限：主端点（input 可携带多模态 base64 图像）64MB，
// 配置类小端点（settings/技能/policy）1MB。
const (
	bodyLimitMain  = 64 << 20
	bodyLimitSmall = 1 << 20
)

// decodeJSON 读取请求体并解析为 JSON：读取失败或解析失败时写好 400 错误
// 响应并返回 false（调用方直接 return）。limit 为请求体字节上限。
func decodeJSON(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, limit))
	if err != nil {
		writeError(w, 400, errInvalid, "read body: "+err.Error(), "")
		return false
	}
	if err := json.Unmarshal(body, v); err != nil {
		writeError(w, 400, errInvalid, "invalid JSON body: "+err.Error(), "")
		return false
	}
	return true
}

// apiError 带 HTTP 语义的内部错误。
type apiError struct {
	Type    string
	Code    string
	Message string
	Param   string
}

func (e *apiError) Error() string { return e.Type + ": " + e.Message }

func apiErr(typ, msg string) error { return &apiError{Type: typ, Message: msg} }

// writeError ORS 错误响应（{"error":{type,code,message,param}}）。
func writeError(w http.ResponseWriter, httpStatus int, typ, msg, param string) {
	if httpStatus == 0 {
		httpStatus = statusOf(typ)
	}
	writeJSON(w, httpStatus, errorBody{Error: ErrorPayload{Type: typ, Code: nil, Message: msg, Param: paramOrNil(param)}})
}

func paramOrNil(p string) any {
	if p == "" {
		return nil
	}
	return p
}

func writeJSONError(w http.ResponseWriter, err error) {
	ae, ok := err.(*apiError)
	if !ok {
		ae = &apiError{Type: errServer, Message: err.Error()}
	}
	writeError(w, statusOf(ae.Type), ae.Type, ae.Message, ae.Param)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Handler 路由（net/http 1.22+ 方法与路径参数模式）。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/responses", s.guard(s.handleCreate))
	mux.HandleFunc("POST /v1/responses/compact", s.guard(s.handleCompact))
	mux.HandleFunc("GET /v1/responses/{id}", s.guard(s.handleRetrieve))
	mux.HandleFunc("POST /v1/responses/{id}/cancel", s.guard(s.handleCancel))
	mux.HandleFunc("GET /v1/conversations", s.guard(s.handleListConversations))
	mux.HandleFunc("GET /v1/conversations/{id}", s.guard(s.handleGetConversation))
	mux.HandleFunc("PUT /v1/conversations/{id}/policy", s.guard(s.handleSetPolicy))
	mux.HandleFunc("DELETE /v1/conversations/{id}", s.guard(s.handleArchiveConversation))
	mux.HandleFunc("GET /v1/conversations/{id}/items", s.guard(s.handleConversationItems))
	// 智能体配置(网页设置页用;密钥掩码,空值=保留,见 agentapi.go)
	mux.HandleFunc("GET /v1/agent/models", s.guard(s.handleAgentModelsGet))
	mux.HandleFunc("PUT /v1/agent/models", s.guard(s.handleAgentModelsPut))
	mux.HandleFunc("POST /v1/agent/models/detect", s.guard(s.handleAgentModelsDetect))
	mux.HandleFunc("GET /v1/agent/plugins", s.guard(s.handleAgentPluginsGet))
	mux.HandleFunc("PUT /v1/agent/plugins", s.guard(s.handleAgentPluginsPut))
	mux.HandleFunc("GET /v1/agent/skills", s.guard(s.handleAgentSkillsGet))
	mux.HandleFunc("GET /v1/agent/skills/{name}", s.guard(s.handleAgentSkillGet))
	mux.HandleFunc("PUT /v1/agent/skills/{name}", s.guard(s.handleAgentSkillPut))
	mux.HandleFunc("DELETE /v1/agent/skills/{name}", s.guard(s.handleAgentSkillDelete))
	mux.HandleFunc("POST /v1/agent/skills/{name}/enabled", s.guard(s.handleAgentSkillEnabled))
	mux.HandleFunc("GET /v1/health", s.guard(s.handleHealth))
	return accessLog(mux)
}

// accessLog 访问日志中间件：每个请求一行（方法/路径/状态/耗时/来源）
// 写 stderr——gh server 前台进程可直接看到，多用户/嵌入场景排障的基本
// 观察面。SSE 流式请求在流结束后才记一行（耗时含整个流时长）。
// GRF_SERVER_ACCESS_LOG=off 关闭。
func accessLog(next http.Handler) http.Handler {
	if strings.EqualFold(os.Getenv("GRF_SERVER_ACCESS_LOG"), "off") {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(rw, r)
		log.Printf("server: %s %s %d %s %s", r.Method, r.URL.Path, rw.status,
			time.Since(start).Round(time.Millisecond), r.RemoteAddr)
	})
}

// statusWriter 捕获响应状态码的包装。Flush 必须显式委托——SSE 处理器
// 经 http.Flusher 断言刷新，包装后不实现会把流式响应退化成缓冲到结束。
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ListenAndServe 阻塞监听（main 的 server 子命令入口）。
func (s *Server) ListenAndServe(addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv.ListenAndServe()
}

// guard 鉴权中间件：校验 Bearer 令牌。比较用常数时间（subtle.
// ConstantTimeCompare），防逐字节短路比较的时序侧信道；长度不等先短路
// ——ConstantTimeCompare 对不等长输入本就恒返 0，长度本身不是秘密，
// 先比长度只是省一次内容比较，401 行为不变。
func (s *Server) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		want := "Bearer " + s.token
		auth := r.Header.Get("Authorization")
		if len(auth) != len(want) || subtle.ConstantTimeCompare([]byte(auth), []byte(want)) != 1 {
			writeError(w, 401, errInvalid, "missing or invalid bearer token", "")
			return
		}
		h(w, r)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{"status": "ok"})
}
