// client_test.go MCP 客户端测试：stdio 桩服务用测试二进制自复执行
// （TestHelperProcess 模式，无需外部依赖）；HTTP 覆盖 application/json
// 与 text/event-stream 两种应答形态。
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

const helperEnv = "GRF_MCP_TEST_HELPER"

func testCtx(t *testing.T) (context.Context, func()) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	return ctx, cancel
}

func TestStdioHandshakeAndCalls(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := testCtx(t)
	defer cancel()

	c, err := DialStdio(ctx, []string{exe, "-test.run=TestHelperProcess", "--"}, []string{helperEnv + "=1"})
	if err != nil {
		t.Fatalf("拨号: %v", err)
	}
	defer c.Close()

	if got := c.ServerInfo().Name; got != "stub" {
		t.Fatalf("serverInfo.name = %q, want stub", got)
	}
	tools, err := c.ListTools(ctx)
	if err != nil || len(tools) != 2 {
		t.Fatalf("ListTools = %v, %v", tools, err)
	}
	if tools[0].Name != "echo" || !tools[0].Annotations.ReadOnlyHint {
		t.Fatalf("echo 定义不符: %+v", tools[0])
	}

	res, err := c.CallTool(ctx, "echo", json.RawMessage(`{"msg":"hi"}`))
	if err != nil || res.IsError || len(res.Content) != 1 || res.Content[0].Text != "echo: hi" {
		t.Fatalf("CallTool echo = %+v, %v", res, err)
	}
	res, err = c.CallTool(ctx, "boom", nil)
	if err != nil || !res.IsError || res.Content[0].Text != "boom" {
		t.Fatalf("CallTool boom = %+v, %v（isError 应为工具级失败而非传输错误）", res, err)
	}
	if _, err := c.CallTool(ctx, "nope", nil); err == nil {
		t.Fatal("未知工具应返回 JSON-RPC 错误")
	}
}

// TestHelperProcess 以桩 MCP stdio 服务运行（由 TestStdio* 经 -test.run 复执行本测试二进制触发）。
func TestHelperProcess(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		return
	}
	runStubServer(os.Stdin, os.Stdout)
	os.Exit(0)
}

// runStubServer 极简 MCP 服务端：initialize / tools/list / tools/call。
func runStubServer(r interface{ Read([]byte) (int, error) }, w interface{ Write([]byte) (int, error) }) {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := r.Read(tmp)
		buf = append(buf, tmp[:n]...)
		for {
			i := indexByte(buf, '\n')
			if i < 0 {
				break
			}
			line := buf[:i]
			buf = append([]byte{}, buf[i+1:]...)
			if len(line) == 0 {
				continue
			}
			if resp := stubHandle(line); resp != nil {
				fmt.Fprintln(w, string(resp))
			}
		}
		if err != nil {
			return
		}
	}
}

func indexByte(b []byte, c byte) int {
	for i, v := range b {
		if v == c {
			return i
		}
	}
	return -1
}

func stubHandle(line []byte) []byte {
	var req struct {
		ID     *int64          `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(line, &req); err != nil {
		return nil
	}
	if req.ID == nil {
		return nil // 通知
	}
	resp := func(result any) []byte {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
		return b
	}
	switch req.Method {
	case "initialize":
		return resp(map[string]any{"protocolVersion": "2025-06-18", "serverInfo": map[string]any{"name": "stub", "version": "0.0.1"}})
	case "tools/list":
		return resp(map[string]any{"tools": []map[string]any{
			{"name": "echo", "description": "回显", "inputSchema": map[string]any{"type": "object"},
				"annotations": map[string]any{"readOnlyHint": true}},
			{"name": "boom", "description": "必失败", "inputSchema": map[string]any{"type": "object"}},
		}})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		_ = json.Unmarshal(req.Params, &p)
		if p.Name == "echo" {
			var a struct {
				Msg string `json:"msg"`
			}
			_ = json.Unmarshal(p.Arguments, &a)
			return resp(map[string]any{"content": []map[string]any{{"type": "text", "text": "echo: " + a.Msg}}})
		}
		if p.Name == "boom" {
			return resp(map[string]any{"content": []map[string]any{{"type": "text", "text": "boom"}}, "isError": true})
		}
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": *req.ID,
			"error": map[string]any{"code": -32602, "message": "unknown tool " + p.Name}})
		return b
	case "ping":
		return resp(map[string]any{})
	}
	return nil
}

// ---- HTTP 传输 ----

func httpStub(t *testing.T, mode string, seen *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		*seen = append(*seen, req.Method)
		if req.ID == nil {
			w.WriteHeader(202)
			return
		}
		result := func() map[string]any {
			switch req.Method {
			case "initialize":
				return map[string]any{"protocolVersion": "2025-06-18", "serverInfo": map[string]any{"name": "stub-http"}}
			case "tools/list":
				return map[string]any{"tools": []map[string]any{{"name": "echo"}}}
			case "tools/call":
				return map[string]any{"content": []map[string]any{{"type": "text", "text": "ok"}}}
			}
			return map[string]any{}
		}()
		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
		if mode == "sse" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, ": ping\n\n")
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(payload)
	}))
}

func TestHTTPTransportJSONAndSSE(t *testing.T) {
	ctx, cancel := testCtx(t)
	defer cancel()

	for _, mode := range []string{"json", "sse"} {
		var seen []string
		srv := httpStub(t, mode, &seen)
		c, err := DialHTTP(ctx, srv.URL, "secret-token")
		if err != nil {
			t.Fatalf("[%s] 拨号: %v", mode, err)
		}
		if got := c.ServerInfo().Name; got != "stub-http" {
			t.Fatalf("[%s] serverInfo = %q", mode, got)
		}
		res, err := c.CallTool(ctx, "echo", nil)
		if err != nil || res.Content[0].Text != "ok" {
			t.Fatalf("[%s] CallTool = %+v, %v", mode, res, err)
		}
		c.Close()
		srv.Close()
	}
}

func TestHTTPTransportError(t *testing.T) {
	ctx, cancel := testCtx(t)
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ID == nil {
			w.WriteHeader(202)
			return
		}
		var result any = map[string]any{}
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "serverInfo": map[string]any{"name": "stub-err"}}
		case "tools/call":
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": *req.ID,
				"error": map[string]any{"code": -32601, "message": "method not found"}})
			w.Write(b)
			return
		}
		ok, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
		w.Write(ok)
	}))
	defer srv.Close()
	c, err := DialHTTP(ctx, srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.CallTool(ctx, "x", nil)
	if err == nil || !strings.Contains(err.Error(), "method not found") {
		t.Fatalf("JSON-RPC 错误应透出，got %v", err)
	}
}
