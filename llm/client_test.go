package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
