// client.go HTTP 客户端：Chat（整包）、ChatStream（SSE 流式，非 SSE 自动
// 回退）、Models（GET /models）。
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// apiError 服务端错误哨兵：非 200 状态码（status 记状态码、body 记截断
// 后的响应体）或响应体 JSON 的 error 字段（status=0、body 记 message）。
// ChatStream 用 errors.As 识别它并停止盲重试——服务端明确拒绝（如图片
// 输入不支持）属请求本身的问题，重试会掩盖 agent 的自愈路径。
// Error() 文案与历史 fmt.Errorf 版本逐字一致。
type apiError struct {
	status int
	body   string
}

func (e *apiError) Error() string {
	if e.status == 0 {
		return "api error: " + e.body
	}
	return fmt.Sprintf("api error %d: %s", e.status, e.body)
}

// readCheckedBody 读响应体（上限 limit）并做状态检查：非 200 统一返回
// *apiError（正文截断 2000 字符）。postJSON 与流式非 SSE 回退共用。
func readCheckedBody(resp *http.Response, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &apiError{status: resp.StatusCode, body: truncate(string(data), 2000)}
	}
	return data, nil
}

// postJSON 发起请求并整包读取响应（do → 读体 → 状态检查），Chat 与
// Models 共用；请求由调用方经 buildRequest 构造（已携带 ctx）。
func (c *Client) postJSON(req *http.Request, limit int64) ([]byte, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call api: %w", err)
	}
	defer resp.Body.Close()
	return readCheckedBody(resp, limit)
}

// Config 客户端配置。
type Config struct {
	// Provider 供应商名（settings.json providers 的键）。仅用于 ModelRef
	// 组装对客户端回报的全名；上游 API 请求体始终用裸名 Model。
	Provider string
	BaseURL  string
	APIKey   string
	Model    string
}

// Client OpenAI 兼容接口客户端。
type Client struct {
	cfg      Config
	http     *http.Client
	st       stats
	cacheKey string // prompt_cache_key：会话级缓存路由提示（OpenAI 协议）
	effort   string // reasoning_effort：思考强度（low/high/max…，空 = 服务端默认）
}

func NewClient(cfg Config) *Client {
	// 不设整体 Timeout：思考型模型静默思考可达十几分钟，总超时会掐断流
	// （"context deadline exceeded (Client.Timeout ...)"）。连接建立与响应头
	// 各自有超时，杜绝挂死；读体阶段交给调用方 context 取消。
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSHandshakeTimeout = 30 * time.Second
	tr.ResponseHeaderTimeout = 30 * time.Minute
	return &Client{
		cfg:  cfg,
		http: &http.Client{Transport: tr},
	}
}

func (c *Client) Model() string { return c.cfg.Model }

// ModelRef 模型全名 "provider/model"，供 server 等接入表面回报给客户端
// （ORS 的 response.model）。客户端拿全名可原样回传请求体切换模型；
// 裸名会被忽略。模型名自身可含 "/"（如 "qwen/qwen3.8-27b"），拼接后
// 仍可无损往返：SplitDefault 按首个 "/" 拆分。Provider 未设置（环境
// 变量直连等）或 Model 为空时退化为裸名/空串。
func (c *Client) ModelRef() string {
	if c.cfg.Model == "" || c.cfg.Provider == "" {
		return c.cfg.Model
	}
	return c.cfg.Provider + "/" + c.cfg.Model
}

// SetCacheKey / CacheKey 会话级提示词缓存路由键：同一会话的多轮请求
// 携带同一 key，支持该字段的服务端会把请求路由到同一缓存分片，提高
// 前缀缓存命中（不支持的服务端忽略该字段）。换 client（切模型）时
// 由上层把 key 迁移过去。
func (c *Client) SetCacheKey(key string) { c.cacheKey = key }
func (c *Client) CacheKey() string       { return c.cacheKey }

// SetReasoningEffort 设置思考强度（"low"/"high"/"max" 等，空串 = 不发送，
// 用服务端默认）。值原样透传 reasoning_effort 字段，由上游决定语义。
func (c *Client) SetReasoningEffort(e string) { c.effort = e }

