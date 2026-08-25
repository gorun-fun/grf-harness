package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"grf/config"
	"grf/llm"
	"grf/tools"
)

func TestWebSearchLoop(t *testing.T) {
	var mu sync.Mutex
	var got [][]llm.Message
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []llm.Message `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		calls++
		idx := calls
		got = append(got, req.Messages)
		mu.Unlock()
		var msg llm.Message
		if idx == 1 {
			msg = llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function",
				Function: llm.FunctionCall{Name: "web_search", Arguments: `{"queries":["golang test"]}`}}}}
		} else {
			msg = llm.Message{Role: "assistant", Content: "done"}
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": msg}}})
	}))
	defer srv.Close()

	h, err := Build(Options{
		Workspace: t.TempDir(),
		Client:    llm.NewClient(llm.Config{BaseURL: srv.URL, APIKey: "t", Model: "m"}),
		Settings:  &config.Settings{Sandbox: "off", FsPolicy: "observe"},
		Host:      &tools.NoopHost{},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() {
		out, err := h.Root.Run(context.Background(), "search")
		if err != nil {
			t.Error(err)
		}
		done <- out
	}()
	select {
	case out := <-done:
		if out != "done" {
			t.Fatalf("out=%q", out)
		}
	case <-time.After(150 * time.Second):
		t.Fatal("run 卡住/超时")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls < 2 {
		t.Fatalf("第二轮请求未发出 calls=%d", calls)
	}
	last := got[len(got)-1]
	hasTool := false
	for _, m := range last {
		if m.Role == "tool" && m.ToolCallID == "c1" {
			hasTool = true
		}
	}
	if !hasTool {
		t.Fatalf("第二轮请求缺少 tool 结果消息:\n%+v", last)
	}
}
