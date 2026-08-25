// stats_test.go 流式 usage 统计链路：SSE 尾包带 usage 时，快照应给出
// 精确 token 用量与缓存命中率。
package llm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStreamUsageStats(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		fl.Flush()
		io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":50,\"prompt_tokens_details\":{\"cached_tokens\":970}}}\n\n")
		fl.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, APIKey: "t", Model: "m"})
	msg, err := c.ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, StreamHooks{})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "hello" {
		t.Fatalf("content = %q", msg.Content)
	}
	s := c.Stats()
	if s.Rounds != 1 || !s.UsageSeen {
		t.Fatalf("rounds=%d usageSeen=%v", s.Rounds, s.UsageSeen)
	}
	if s.PromptTokens != 1000 || s.CompletionTokens != 50 {
		t.Fatalf("tokens: prompt=%d completion=%d", s.PromptTokens, s.CompletionTokens)
	}
	if s.CachePct != 97 {
		t.Fatalf("cachePct = %d", s.CachePct)
	}
	if s.FirstTokenAvg <= 0 || s.LLMTime <= 0 {
		t.Fatalf("timing not recorded: first=%v llm=%v", s.FirstTokenAvg, s.LLMTime)
	}
}

func TestStreamOptionsFallback(t *testing.T) {
	called := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		body, _ := io.ReadAll(r.Body)
		if called == 1 {
			// 第一次：服务端不认识 stream_options，返回 400
			if !strings.Contains(string(body), "stream_options") {
				t.Errorf("首次请求应带 stream_options")
			}
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"Unknown parameter: stream_options"}}`)
			return
		}
		if strings.Contains(string(body), "stream_options") {
			t.Errorf("重试请求不应带 stream_options")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, APIKey: "t", Model: "m"})
	msg, err := c.ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, StreamHooks{})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "ok" || called != 2 {
		t.Fatalf("msg=%q called=%d", msg.Content, called)
	}
	_ = time.Since(time.Now())
}