// Stats 累计用量统计快照。
func (c *Client) Stats() StatsSnapshot { return c.st.snapshot() }

type chatRequest struct {
	Model           string         `json:"model"`
	Messages        []Message      `json:"messages"`
	Tools           []toolDef      `json:"tools,omitempty"`
	Stream          bool           `json:"stream,omitempty"`
	StreamOptions   *streamOptions `json:"stream_options,omitempty"`
	PromptCacheKey  string         `json:"prompt_cache_key,omitempty"`
	ReasoningEffort string         `json:"reasoning_effort,omitempty"` // 思考强度（OpenAI 兼容扩展，不支持的服务端忽略）
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
		return nil, &apiError{status: resp.StatusCode, body: truncate(string(data), 2000)}
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
		return Message{}, &apiError{body: out.Error.Message}
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
	req := chatRequest{Model: c.cfg.Model, Messages: messages, PromptCacheKey: c.cacheKey, ReasoningEffort: c.effort}
	for _, t := range tools {
		req.Tools = append(req.Tools, toolDef{Type: "function", Function: t})
	}
	httpReq, err := c.buildRequest(ctx, req)
	if err != nil {
		return Message{}, err
	}
	data, err := c.postJSON(httpReq, 32<<20)
	if err != nil {
		return Message{}, err
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

// ChatStream 流式请求：连接在产出任何增量之前断开（上游重启/网关掐线，
// 典型报错 "read stream: unexpected EOF"）时自动重试最多 10 次；已流出
// 增量的中断不内部重试（会造成 UI 重复输出），交由上层（宿主整回合重发）
// 处理。服务端明确拒绝（api error，如图片输入不支持）不重试——那属于
// 请求本身的问题，盲重试会掩盖 agent 的自愈路径（图片降级重发）。
func (c *Client) ChatStream(ctx context.Context, messages []Message, tools []Tool, hooks StreamHooks) (Message, error) {
	const maxRetry = 10
	for attempt := 0; ; attempt++ {
		msg, emitted, err := c.chatStreamOnce(ctx, messages, tools, hooks)
		if err == nil {
			return msg, nil
		}
		var apiErr *apiError
		if ctx.Err() != nil || emitted || attempt >= maxRetry || errors.As(err, &apiErr) {
			return Message{}, err
		}
		time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
	}
}

// chatStreamOnce 单次流式尝试；emitted 表示是否已向宿主回调过任何增量。
func (c *Client) chatStreamOnce(ctx context.Context, messages []Message, tools []Tool, hooks StreamHooks) (_ Message, emitted bool, _ error) {
	start := time.Now()
	var first time.Duration // 首 token 延迟
	var u *usageInfo
	var outEst int64 // 输出 token 估算（无 usage 时兜底）
	promptEst := estimateMessagesTokens(messages)
	firstMark := func() {
		if first == 0 {
			first = time.Since(start)
		}
		emitted = true
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
		PromptCacheKey: c.cacheKey, ReasoningEffort: c.effort}
	for _, t := range tools {
		req.Tools = append(req.Tools, toolDef{Type: "function", Function: t})
	}

	resp, err := c.doStream(ctx, req)
	if err != nil {
		return Message{}, false, err
	}
	defer resp.Body.Close()

	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		first = time.Since(start)
		data, err := readCheckedBody(resp, 32<<20)
		if err != nil {
			return Message{}, false, err
		}
		var whole chatResponse
		if json.Unmarshal(data, &whole) == nil && whole.Usage != nil {
			u = whole.Usage
		}
		msg, err := parseWhole(data)
		if err != nil {
			return Message{}, false, err
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
		return msg, true, nil
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
			return Message{}, emitted, ctx.Err() // 取消：不得把半截消息当成功返回
		}
		return Message{}, emitted, fmt.Errorf("read stream: %w", err)
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
	return msg, true, nil
}

// Models 查询服务端模型列表（GET /models）。
func (c *Client) Models(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimSuffix(c.cfg.BaseURL, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	data, err := c.postJSON(req, 4<<20)
	if err != nil {
		return nil, err
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
