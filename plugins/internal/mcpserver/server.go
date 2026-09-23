// Package mcpserver 是 grf 插件的 MCP 服务端 SDK：工具实现一次注册，
// 即可以两种模式对外服务——
//
//	stdio 模式（默认）：行分隔 JSON-RPC 走 stdin/stdout，由 grf-harness
//	直接 spawn（本地模式）；
//	serve 模式：`插件 serve --addr :7610` 起一个 MCP Streamable HTTP
//	端点，远端 grf-harness 经 settings.json 的 plugins.url 接入。
//
// 仅覆盖 harness 所需子集：initialize / tools/list / tools/call / ping，
// 通知一律不回复。协议版本 2024-11-05 ~ 2025-06-18 均可协商。
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// ProtocolVersion 本 SDK 实现的 MCP 协议版本。
const ProtocolVersion = "2025-06-18"

// knownVersions 可协商接受的客户端版本。
var knownVersions = map[string]bool{
	"2024-11-05": true,
	"2025-03-26": true,
	"2025-06-18": true,
}

// Content 工具返回内容项（text / image）。
type Content struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Data     string `json:"data,omitempty"` // base64（image）
}

// Text 纯文本内容。
func Text(s string) Content { return Content{Type: "text", Text: s} }

// ImagePNG PNG 图片内容（base64 编码）。
func ImagePNG(b64 string) Content {
	return Content{Type: "image", MimeType: "image/png", Data: b64}
}

// Tool 一个 MCP 工具定义与处理器。ReadOnly 标注会随 annotations 下发，
// grf-harness 据此在计划模式放行（未标注的一律按改动类拦截）。
type Tool struct {
	Name        string
	Description string
	Schema      map[string]any // JSON Schema（inputSchema）
	ReadOnly    bool
	Handler     func(ctx context.Context, args json.RawMessage) ([]Content, error)
}

// Server MCP 服务端（单进程内全部工具）。
type Server struct {
	name    string
	version string
	mu      sync.Mutex
	tools   map[string]Tool
}

// New 构建服务端（name/version 用于 initialize 应答的 serverInfo）。
func New(name, version string) *Server {
	return &Server{name: name, version: version, tools: map[string]Tool{}}
}

// Add 注册工具（重名覆盖，启动期 panic 由插件自担）。
func (s *Server) Add(t Tool) {
	if t.Schema == nil {
		t.Schema = map[string]any{"type": "object"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools[t.Name] = t
}

// Names 已注册工具名（字典序）。
func (s *Server) Names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.tools))
	for n := range s.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Tools 已注册工具的快照（嵌入方测试直调 handler 用）。
func (s *Server) Tools() []Tool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Tool, 0, len(s.tools))
	for _, t := range s.tools {
		out = append(out, t)
	}
	return out
}

// ---- JSON-RPC 信封 ----

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// toolDef tools/list 的条目。
type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`
}

// Dispatch 处理一条 JSON-RPC 消息：返回应答字节（通知无应答返回 nil）。
func (s *Server) Dispatch(ctx context.Context, raw []byte) []byte {
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return mustJSON(rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error: " + err.Error()}})
	}
	if len(req.ID) == 0 || string(req.ID) == "null" {
		return nil // 通知：无应答
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := ProtocolVersion
		if knownVersions[p.ProtocolVersion] {
			version = p.ProtocolVersion // 客户端版本已知则回显，协商一致
		}
		resp.Result = map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": s.name, "version": s.version},
		}
	case "ping":
		// 空对象必须出现在 result 字段（JSON-RPC 要求 result/error 必有其一，
		// 而 omitempty 会把空 map 整个省掉，故用 RawMessage 强制写出 {}）
		resp.Result = json.RawMessage("{}")
	case "tools/list":
		s.mu.Lock()
		defs := make([]toolDef, 0, len(s.tools))
		for _, t := range s.tools {
			def := toolDef{Name: t.Name, Description: t.Description, InputSchema: t.Schema}
			if t.ReadOnly {
				def.Annotations = map[string]any{"readOnlyHint": true}
			}
			defs = append(defs, def)
		}
		s.mu.Unlock()
		sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
		resp.Result = map[string]any{"tools": defs}
	case "tools/call":
		resp.Result = s.callTool(ctx, req.Params)
	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return mustJSON(resp)
}

// callTool 执行 tools/call：工具级失败走 isError 内容（协议层正常），
// 处理器 panic 同样收敛为 isError，绝不让插件进程崩掉整个连接。
func (s *Server) callTool(ctx context.Context, params json.RawMessage) any {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return map[string]any{"content": []Content{Text("invalid params: " + err.Error())}, "isError": true}
	}
	if len(p.Arguments) == 0 {
		p.Arguments = json.RawMessage("{}") // 无参工具可省略 arguments，补空对象避免下游 Unmarshal 报错
	}
	s.mu.Lock()
	t, ok := s.tools[p.Name]
	s.mu.Unlock()
	if !ok {
		return map[string]any{"content": []Content{Text(fmt.Sprintf("unknown tool %q (available: %v)", p.Name, s.Names()))}, "isError": true}
	}
	contents, err := runTool(ctx, t, p.Arguments)
	if err != nil {
		return map[string]any{"content": []Content{Text("error: " + err.Error())}, "isError": true}
	}
	if contents == nil {
		contents = []Content{Text("(no output)")}
	}
	return map[string]any{"content": contents}
}

func runTool(ctx context.Context, t Tool, args json.RawMessage) (contents []Content, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			contents, err = nil, fmt.Errorf("tool %q panicked: %v", t.Name, rec)
		}
	}()
	return t.Handler(ctx, args)
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"jsonrpc":"2.0","error":{"code":-32603,"message":"internal marshal error"}}`)
	}
	return b
}
