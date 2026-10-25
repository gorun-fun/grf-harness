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
	"strconv"
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
// 瞬时错误（连接失败、429/5xx）按重试策略退避重试（见 retryable）。
func (c *Client) Chat(ctx context.Context, messages []Message, tools []Tool) (Message, error) {
	const maxRetry = 3
	for attempt := 0; ; attempt++ {
		msg, err := c.chatOnce(ctx, messages, tools)
		if err == nil {
			return msg, nil
		}
		if ctx.Err() != nil || attempt >= maxRetry || !retryable(err) {
			return Message{}, err
		}
		time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
	}
}

func (c *Client) chatOnce(ctx context.Context, messages []Message, tools []Tool) (Message, error) {
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

// retryable 一个请求错误是否值得原样重试：传输层失败（连接拒绝/重置/
// EOF）可重试；服务端 api error 仅限瞬时类（429 限流、5xx 网关/上游）——
// 其余 4xx 属请求本身的问题，盲重试会掩盖 agent 的自愈路径（图片降级
// 重发等）。并发子代理（workflow 等）同端点压出偶发 429 是结构化任务
// 偶发 null 的根因，必须在此层吸收。
func retryable(err error) bool {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		return apiErr.status == 429 || apiErr.status >= 500
	}
	return true
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

// streamToolCallDelta 流式 tool_call 分片（OpenAI 按 index 分片投递）。
type streamToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type streamChunk struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Delta        struct {
			Content   string `json:"content"`
			Reasoning string `json:"reasoning_content"`
			ReasonAlt string `json:"reasoning"`
			ToolCalls []streamToolCallDelta `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *usageInfo `json:"usage"`
}

// ChatStream 流式请求：连接在产出任何增量之前断开（上游重启/网关掐线，
// 典型报错 "read stream: unexpected EOF"）时自动重试最多 10 次；瞬时
// 服务端错误（429 限流/5xx，见 retryable）同样退避重试。已流出增量的
// 中断不内部重试（会造成 UI 重复输出），交由上层（宿主整回合重发）
// 处理。服务端明确拒绝（其余 4xx api error，如图片输入不支持）不重试
// ——那属于请求本身的问题，盲重试会掩盖 agent 的自愈路径（图片降级
// 重发）。
func (c *Client) ChatStream(ctx context.Context, messages []Message, tools []Tool, hooks StreamHooks) (Message, error) {
	const maxRetry = 10
	for attempt := 0; ; attempt++ {
		msg, emitted, err := c.chatStreamOnce(ctx, messages, tools, hooks)
		if err == nil {
			return msg, nil
		}
		if ctx.Err() != nil || emitted || attempt >= maxRetry || !retryable(err) {
			return Message{}, err
		}
		time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
	}
}

// chatStreamOnce 单次流式尝试；emitted 表示是否已向宿主回调过任何增量
// （连接产出增量后中断不内部重试，见 ChatStream 注释）。统计簿记（首
// token/usage/估算）留在此层，传输形态分流见 wholeFallback/consumeSSE。
func (c *Client) chatStreamOnce(ctx context.Context, messages []Message, tools []Tool, hooks StreamHooks) (_ Message, emitted bool, _ error) {
	start := time.Now()
	var first time.Duration // 首 token 延迟
	var u *usageInfo
	var outEst int64 // 输出 token 估算（无 usage 时兜底）
	promptEst := estimateMessagesTokens(messages)
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
		msg, uu, est, err := wholeFallback(resp, hooks)
		if err != nil {
			return Message{}, false, err
		}
		u, outEst = uu, est
		ok = true
		return msg, true, nil
	}

	acc, err := consumeSSE(ctx, resp.Body, hooks)
	if err != nil {
		// 取消：不得把半截消息当成功返回；emitted 透传（重试语义判定）
		return Message{}, acc.emitted, err
	}
	u, outEst = acc.usage, acc.outEst
	if acc.firstDelta > 0 {
		first = acc.firstDelta
	}
	fallbackContentToolCalls(&acc.msg)
	ok = true
	return acc.msg, true, nil
}

// wholeFallback 非 SSE 应答（部分网关对 stream 请求仍回整包 JSON）：读
// 全量、解析、回退工具调用解析，正文/思考整段一次性播给宿主。返回
// 消息、usage 与输出 token 估算。
func wholeFallback(resp *http.Response, hooks StreamHooks) (Message, *usageInfo, int64, error) {
	data, err := readCheckedBody(resp, 32<<20)
	if err != nil {
		return Message{}, nil, 0, err
	}
	var whole chatResponse
	var u *usageInfo
	if json.Unmarshal(data, &whole) == nil && whole.Usage != nil {
		u = whole.Usage
	}
	msg, err := parseWhole(data)
	if err != nil {
		return Message{}, nil, 0, err
	}
	fallbackContentToolCalls(&msg)
	est := estimateTokens(msg.Content) + estimateTokens(msg.Reasoning)
	if msg.Content != "" && hooks.Content != nil {
		hooks.Content(msg.Content)
	}
	if msg.Reasoning != "" && hooks.Reasoning != nil {
		hooks.Reasoning(msg.Reasoning)
	}
	return msg, u, est, nil
}

// streamAccum SSE 流消费的累积结果。
type streamAccum struct {
	msg        Message
	usage      *usageInfo
	outEst     int64
	firstDelta time.Duration // 首 token 延迟（0 = 无增量）
	emitted    bool          // 是否已向宿主流出增量（重试语义据此判定）
}

// consumeSSE 消费 SSE 流：data: 行解析、usage / finish_reason / 正文与
// 思考增量（过显示过滤剔除 DSML/<tool_call> 标签文本再送 UI）/ tool_call
// 分片聚合（即时回调）四路累积。ctx 取消时返回已流出前缀的 emitted。
func consumeSSE(ctx context.Context, body io.Reader, hooks StreamHooks) (streamAccum, error) {
	var acc streamAccum
	acc.msg = Message{Role: "assistant"}
	start := time.Now()
	firstMark := func() {
		if acc.firstDelta == 0 {
			acc.firstDelta = time.Since(start)
		}
		acc.emitted = true
	}
	collector := newToolCallCollector(hooks)
	var cf, rf displayFilter
	scanner := bufio.NewScanner(body)
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
			acc.usage = chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		if fr := chunk.Choices[0].FinishReason; fr != "" {
			acc.msg.FinishReason = fr
		}
		delta := chunk.Choices[0].Delta
		if d := delta.Reasoning + delta.ReasonAlt + delta.Content; d != "" {
			firstMark()
			acc.outEst += estimateTokens(d)
		}
		for _, tc := range delta.ToolCalls {
			if tc.Function.Name != "" || tc.Function.Arguments != "" {
				firstMark()
				acc.outEst += estimateTokens(tc.Function.Name) + estimateTokens(tc.Function.Arguments)
			}
		}
		if reasoning := delta.Reasoning + delta.ReasonAlt; reasoning != "" {
			acc.msg.Reasoning += reasoning
			if hooks.Reasoning != nil {
				if d := rf.delta(acc.msg.Reasoning); d != "" {
					hooks.Reasoning(d)
				}
			}
		}
		if delta.Content != "" {
			acc.msg.Content += delta.Content
			if hooks.Content != nil {
				if d := cf.delta(acc.msg.Content); d != "" {
					hooks.Content(d)
				}
			}
		}
		collector.addAll(delta.ToolCalls)
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return acc, ctx.Err()
		}
		return acc, fmt.Errorf("read stream: %w", err)
	}
	acc.msg.ToolCalls = collector.finish()
	return acc, nil
}

// toolCallCollector 流式 tool_call 分片聚合：按 index 合并 id/name/参数
// 增量；头片（name/id 先到、参数为空）与参数增量都即时报给宿主——头片
// 一到宿主就能建卡（工具名先显示，不再干等参数）。
type toolCallCollector struct {
	calls map[int]*ToolCall
	hooks StreamHooks
}

func newToolCallCollector(hooks StreamHooks) *toolCallCollector {
	return &toolCallCollector{calls: map[int]*ToolCall{}, hooks: hooks}
}

// addAll 并入一包增量。
func (c *toolCallCollector) addAll(tcs []streamToolCallDelta) {
	for _, tc := range tcs {
		cur, ok := c.calls[tc.Index]
		if !ok {
			cur = &ToolCall{Type: "function"}
			c.calls[tc.Index] = cur
		}
		if tc.ID != "" {
			cur.ID = tc.ID
		}
		cur.Function.Name += tc.Function.Name
		cur.Function.Arguments += tc.Function.Arguments
		if c.hooks.ToolArgs != nil &&
			(tc.ID != "" || tc.Function.Name != "" || tc.Function.Arguments != "") {
			c.hooks.ToolArgs(tc.Index, cur.ID, cur.Function.Name, cur.Function.Arguments)
		}
	}
}

// finish 按 index 升序收尾。
func (c *toolCallCollector) finish() []ToolCall {
	if len(c.calls) == 0 {
		return nil
	}
	idxs := make([]int, 0, len(c.calls))
	for i := range c.calls {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	out := make([]ToolCall, 0, len(idxs))
	for _, i := range idxs {
		out = append(out, *c.calls[i])
	}
	return out
}

// ModelInfo 一条模型信息（GET /models 解析）。
type ModelInfo struct {
	ID     string
	CtxLen int // 上下文窗口（按社区常见字段名探测，未提供为 0）
}

// ModelsRich 查询模型列表（含上下文窗口）：data[].id 与裸数组 [{id}]
// 两种应答形态都兼容；上下文窗口按社区常见字段名依次尝试（自建网关
// 三者同值；OpenRouter/SiliconFlow 用 context_length，vLLM 用
// max_model_len）。按 id 字典序返回。
func (c *Client) ModelsRich(ctx context.Context) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimSuffix(c.cfg.BaseURL, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
	data, err := c.postJSON(req, 4<<20)
	if err != nil {
		return nil, err
	}
	var wrapped struct {
		Data []map[string]any `json:"data"`
	}
	var bare []map[string]any
	if err := json.Unmarshal(data, &wrapped); err == nil && len(wrapped.Data) > 0 {
		bare = wrapped.Data
	} else if err := json.Unmarshal(data, &bare); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	out := make([]ModelInfo, 0, len(bare))
	for _, item := range bare {
		id, _ := item["id"].(string)
		if id == "" {
			continue
		}
		out = append(out, ModelInfo{ID: id, CtxLen: modelCtxLen(item)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// modelCtxLen 从模型对象里取上下文窗口，数字或数字字符串均可，取首个
// >0 的值。
func modelCtxLen(item map[string]any) int {
	for _, key := range []string{"context_length", "context_window", "max_model_len"} {
		switch v := item[key].(type) {
		case float64:
			if v > 0 {
				return int(v)
			}
		case string:
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

// Models 查询服务端模型列表（GET /models，纯 id 数组——ModelsRich 的
// 薄包装）。
func (c *Client) Models(ctx context.Context) ([]string, error) {
	infos, err := c.ModelsRich(ctx)
	if err != nil {
		return nil, err
	}
	models := make([]string, 0, len(infos))
	for _, m := range infos {
		models = append(models, m.ID)
	}
	return models, nil
}
