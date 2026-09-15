// client.go HTTP 客户端：Chat（整包）、ChatStream（SSE 流式，非 SSE 自动
// 回退）、Models（GET /models）。
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Config 客户端配置。
type Config struct {
	BaseURL string
	APIKey  string
	Model   string
}

// Client OpenAI 兼容接口客户端。
type Client struct {
	cfg      Config
	http     *http.Client
	st       stats
	cacheKey string // prompt_cache_key：会话级缓存路由提示（OpenAI 协议）
}

func NewClient(cfg Config) *Client {
	return &Client{
		cfg:  cfg,
		http: &http.Client{Timeout: 10 * time.Minute},
	}
}

func (c *Client) Model() string { return c.cfg.Model }

// SetCacheKey / CacheKey 会话级提示词缓存路由键：同一会话的多轮请求
// 携带同一 key，支持该字段的服务端会把请求路由到同一缓存分片，提高
// 前缀缓存命中（不支持的服务端忽略该字段）。换 client（切模型）时
// 由上层把 key 迁移过去。
func (c *Client) SetCacheKey(key string) { c.cacheKey = key }
func (c *Client) CacheKey() string       { return c.cacheKey }

// Stats 累计用量统计快照。
func (c *Client) Stats() StatsSnapshot { return c.st.snapshot() }

type chatRequest struct {
	Model          string         `json:"model"`
	Messages       []Message      `json:"messages"`
	Tools          []toolDef      `json:"tools,omitempty"`
	Stream         bool           `json:"stream,omitempty"`
	StreamOptions  *streamOptions `json:"stream_options,omitempty"`
	PromptCacheKey string         `json:"prompt_cache_key,omitempty"`
}

// streamOptions 请求服务端在流式尾包携带 usage（OpenAI 兼容协议）。
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type toolDef struct {
	Type     string `json:"type"`
	Function Tool   `json:"function"`
}

type chatResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage *usageInfo `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// buildRequest 构造 HTTP 请求。
func (c *Client) buildRequest(ctx context.Context, req chatRequest) (*http.Request, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(c.cfg.BaseURL, "/")+"/chat/completions",
		bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	return httpReq, nil
}

// doStream 发起流式请求：带 stream_options.include_usage；个别服务端
// 不认识该参数返回 4xx 时去掉它重试一次。
func (c *Client) doStream(ctx context.Context, req chatRequest) (*http.Response, error) {
	do := func() (*http.Response, error) {
		httpReq, err := c.buildRequest(ctx, req)
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Accept", "text/event-stream")
		return c.http.Do(httpReq)
	}
	resp, err := do()
	if err != nil {
		return nil, fmt.Errorf("call api: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && strings.Contains(strings.ToLower(string(data)), "stream_options") {
			req.StreamOptions = nil // 服务端不支持：去掉参数重试
			resp, err = do()
			if err != nil {
				return nil, fmt.Errorf("call api: %w", err)
			}
			return resp, nil
		}
		return nil, fmt.Errorf("api error %d: %s", resp.StatusCode, truncate(string(data), 2000))
	}
	return resp, nil
}

// parseWhole 整包 JSON 解析。
func parseWhole(data []byte) (Message, error) {
	var out chatResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return Message{}, fmt.Errorf("decode response: %w", err)
	}
	if out.Error != nil {
		return Message{}, fmt.Errorf("api error: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return Message{}, fmt.Errorf("api returned no choices")
	}
	msg := out.Choices[0].Message
	msg.FinishReason = out.Choices[0].FinishReason
	return msg, nil
}

// Chat 发送一次对话请求，返回 assistant 消息（可能携带 ToolCalls）。
func (c *Client) Chat(ctx context.Context, messages []Message, tools []Tool) (Message, error) {
	req := chatRequest{Model: c.cfg.Model, Messages: messages, PromptCacheKey: c.cacheKey}
	for _, t := range tools {
		req.Tools = append(req.Tools, toolDef{Type: "function", Function: t})
	}
	httpReq, err := c.buildRequest(ctx, req)
	if err != nil {
		return Message{}, err
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return Message{}, fmt.Errorf("call api: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return Message{}, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Message{}, fmt.Errorf("api error %d: %s", resp.StatusCode, truncate(string(data), 2000))
	}
	msg, err := parseWhole(data)
	if err != nil {
		return Message{}, err
	}
	fallbackContentToolCalls(&msg)
	return msg, nil
}

// ToolArgsHook 工具调用参数生成增量回调：模型流式生成 tool_call 参数期间
// 逐片触发。index 为本次响应内的工具调用序号；id/name 为累计值（个别
// 服务端分片先后到达，参数开始流出时 name 一般已齐）；delta 为本片参数
// 增量。DSML 正文回退路径不触发本回调。
type ToolArgsHook func(index int, id, name, delta string)

// StreamHooks 流式输出回调。
type StreamHooks struct {
	Content   func(delta string)
	Reasoning func(delta string)
	ToolArgs  ToolArgsHook
}

type streamChunk struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Delta        struct {
			Content   string `json:"content"`
			Reasoning string `json:"reasoning_content"`
			ReasonAlt string `json:"reasoning"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *usageInfo `json:"usage"`
}

