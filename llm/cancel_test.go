// cancel_test.go Ctrl+C 中断流式请求：取消不得把半截消息当成功返回
// （否则半截 assistant 消息入库，content 被省略，下次请求 400）。
package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatStreamCancelReturnsError(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		// 先吐一个增量，然后挂住连接等待客户端取消
		_, err := w.Write([]byte("data: " + `{"choices":[{"delta":{"content":"hel"}}]}` + "\n\n"))
		if err != nil {
			t.Error(err)
		}
		w.(http.Flusher).Flush()
		<-block
	}))
	defer srv.Close()
	defer close(block)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gotDelta := make(chan struct{}, 1)
	hooks := StreamHooks{Content: func(string) {
		select {
		case gotDelta <- struct{}{}:
			cancel() // 收到首个增量即取消（模拟 Ctrl+C）
		default:
		}
	}}
	c := NewClient(Config{BaseURL: srv.URL, APIKey: "k", Model: "m"})
	msg, err := c.ChatStream(ctx, []Message{{Role: "user", Content: "hi"}}, nil, hooks)
	if err == nil {
		t.Fatalf("canceled stream must return an error, got success with msg %+v (content %q)", msg, msg.Content)
	}
	if msg.Content != "" || len(msg.ToolCalls) != 0 {
		t.Fatalf("no partial message should be returned on cancel, got %+v", msg)
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("expected context canceled error, got: %v", err)
	}
}
