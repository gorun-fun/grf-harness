// server_test.go mcpserver SDK 协议测试：stdio 行循环（注入 reader/
// writer）与 Streamable HTTP handler（httptest），覆盖握手/枚举/调用/
// 工具级失败/未知方法/通知不回复/鉴权。
package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer() *Server {
	s := New("test-plugin", "0.1.0")
	s.Add(Tool{
		Name:        "echo",
		Description: "回显",
		Schema:      map[string]any{"type": "object"},
		Handler: func(ctx context.Context, args json.RawMessage) ([]Content, error) {
			return []Content{Text("echo: " + string(args))}, nil
		},
	})
	s.Add(Tool{
		Name: "boom",
		Handler: func(ctx context.Context, args json.RawMessage) ([]Content, error) {
			return nil, fmt.Errorf("爆炸")
		},
	})
	s.Add(Tool{
		Name:     "peek",
		ReadOnly: true,
		Handler: func(ctx context.Context, args json.RawMessage) ([]Content, error) {
			return []Content{Text("ok")}, nil
		},
	})
	return s
}

type lineRW struct {
	in  *io.PipeWriter
	out *bufio.Reader
}

func (l *lineRW) send(line string) { io.WriteString(l.in, line+"\n") }

func (l *lineRW) recv() string {
	b, err := l.out.ReadString('\n')
	if err != nil {
		return ""
	}
	return strings.TrimSpace(b)
}

func TestServeStdio(t *testing.T) {
	s := newTestServer()
	srvIn, testWrite := io.Pipe() // 测试写请求 → 服务端读
	testRead, srvOut := io.Pipe() // 服务端写应答 → 测试读
	go func() { _ = s.Serve(context.Background(), srvIn, srvOut) }()
	l := &lineRW{in: testWrite, out: bufio.NewReader(testRead)}

	l.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`)
	resp := l.recv()
	if !strings.Contains(resp, `"protocolVersion":"2025-03-26"`) || !strings.Contains(resp, `"name":"test-plugin"`) {
		t.Fatalf("initialize 应答不符: %s", resp)
	}

	l.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	l.send(`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if got := l.recv(); !strings.Contains(got, `"id":2`) {
		t.Fatalf("通知不应回复，ping 应答应为下一行: %s", got)
	}

	l.send(`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	resp = l.recv()
	for _, want := range []string{`"name":"echo"`, `"name":"peek"`, `"readOnlyHint":true`, `"inputSchema"`} {
		if !strings.Contains(resp, want) {
			t.Fatalf("tools/list 缺 %s: %s", want, resp)
		}
	}

	l.send(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"echo","arguments":{"msg":"hi"}}}`)
	resp = l.recv()
	if !strings.Contains(resp, `"text":"echo: {\"msg\":\"hi\"}"`) {
		t.Fatalf("echo 调用不符: %s", resp)
	}

	l.send(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"boom"}}`)
	resp = l.recv()
	if !strings.Contains(resp, `"isError":true`) || !strings.Contains(resp, "爆炸") {
		t.Fatalf("工具失败应为 isError 内容: %s", resp)
	}

	l.send(`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"nope"}}`)
	resp = l.recv()
	if !strings.Contains(resp, `"isError":true`) || !strings.Contains(resp, "unknown tool") {
		t.Fatalf("未知工具应 isError: %s", resp)
	}

	l.send(`{"jsonrpc":"2.0","id":7,"method":"resources/list"}`)
	resp = l.recv()
	if !strings.Contains(resp, "-32601") {
		t.Fatalf("未知方法应 -32601: %s", resp)
	}
}

func TestHTTPHandler(t *testing.T) {
	s := newTestServer()
	ts := httptest.NewServer(s.Handler("secret"))
	defer ts.Close()

	post := func(body, token string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	if resp := post(`{}`, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无 token 应 401，got %d", resp.StatusCode)
	}
	if resp := post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`, "secret"); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("通知应 202，got %d", resp.StatusCode)
	}
	resp := post(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"echo","arguments":{}}}`, "secret")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("请求应 200，got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"text":"echo: {}"`) {
		t.Fatalf("echo 应答不符: %s", body)
	}

	// GET 应 405
	res, _ := http.Get(ts.URL)
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET 应 405，got %d", res.StatusCode)
	}
}
