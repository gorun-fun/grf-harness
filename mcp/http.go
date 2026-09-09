// http.go 远程插件传输：MCP Streamable HTTP——POST 单条 JSON-RPC 到端点，
// 应答为 application/json（单条）或 text/event-stream（逐事件扫描，取与本
// 请求 id 匹配的响应，通知丢弃）。支持 Bearer token 鉴权。
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

type httpTransport struct {
	endpoint string
	token    string
	hc       *http.Client
	nextID   int64
}

// DialHTTP 连接远程 MCP 端点（Streamable HTTP）并完成握手。token 非空时
// 以 Authorization: Bearer 携带。
func DialHTTP(ctx context.Context, endpoint, token string) (*Client, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("远程插件 URL 为空")
	}
	t := &httpTransport{
		endpoint: endpoint,
		token:    token,
		hc:       &http.Client{Timeout: 60 * time.Second},
	}
	return dial(ctx, t)
}

func (t *httpTransport) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := atomic.AddInt64(&t.nextID, 1)
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", ProtocolVersion)
	if t.token != "" {
		req.Header.Set("Authorization", "Bearer "+t.token)
	}
	resp, err := t.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 %s: %w", t.endpoint, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusAccepted:
		return nil, fmt.Errorf("mcp: 服务端对请求 %s 返回 202（未应答）", method)
	case resp.StatusCode != http.StatusOK:
		snippet := readSnippet(resp.Body, 512)
		return nil, fmt.Errorf("mcp: HTTP %d %s", resp.StatusCode, snippet)
	}
	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "text/event-stream") {
		return t.readSSE(resp.Body, id)
	}
	var r rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("mcp: 解析应答: %w", err)
	}
	if r.Error != nil {
		return nil, r.Error
	}
	return r.Result, nil
}

// readSSE 扫描事件流：事件数据按行聚合（data: 前缀），每个事件尝试解析
// 为 JSON-RPC 应答，匹配本请求 id 即返回；其余（通知/其他请求）丢弃。
func (t *httpTransport) readSSE(r io.Reader, id int64) (json.RawMessage, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	var dataLines []string
	flush := func() (json.RawMessage, bool, error) {
		defer func() { dataLines = nil }()
		if len(dataLines) == 0 {
			return nil, false, nil
		}
		payload := strings.Join(dataLines, "\n")
		var resp rpcResponse
		if err := json.Unmarshal([]byte(payload), &resp); err != nil {
			return nil, false, nil // 非协议内容的事件，跳过
		}
		if resp.Method != "" || resp.ID != id {
			return nil, false, nil
		}
		if resp.Error != nil {
			return nil, false, resp.Error
		}
		return resp.Result, true, nil
	}
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		case line == "":
			if res, ok, e := flush(); ok || e != nil {
				return res, e
			}
		}
		if err != nil {
			// EOF 前最后一段未以空行结尾的残余也尝试冲刷
			if res, ok, e := flush(); ok || e != nil {
				return res, e
			}
			return nil, fmt.Errorf("mcp: SSE 流在收到响应前结束")
		}
	}
}

func (t *httpTransport) Notify(ctx context.Context, method string, params any) error {
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if t.token != "" {
		req.Header.Set("Authorization", "Bearer "+t.token)
	}
	resp, err := t.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("mcp: 通知 %s 被拒绝（HTTP %d）", method, resp.StatusCode)
	}
	return nil
}

func (t *httpTransport) Close() error { return nil } // 无状态，HTTP 空闲连接自动复用

// readSnippet 限量读取一段 body 用于错误信息。
func readSnippet(r io.Reader, n int) string {
	buf := make([]byte, n)
	m, _ := io.ReadFull(r, buf)
	if m <= 0 {
		return ""
	}
	return strings.TrimSpace(string(buf[:m]))
}
