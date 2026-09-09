// Package mcp 实现轻量 MCP（Model Context Protocol）客户端：把外部插件
// 进程（本地 stdio 模式）或远程 MCP 端点（Streamable HTTP 模式）提供的
// 工具接入 harness。仅覆盖协议子集：initialize 握手、tools/list、
// tools/call、ping；服务端通知与反向请求一律接收即弃（v1 不做
// sampling/roots 等客户端能力）。零第三方依赖。
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
)

// ProtocolVersion 本客户端声明并优先协商的 MCP 协议版本。
const ProtocolVersion = "2025-06-18"

// knownVersions 服务端可能回落的已知版本（握手时全部接受）。
var knownVersions = map[string]bool{
	"2024-11-05": true,
	"2025-03-26": true,
	"2025-06-18": true,
}

// Tool MCP 工具定义（tools/list 返回项；inputSchema 为标准 JSON Schema）。
type Tool struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	InputSchema map[string]any   `json:"inputSchema,omitempty"`
	Annotations *ToolAnnotations `json:"annotations,omitempty"`
}

// ToolAnnotations 工具注解：harness 用 ReadOnlyHint 判定计划模式是否放行。
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    bool   `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
}

// Content 工具返回的内容项（text / image；其余类型按文本占位处理）。
type Content struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Data     string `json:"data,omitempty"` // base64（image 类型）
}

// CallResult tools/call 的返回。IsError 为 true 表示工具自身执行失败
// （协议层正常），文本随 Content 回传。
type CallResult struct {
	Content []Content `json:"content,omitempty"`
	IsError bool      `json:"isError,omitempty"`
}

// ServerInfo 服务端自述（initialize 握手返回）。
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// InitializeResult initialize 握手结果（保留服务端确认的协议版本）。
type InitializeResult struct {
	ProtocolVersion string     `json:"protocolVersion"`
	ServerInfo      ServerInfo `json:"serverInfo"`
}

// ---- JSON-RPC 信封（内部） ----

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	s := fmt.Sprintf("mcp: jsonrpc error %d: %s", e.Code, e.Message)
	if len(e.Data) > 0 {
		s += " (" + string(e.Data) + ")"
	}
	return s
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
	// Method 非空表示这是服务端发出的请求/通知（v1 一律忽略）。
	Method string `json:"method,omitempty"`
}

// ---- 请求参数/响应载荷 ----

type initializeParams struct {
	ProtocolVersion string     `json:"protocolVersion"`
	Capabilities    struct{}   `json:"capabilities"`
	ClientInfo      ServerInfo `json:"clientInfo"`
}

type listToolsParams struct {
	Cursor string `json:"cursor,omitempty"`
}

type listToolsResult struct {
	Tools      []Tool `json:"tools"`
	NextCursor string `json:"nextCursor,omitempty"`
}

type callToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// Client 一条 MCP 连接（stdio 子进程或远程 HTTP 端点），握手后可用。
type Client struct {
	tr   transport
	info InitializeResult
}

// transport 底层传输：发请求等响应 / 发通知（通知不等应答）。
type transport interface {
	Call(ctx context.Context, method string, params any) (json.RawMessage, error)
	Notify(ctx context.Context, method string, params any) error
	Close() error
}

// dial 启动传输并完成 initialize 握手 + initialized 通知。
func dial(ctx context.Context, tr transport) (*Client, error) {
	c := &Client{tr: tr}
	params := initializeParams{ProtocolVersion: ProtocolVersion}
	params.ClientInfo = ServerInfo{Name: "grf-harness", Version: "1.0"}
	var res InitializeResult
	if err := callInto(ctx, tr, "initialize", params, &res); err != nil {
		_ = tr.Close()
		return nil, fmt.Errorf("initialize 握手失败: %w", err)
	}
	if res.ProtocolVersion != "" && !knownVersions[res.ProtocolVersion] {
		_ = tr.Close()
		return nil, fmt.Errorf("initialize 握手失败: 服务端协议版本 %q 不受支持", res.ProtocolVersion)
	}
	c.info = res
	_ = tr.Notify(ctx, "notifications/initialized", nil)
	return c, nil
}

// callInto 发请求并把 result 解到 v。
func callInto(ctx context.Context, tr transport, method string, params any, v any) error {
	raw, err := tr.Call(ctx, method, params)
	if err != nil {
		return err
	}
	if v == nil {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("解析 %s 结果: %w", method, err)
	}
	return nil
}

// ListTools 列出服务端全部工具（自动翻页）。
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var out []Tool
	cursor := ""
	for {
		var res listToolsResult
		if err := callInto(ctx, c.tr, "tools/list", listToolsParams{Cursor: cursor}, &res); err != nil {
			return out, err
		}
		out = append(out, res.Tools...)
		if res.NextCursor == "" {
			return out, nil
		}
		cursor = res.NextCursor
	}
}

// CallTool 调用服务端工具。协议错误返回 error；工具自身失败（isError）
// 以 CallResult.IsError 标记、文本在 Content 中。
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (CallResult, error) {
	var res CallResult
	if err := callInto(ctx, c.tr, "tools/call", callToolParams{Name: name, Arguments: args}, &res); err != nil {
		return CallResult{}, err
	}
	return res, nil
}

// ServerInfo 服务端自述。
func (c *Client) ServerInfo() ServerInfo { return c.info.ServerInfo }

// Close 关闭连接（stdio 模式会终止子进程；HTTP 模式释放空闲连接）。
func (c *Client) Close() error { return c.tr.Close() }