// ChatStream 流式请求：通过 hooks 回调正文/思考增量，返回完整 assistant
// 消息。服务端不支持 SSE 时自动回退整包解析。
func (c *Client) ChatStream(ctx context.Context, messages []Message, tools []Tool, hooks StreamHooks) (Message, error) {
	start := time.Now()
	var first time.Duration // 首 token 延迟
	var u *usageInfo
	var outEst int64 // 输出 token 估算（无 usage 时兜底）
	promptEst := estimateMessagesTokens(messages)
	firstMark := func() {
		if first == 0 {
			first = time.Since(start)
		}
	}
	ok := false
	defer func() {
		if !ok {
			return
		}
		if first == 0 {
			first = time.Since(start)
		}
		c.st.addRound(time.Since(start), first, u, promptEst, outEst)
	}()

	req := chatRequest{Model: c.cfg.Model, Messages: messages, Stream: true,
		StreamOptions:  &streamOptions{IncludeUsage: true}, // 请求尾包 usage（统计用）
		PromptCacheKey: c.cacheKey}
	for _, t := range tools {
		req.Tools = append(req.Tools, toolDef{Type: "function", Function: t})
	}

	resp, err := c.doStream(ctx, req)
	if err != nil {
		return Message{}, err
	}
	defer resp.Body.Close()

	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		first = time.Since(start)
		data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		if err != nil {
			return Message{}, fmt.Errorf("read response: %w", err)
		}
		var whole chatResponse
		if json.Unmarshal(data, &whole) == nil && whole.Usage != nil {
			u = whole.Usage
		}
		msg, err := parseWhole(data)
		if err != nil {
			return Message{}, err
		}
		fallbackContentToolCalls(&msg)
		outEst = estimateTokens(msg.Content) + estimateTokens(msg.Reasoning)
		if msg.Content != "" && hooks.Content != nil {
			hooks.Content(msg.Content)
		}
		if msg.Reasoning != "" && hooks.Reasoning != nil {
			hooks.Reasoning(msg.Reasoning)
		}
		ok = true
		return msg, nil
	}

	msg := Message{Role: "assistant"}
	calls := map[int]*ToolCall{}
	// 流式增量先过显示过滤，剔除 DSML/<tool_call> 标签文本再送 UI
	var cf, rf displayFilter
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk streamChunk
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if chunk.Usage != nil {
			u = chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		if fr := chunk.Choices[0].FinishReason; fr != "" {
			msg.FinishReason = fr
		}
		delta := chunk.Choices[0].Delta
		if d := delta.Reasoning + delta.ReasonAlt + delta.Content; d != "" {
			firstMark()
			outEst += estimateTokens(d)
		}
		for _, tc := range delta.ToolCalls {
			if tc.Function.Name != "" || tc.Function.Arguments != "" {
				firstMark()
				outEst += estimateTokens(tc.Function.Name) + estimateTokens(tc.Function.Arguments)
			}
		}
		if reasoning := delta.Reasoning + delta.ReasonAlt; reasoning != "" {
			msg.Reasoning += reasoning
			if hooks.Reasoning != nil {
				if d := rf.delta(msg.Reasoning); d != "" {
					hooks.Reasoning(d)
				}
			}
		}
		if delta.Content != "" {
			msg.Content += delta.Content
			if hooks.Content != nil {
				if d := cf.delta(msg.Content); d != "" {
					hooks.Content(d)
				}
			}
		}
		for _, tc := range delta.ToolCalls {
			cur, ok := calls[tc.Index]
			if !ok {
				cur = &ToolCall{Type: "function"}
				calls[tc.Index] = cur
			}
			if tc.ID != "" {
				cur.ID = tc.ID
			}
			cur.Function.Name += tc.Function.Name
			cur.Function.Arguments += tc.Function.Arguments
			// 工具调用头(name/id 先到、参数为空)与参数增量都即时报给宿主
			// ——头片一到宿主就能建卡(工具名先显示,不再干等参数)。
			if hooks.ToolArgs != nil &&
				(tc.ID != "" || tc.Function.Name != "" || tc.Function.Arguments != "") {
				hooks.ToolArgs(tc.Index, cur.ID, cur.Function.Name, tc.Function.Arguments)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return Message{}, ctx.Err() // 取消/超时：不得把半截消息当成功返回
		}
		return Message{}, fmt.Errorf("read stream: %w", err)
	}
	if len(calls) > 0 {
		idxs := make([]int, 0, len(calls))
		for i := range calls {
			idxs = append(idxs, i)
		}
		sort.Ints(idxs)
		for _, i := range idxs {
			msg.ToolCalls = append(msg.ToolCalls, *calls[i])
		}
	}
	fallbackContentToolCalls(&msg)
	ok = true
	return msg, nil
}

// Models 查询服务端模型列表（GET /models）。
func (c *Client) Models(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimSuffix(c.cfg.BaseURL, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call api: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("api error %d: %s", resp.StatusCode, truncate(string(data), 2000))
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	models := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		models = append(models, m.ID)
	}
	sort.Strings(models)
	return models, nil
}
