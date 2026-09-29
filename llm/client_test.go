package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestChatToolRoundtrip 整包：请求含 tools、回复含 tool_calls。
func TestChatToolRoundtrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if len(req.Tools) != 1 || req.Tools[0].Function.Name != "read" {
			t.Errorf("tools not sent: %+v", req.Tools)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []any{map[string]any{
						"id":   "call_1",
						"type": "function",
						"function": map[string]any{
							"name":      "read",
							"arguments": `{"file_path":"a.go"}`,
						},
					}},
				},
			}},
		})
	}))
	defer srv.Close()
	c := NewClient(Config{BaseURL: srv.URL, APIKey: "k", Model: "m"})
	msg, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}},
		[]Tool{{Name: "read", Description: "d", Parameters: map[string]any{"type": "object"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Name() != "read" {
		t.Fatalf("%+v", msg)
	}
}

// TestChatStreamSSE SSE 流式：content/reasoning 增量与 tool_calls 拼装。
func TestChatStreamSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"reasoning_content":"思"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"reasoning_content":"考"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"Hel"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"lo"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"ba"}}]}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"sh"}}]}}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	c := NewClient(Config{BaseURL: srv.URL, Model: "m"})

	var content, reasoning string
	msg, err := c.ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil,
		StreamHooks{
			Content:   func(d string) { content += d },
			Reasoning: func(d string) { reasoning += d },
		})
	if err != nil {
		t.Fatal(err)
	}
	if content != "Hello" || reasoning != "思考" {
		t.Fatalf("content=%q reasoning=%q", content, reasoning)
	}
	if msg.Content != "Hello" || msg.Reasoning != "思考" {
		t.Fatalf("msg: %+v", msg)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Name() != "ba" || msg.ToolCalls[0].Arguments() != "sh" {
		t.Fatalf("toolcalls: %+v", msg.ToolCalls)
	}
}

// TestMultimodalMarshal 图像 parts 的消息序列化。
func TestMultimodalMarshal(t *testing.T) {
	m := Message{Role: "user", ContentParts: []Part{TextPart("看图"), ImagePart("image/png", "aGk=")}}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `"image_url"`) || !strings.Contains(s, `"data:image/png;base64,aGk="`) {
		t.Fatalf("marshal: %s", s)
	}
	if strings.Contains(s, `"content":""`) {
		t.Fatalf("empty content should be omitted: %s", s)
	}
}

// TestPromptCacheKeySent 设置缓存键后随请求发送；未设置时不携带字段。
func TestPromptCacheKeySent(t *testing.T) {
	var got []chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		got = append(got, req)
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]any{"role": "assistant", "content": "ok"},
			}},
		})
	}))
	defer srv.Close()
	c := NewClient(Config{BaseURL: srv.URL, APIKey: "k", Model: "m"})
	if _, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil); err != nil {
		t.Fatal(err)
	}
	if got[0].PromptCacheKey != "" {
		t.Fatalf("未设置时不应携带 prompt_cache_key, got %q", got[0].PromptCacheKey)
	}
	c.SetCacheKey("grf-abc")
	if _, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil); err != nil {
		t.Fatal(err)
	}
	if got[1].PromptCacheKey != "grf-abc" {
		t.Fatalf("prompt_cache_key = %q, 期望 grf-abc", got[1].PromptCacheKey)
	}
}

func TestChatStreamFinishReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"reasoning_content":"思考"},"finish_reason":null}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"length"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	c := NewClient(Config{BaseURL: srv.URL, APIKey: "t", Model: "m"})
	msg, err := c.ChatStream(context.Background(), nil, nil, StreamHooks{})
	if err != nil {
		t.Fatal(err)
	}
	if msg.FinishReason != "length" || msg.Reasoning != "思考" {
		t.Fatalf("FinishReason=%q Reasoning=%q", msg.FinishReason, msg.Reasoning)
	}
}

// TestModelRef 全名访问器：provider 与模型都设置时拼 "provider/model"
// （模型名可含 "/"，按首个 "/" 拆分往返）；provider 未设置或模型为空时退化。
func TestModelRef(t *testing.T) {
	cases := []struct {
		cfg  Config
		want string
	}{
		{Config{Provider: "deepseek", Model: "deepseek-chat"}, "deepseek/deepseek-chat"},
		{Config{Model: "gpt-4o"}, "gpt-4o"},
		{Config{Provider: "deepseek", Model: ""}, ""},
		// 模型名自身含 "/"：纯拼接，SplitDefault 按首个 "/" 拆分仍可无损往返
		{Config{Provider: "九天", Model: "deepseek/deepseek-r1"}, "九天/deepseek/deepseek-r1"},
	}
	for _, c := range cases {
		if got := NewClient(c.cfg).ModelRef(); got != c.want {
			t.Errorf("ModelRef(%+v) = %q, want %q", c.cfg, got, c.want)
		}
	}
}

// TestTransientRetry 非流式请求的瞬时错误重试：首个 429/503 后恢复 200
// 应被有界重试吸收；不可重试的 400（如上下文超限）立即失败不重发。
func TestTransientRetry(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch atomic.AddInt32(&n, 1) {
		case 1:
			http.Error(w, `{"error":{"message":"rate limited"}}`, http.StatusTooManyRequests)
		case 2:
			http.Error(w, `{"error":{"message":"upstream boom"}}`, http.StatusBadGateway)
		default:
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
		}
	}))
	defer srv.Close()
	c := NewClient(Config{BaseURL: srv.URL, APIKey: "t", Model: "m"})
	msg, err := c.Chat(context.Background(), nil, nil)
	if err != nil || msg.Content != "ok" {
		t.Fatalf("429+503 后应重试成功: msg=%+v err=%v", msg, err)
	}
	if got := atomic.LoadInt32(&n); got != 3 {
		t.Fatalf("应发 3 次请求（429、503、成功），实际 %d", got)
	}

	var m int32
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&m, 1)
		http.Error(w, `{"error":{"message":"context length exceeded"}}`, http.StatusBadRequest)
	}))
	defer srv2.Close()
	c2 := NewClient(Config{BaseURL: srv2.URL, APIKey: "t", Model: "m"})
	if _, err := c2.Chat(context.Background(), nil, nil); err == nil {
		t.Fatal("400 应失败")
	}
	if got := atomic.LoadInt32(&m); got != 1 {
		t.Fatalf("400 不应重试，实际发 %d 次", got)
	}
}
