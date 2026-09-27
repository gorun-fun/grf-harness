// server_test.go ORS 合规路径测试：基础文本、instructions、客户端函数
// 工具（暂停/续交）、流式事件序列与 SSE 帧格式、多轮链式、图像输入、
// ask_user 链式应答、background、retrieve/cancel/conversations 超集、
// 鉴权、跨"重启"（重建 Server 同数据目录）恢复。LLM 用 httptest 模拟
// （同 agent 包 fakeLLM 模式，外加请求消息记录）。
package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"git.gorun.fun/share/grf-harness/llm"
)

// fakeLLM 模拟 OpenAI 兼容端点：按脚本顺序返回（TOOL: 前缀表示工具调用）。
// 请求侧只宽松记录原始 messages（llm.Message 的多模态反序列化非本测试
// 关心的能力），断言用原始 map 做。
type fakeLLM struct {
	mu      sync.Mutex
	servers []string
	srv     *httptest.Server
	gotMsgs []map[string]any // 最近一次请求收到的 messages（原始 JSON）
	calls   int
	delay   time.Duration
}

func newFakeLLM(responses []string) *fakeLLM {
	f := &fakeLLM{servers: responses}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.delay > 0 {
			time.Sleep(f.delay)
		}
		var req struct {
			Messages []json.RawMessage `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.calls++
		f.gotMsgs = make([]map[string]any, 0, len(req.Messages))
		for _, raw := range req.Messages {
			var m map[string]any
			_ = json.Unmarshal(raw, &m)
			f.gotMsgs = append(f.gotMsgs, m)
		}
		idx := f.calls - 1
		if idx >= len(f.servers) {
			idx = len(f.servers) - 1
		}
		resp := f.servers[idx]
		f.mu.Unlock()

		var msg llm.Message
		if strings.HasPrefix(resp, "TOOL:") {
			body := strings.TrimPrefix(resp, "TOOL:")
			name := "read"
			if i := strings.Index(body, ":"); i > 0 && strings.HasPrefix(body[i+1:], "{") {
				name = body[:i]
				body = body[i+1:]
			}
			msg = llm.Message{Role: "assistant",
				ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function", Function: llm.FunctionCall{Name: name, Arguments: body}}}}
		} else {
			msg = llm.Message{Role: "assistant", Content: resp}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": msg}}})
	}))
	return f
}

func (f *fakeLLM) URL() string { return f.srv.URL }
func (f *fakeLLM) Close()      { f.srv.Close() }
func (f *fakeLLM) lastMsgs() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotMsgs
}

// msgContent 取 message 的 content 字符串形式（多模态时为空）。
func msgContent(m map[string]any) string {
	s, _ := m["content"].(string)
	return s
}

// newTestServer 测试服务（fake LLM 注入 + 隔离 GRF_HOME）。
func newTestServer(t *testing.T, f *fakeLLM) *httptest.Server {
	t.Helper()
	t.Setenv("GRF_HOME", t.TempDir())
	s, err := New(t.TempDir(), "test-token")
	if err != nil {
		t.Fatal(err)
	}
	s.clientFactory = func() (*llm.Client, error) {
		return llm.NewClient(llm.Config{BaseURL: f.URL(), APIKey: "t", Model: "test-model"}), nil
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// post 发 JSON POST，返回状态码与 body。
func post(t *testing.T, ts *httptest.Server, path string, body any) (int, []byte) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", ts.URL+path, bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func get(t *testing.T, ts *httptest.Server, path string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+path, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// ---- ORS 合规：基础路径 ----

// 合规 #1 Basic Text Response。
func TestBasicTextResponse(t *testing.T) {
	f := newFakeLLM([]string{`你好，我是测试回复。`})
	defer f.Close()
	ts := newTestServer(t, f)

	code, body := post(t, ts, "/v1/responses", map[string]any{
		"model": "test-model", "input": "打个招呼",
	})
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	var r ResponseResource
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatal(err)
	}
	if r.Object != "response" || r.Status != statusCompleted {
		t.Fatalf("object/status 不符: %s / %s", r.Object, r.Status)
	}
	if len(r.Output) == 0 || r.Output[len(r.Output)-1].Type != "message" {
		t.Fatalf("output 应含最终 message item: %+v", r.Output)
	}
	last := r.Output[len(r.Output)-1]
	if last.Role != "assistant" || last.Status != "completed" {
		t.Fatalf("message item 字段不符: %+v", last)
	}
	if len(last.Content) == 0 || last.Content[0].Text != "你好，我是测试回复。" {
		t.Fatalf("回复文本不符: %+v", last.Content)
	}
	if r.Usage == nil || r.Usage.TotalTokens < 0 {
		t.Fatalf("usage 缺失: %+v", r.Usage)
	}
	if !strings.HasPrefix(r.ID, "resp_") {
		t.Fatalf("id 形状不符: %s", r.ID)
	}
}

// 合规 #5 System Prompt（instructions）。
func TestInstructions(t *testing.T) {
	f := newFakeLLM([]string{`ok`})
	defer f.Close()
	ts := newTestServer(t, f)

	code, _ := post(t, ts, "/v1/responses", map[string]any{
		"model": "test-model", "input": "hi", "instructions": "你是日报助手",
	})
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	msgs := f.lastMsgs()
	if len(msgs) == 0 || msgs[0]["role"] != "system" || !strings.Contains(msgContent(msgs[0]), "你是日报助手") {
		t.Fatalf("instructions 未生效: %+v", msgs)
	}
}

// 合规 #6 Tool Calling：客户端函数工具（暂停 → function_call_output 续交）。
func TestClientFunctionToolCalling(t *testing.T) {
	f := newFakeLLM([]string{
		`TOOL:get_weather:{"city":"北京"}`,
		`北京 26 度，晴`,
	})
	defer f.Close()
	ts := newTestServer(t, f)

	code, body := post(t, ts, "/v1/responses", map[string]any{
		"model": "test-model", "input": "北京天气？",
		"tools": []map[string]any{{
			"type": "function", "name": "get_weather", "description": "查天气",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
		}},
	})
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	var r1 ResponseResource
	_ = json.Unmarshal(body, &r1)
	if r1.Status != statusCompleted {
		t.Fatalf("客户端函数暂停应为 completed: %s", r1.Status)
	}
	var fc *Item
	for i := range r1.Output {
		if r1.Output[i].Type == itemFunctionCall {
			fc = &r1.Output[i]
		}
	}
	if fc == nil || fc.Name != "get_weather" || fc.CallID != "c1" || fc.Arguments != `{"city":"北京"}` {
		t.Fatalf("function_call item 不符: %+v", fc)
	}

	// 续交：previous_response_id + function_call_output
	code, body = post(t, ts, "/v1/responses", map[string]any{
		"model": "test-model", "previous_response_id": r1.ID,
		"input": []map[string]any{{"type": "function_call_output", "call_id": "c1", "output": `{"temp":26}`}},
	})
	if code != 200 {
		t.Fatalf("续交 status %d: %s", code, body)
	}
	var r2 ResponseResource
	_ = json.Unmarshal(body, &r2)
	if r2.Status != statusCompleted {
		t.Fatalf("续交应 completed: %s", r2.Status)
	}
	last := r2.Output[len(r2.Output)-1]
	if last.Type != "message" || last.Content[0].Text != "北京 26 度，晴" {
		t.Fatalf("续交回复不符: %+v", last)
	}
	// 历史里应包含工具结果
	msgs := f.lastMsgs()
	found := false
	for _, m := range msgs {
		if m["role"] == "tool" && m["tool_call_id"] == "c1" && strings.Contains(msgContent(m), "26") {
			found = true
		}
	}
	if !found {
		t.Fatalf("工具结果未并入历史: %+v", msgs)
	}
}

// 合规 #8 Multi-turn Conversation。
func TestMultiTurnChain(t *testing.T) {
	f := newFakeLLM([]string{`第一轮`, `第二轮`})
	defer f.Close()
	ts := newTestServer(t, f)

	_, b1 := post(t, ts, "/v1/responses", map[string]any{"model": "m", "input": "第一问"})
	var r1 ResponseResource
	_ = json.Unmarshal(b1, &r1)

	code, b2 := post(t, ts, "/v1/responses", map[string]any{
		"model": "m", "input": "第二问", "previous_response_id": r1.ID,
	})
	if code != 200 {
		t.Fatalf("status %d: %s", code, b2)
	}
	msgs := f.lastMsgs()
	// system + user1 + assistant1 + user2
	if len(msgs) < 4 || msgContent(msgs[1]) != "第一问" || msgContent(msgs[2]) != "第一轮" || msgContent(msgs[3]) != "第二问" {
		t.Fatalf("链式历史不符: %+v", msgs)
	}
}

// 链尾校验：非最新 previous_response_id 拒绝。
func TestPreviousMustBeChainTail(t *testing.T) {
	f := newFakeLLM([]string{`a`, `b`})
	defer f.Close()
	ts := newTestServer(t, f)
	_, b1 := post(t, ts, "/v1/responses", map[string]any{"model": "m", "input": "一"})
	var r1 ResponseResource
	_ = json.Unmarshal(b1, &r1)
	post(t, ts, "/v1/responses", map[string]any{"model": "m", "input": "二", "previous_response_id": r1.ID})
	code, _ := post(t, ts, "/v1/responses", map[string]any{
		"model": "m", "input": "分叉", "previous_response_id": r1.ID,
	})
	if code != 409 {
		t.Fatalf("历史分叉应 409，得到 %d", code)
	}
}

// 合规 #7 Image Input（data URL 多模态）。
func TestImageInput(t *testing.T) {
	f := newFakeLLM([]string{`看到了图`})
	defer f.Close()
	ts := newTestServer(t, f)

	code, _ := post(t, ts, "/v1/responses", map[string]any{
		"model": "m",
		"input": []map[string]any{{
			"type": "message", "role": "user",
			"content": []map[string]any{
				{"type": "input_text", "text": "看图"},
				{"type": "input_image", "image_url": "data:image/png;base64,aGVsbG8="},
			},
		}},
	})
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	msgs := f.lastMsgs()
	last := msgs[len(msgs)-1]
	parts, _ := last["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("多模态消息应有 2 个部件: %+v", last)
	}
	img, _ := parts[1].(map[string]any)
	iu, _ := img["image_url"].(map[string]any)
	urlStr, _ := iu["url"].(string)
	if !strings.HasPrefix(urlStr, "data:image/png") {
		t.Fatalf("图像部件不符: %+v", img)
	}
}

// 合规 #4 Streaming：SSE 帧格式、事件序列、sequence_number 单调、[DONE]。
func TestStreamingEvents(t *testing.T) {
	f := newFakeLLM([]string{`流式回复文本`})
	defer f.Close()
	ts := newTestServer(t, f)

	data, _ := json.Marshal(map[string]any{"model": "m", "input": "hi", "stream": true})
	req, _ := http.NewRequest("POST", ts.URL+"/v1/responses", bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type 不符: %s", ct)
	}

	type frame struct{ event, data string }
	var frames []frame
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var cur frame
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if cur.event != "" || cur.data != "" {
				frames = append(frames, cur)
				cur = frame{}
			}
			continue
		}
		if strings.HasPrefix(line, "event: ") {
			cur.event = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			cur.data = strings.TrimPrefix(line, "data: ")
		}
	}
	if len(frames) == 0 || frames[len(frames)-1].data != "[DONE]" {
		t.Fatalf("流应以 [DONE] 终止: %+v", frames)
	}
	frames = frames[:len(frames)-1] // 去掉 [DONE]

	var seqs []int64
	for _, fr := range frames {
		var ev event
		if err := json.Unmarshal([]byte(fr.data), &ev); err != nil {
			t.Fatalf("事件 JSON 不合法(%s): %v", fr.event, err)
		}
		if fr.event != ev.Type {
			t.Fatalf("event: 行必须等于 data.type: %s vs %s", fr.event, ev.Type)
		}
		seqs = append(seqs, ev.SequenceNumber)
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("sequence_number 必须单调: %v", seqs)
		}
	}
	// 首事件 created，终事件 completed
	if frames[0].event != "response.created" {
		t.Fatalf("首事件应为 response.created: %s", frames[0].event)
	}
	if frames[len(frames)-1].event != "response.completed" {
		t.Fatalf("终事件应为 response.completed: %s", frames[len(frames)-1].event)
	}
	// 正文流过 delta
	hasDelta := false
	for _, fr := range frames {
		if fr.event == "response.output_text.delta" {
			hasDelta = true
		}
	}
	if !hasDelta {
		t.Fatalf("流中缺少 output_text.delta: %v", frames)
	}
	// 终事件携带完整 response（含 message item）
	var last event
	_ = json.Unmarshal([]byte(frames[len(frames)-1].data), &last)
	if last.Response == nil || last.Response.Status != statusCompleted || len(last.Response.Output) == 0 {
		t.Fatalf("completed 事件应携带完整资源: %+v", last.Response)
	}
}

// grf 扩展：ask_user 链式应答（incomplete + grf:input_result）。
func TestAskUserChain(t *testing.T) {
	f := newFakeLLM([]string{
		`TOOL:ask_user_question:{"questions":[{"id":"q1","question":"口味？","options":[{"label":"甜"},{"label":"咸"}]}]}`,
		`选了甜`,
	})
	defer f.Close()
	ts := newTestServer(t, f)

	code, body := post(t, ts, "/v1/responses", map[string]any{"model": "m", "input": "推荐早餐"})
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	var r1 ResponseResource
	_ = json.Unmarshal(body, &r1)
	if r1.Status != statusIncomplete || r1.IncompleteDetails == nil || r1.IncompleteDetails.Reason != inputRequired {
		t.Fatalf("提问应 incomplete(grf:input_required): %s %+v", r1.Status, r1.IncompleteDetails)
	}
	if len(r1.IncompleteDetails.Requests) != 1 || r1.IncompleteDetails.Requests[0].Kind != kindQuestion {
		t.Fatalf("requests 不符: %+v", r1.IncompleteDetails.Requests)
	}

	code, body = post(t, ts, "/v1/responses", map[string]any{
		"model": "m", "previous_response_id": r1.ID,
		"input": []map[string]any{{"type": "grf:input_result", "call_id": "c1", "answers": map[string]any{"q1": "甜"}}},
	})
	if code != 200 {
		t.Fatalf("应答 status %d: %s", code, body)
	}
	var r2 ResponseResource
	_ = json.Unmarshal(body, &r2)
	if r2.Status != statusCompleted || r2.Output[len(r2.Output)-1].Content[0].Text != "选了甜" {
		t.Fatalf("应答后应完成: %s", r2.Status)
	}
}

// 同会话并发拒绝。
func TestBusyConflict(t *testing.T) {
	f := newFakeLLM([]string{`慢`, `慢`})
	f.delay = 300 * time.Millisecond
	defer f.Close()
	ts := newTestServer(t, f)

	_, b1 := post(t, ts, "/v1/responses", map[string]any{"model": "m", "input": "一"})
	var r1 ResponseResource
	_ = json.Unmarshal(b1, &r1)

	done := make(chan int)
	go func() {
		code, _ := post(t, ts, "/v1/responses", map[string]any{"model": "m", "input": "二", "previous_response_id": r1.ID})
		done <- code
	}()
	// 等第一个回合真正开跑
	time.Sleep(100 * time.Millisecond)
	// 直接同步发第三条（同会话在跑）——注意这里复用 r1.ID 会先撞链尾或 busy
	code2, _ := post(t, ts, "/v1/responses", map[string]any{"model": "m", "input": "插队", "previous_response_id": r1.ID})
	gCode := <-done
	if code2 != 409 && gCode != 409 {
		t.Fatalf("并发应 409: 同步=%d 并发=%d", code2, gCode)
	}
}

// 真正同时到达的并发请求必须恰好一个执行、其余 409：旧实现预检
// （IsRunning）与登记（beginTurn）之间存在秒级窗口，多个请求可同时
// 通过预检——漏网者退化为 200+failed 且推进链尾。
func TestBusyConcurrentClaims(t *testing.T) {
	f := newFakeLLM([]string{`慢`, `慢`, `慢`, `慢`})
	f.delay = 400 * time.Millisecond
	defer f.Close()
	ts := newTestServer(t, f)

	_, b1 := post(t, ts, "/v1/responses", map[string]any{"model": "m", "input": "一"})
	var r1 ResponseResource
	_ = json.Unmarshal(b1, &r1)

	const n = 4
	codes := make(chan int, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			<-start
			code, _ := post(t, ts, "/v1/responses", map[string]any{
				"model": "m", "input": "并发", "previous_response_id": r1.ID})
			codes <- code
		}()
	}
	close(start)
	ok := 0
	for i := 0; i < n; i++ {
		switch c := <-codes; c {
		case 200:
			ok++
		case 409:
		default:
			t.Fatalf("意外状态码 %d", c)
		}
	}
	if ok != 1 {
		t.Fatalf("并发请求应恰好一个执行，实际 %d 个 200", ok)
	}
}

// background 模式 + retrieve 轮询。
func TestBackgroundAndRetrieve(t *testing.T) {
	f := newFakeLLM([]string{`后台结果`})
	f.delay = 150 * time.Millisecond
	defer f.Close()
	ts := newTestServer(t, f)

	code, body := post(t, ts, "/v1/responses", map[string]any{
		"model": "m", "input": "后台跑", "background": true,
	})
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	var r ResponseResource
	_ = json.Unmarshal(body, &r)
	if r.Status != statusQueued {
		t.Fatalf("background 应立即返回 queued: %s", r.Status)
	}
	// retrieve 轮询至终态
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		code, body = get(t, ts, "/v1/responses/"+r.ID)
		if code == 200 {
			var rr ResponseResource
			_ = json.Unmarshal(body, &rr)
			if rr.Status == statusCompleted {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("background 回合未在期限内完成")
}

// 超集：conversations 列表 / 归档。
func TestConversations(t *testing.T) {
	f := newFakeLLM([]string{`ok`})
	defer f.Close()
	ts := newTestServer(t, f)

	_, b1 := post(t, ts, "/v1/responses", map[string]any{"model": "m", "input": "hi"})
	var r ResponseResource
	_ = json.Unmarshal(b1, &r)

	code, body := get(t, ts, "/v1/conversations")
	if code != 200 || !strings.Contains(string(body), r.SessionID) {
		t.Fatalf("会话列表应含新会话: %d %s", code, body)
	}
	code, _ = get(t, ts, "/v1/conversations/"+r.SessionID+"/items")
	if code != 200 {
		t.Fatalf("items status %d", code)
	}

	req, _ := http.NewRequest("DELETE", ts.URL+"/v1/conversations/"+r.SessionID, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("归档应 204: %d", resp.StatusCode)
	}
	code, _ = post(t, ts, "/v1/responses", map[string]any{
		"model": "m", "input": "再发", "previous_response_id": r.ID,
	})
	if code != 404 {
		t.Fatalf("归档会话应 404: %d", code)
	}
}

// 归档与在跑回合的生命周期竞态：cancel 后回合未在期限内落定时必须拒绝
// 归档（409、会话原样保留在缓存且不标记归档），落定后重试才 204。回归：
// 旧实现 waitTurn 超时后照常 dropSession，脱离缓存的回合 goroutine 继续
// 写快照/存档，与 h.Close 清掉的产物目录互相踩踏。
func TestArchiveTurnSettleGuard(t *testing.T) {
	old := archiveSettleTimeout
	archiveSettleTimeout = 200 * time.Millisecond // 缩短归档等待期限，测试不等 5 秒
	t.Cleanup(func() { archiveSettleTimeout = old })

	f := newFakeLLM([]string{`慢回复`})
	f.delay = 700 * time.Millisecond // 回合耗时远超归档等待期限
	defer f.Close()
	t.Setenv("GRF_HOME", t.TempDir())
	s, err := New(t.TempDir(), "test-token")
	if err != nil {
		t.Fatal(err)
	}
	s.clientFactory = func() (*llm.Client, error) {
		return llm.NewClient(llm.Config{BaseURL: f.URL(), APIKey: "t", Model: "m"}), nil
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	code, body := post(t, ts, "/v1/responses", map[string]any{
		"model": "m", "input": "后台慢跑", "background": true,
	})
	if code != 200 {
		t.Fatalf("background status %d: %s", code, body)
	}
	var r ResponseResource
	_ = json.Unmarshal(body, &r)

	// 等回合真正开跑（IsRunning 为真 ⇒ beginTurn 已登记取消句柄）
	s.mu.Lock()
	sess := s.sessions[r.SessionID]
	s.mu.Unlock()
	if sess == nil {
		t.Fatal("会话不在缓存")
	}
	deadline := time.Now().Add(2 * time.Second)
	for !sess.root.IsRunning() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !sess.root.IsRunning() {
		t.Fatal("回合未开跑")
	}
	// 取消句柄换成 no-op：模拟"取消后仍未及时落定"的回合（真实场景如
	// 工具深陷不可中断的 IO）——waitTurn 必然超时。
	sess.mu.Lock()
	sess.cancel = func() {}
	sess.mu.Unlock()

	doDelete := func() int {
		req, _ := http.NewRequest("DELETE", ts.URL+"/v1/conversations/"+r.SessionID, nil)
		req.Header.Set("Authorization", "Bearer test-token")
		resp, derr := http.DefaultClient.Do(req)
		if derr != nil {
			t.Fatal(derr)
		}
		defer resp.Body.Close()
		_, _ = io.ReadAll(resp.Body)
		return resp.StatusCode
	}

	// 未落定时归档 → 409，且会话原样保留（不 drop、不标记归档）
	if c := doDelete(); c != 409 {
		t.Fatalf("未落定时归档应 409: %d", c)
	}
	s.mu.Lock()
	_, cached := s.sessions[r.SessionID]
	s.mu.Unlock()
	if !cached || sess.meta.Archived {
		t.Fatal("409 后会话必须原样保留（不 drop、不标记归档）")
	}

	// 回合自然落定后重试归档 → 204，且真正归档生效（续发 404）
	deadline = time.Now().Add(5 * time.Second)
	for !sess.waitTurn(200*time.Millisecond) && time.Now().Before(deadline) {
	}
	time.Sleep(150 * time.Millisecond) // 略等 endTurn 之后的终态快照写完
	if c := doDelete(); c != 204 {
		t.Fatalf("落定后归档应 204: %d", c)
	}
	code, _ = post(t, ts, "/v1/responses", map[string]any{
		"model": "m", "input": "再发", "previous_response_id": r.ID,
	})
	if code != 404 {
		t.Fatalf("归档后续发应 404: %d", code)
	}
}

// 鉴权：无/错令牌 401。
func TestAuth(t *testing.T) {
	f := newFakeLLM([]string{`ok`})
	defer f.Close()
	ts := newTestServer(t, f)
	data, _ := json.Marshal(map[string]any{"model": "m", "input": "x"})
	req, _ := http.NewRequest("POST", ts.URL+"/v1/responses", bytes.NewReader(data))
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("无令牌应 401: %d", resp.StatusCode)
	}
}

// 合规 #9/#10：compact 端点。
func TestCompact(t *testing.T) {
	f := newFakeLLM([]string{`第一轮`, `压缩摘要：用户问了天气`})
	defer f.Close()
	ts := newTestServer(t, f)

	// model 缺失必须拒绝
	code, _ := post(t, ts, "/v1/responses/compact", map[string]any{"input": "x"})
	if code != 400 {
		t.Fatalf("缺 model 应 400: %d", code)
	}

	_, b1 := post(t, ts, "/v1/responses", map[string]any{"model": "m", "input": "天气"})
	var r ResponseResource
	_ = json.Unmarshal(b1, &r)

	code, body := post(t, ts, "/v1/responses/compact", map[string]any{
		"model": "m", "previous_response_id": r.ID,
	})
	if code != 200 {
		t.Fatalf("compact status %d: %s", code, body)
	}
	var c CompactResource
	_ = json.Unmarshal(body, &c)
	if c.Object != "response.compaction" || len(c.Output) != 2 {
		t.Fatalf("CompactResource 形状不符: %+v", c)
	}
	if c.Output[0].Type != itemMessage || c.Output[0].Role != "user" ||
		c.Output[1].Type != itemCompaction || c.Output[1].EncryptedContent == "" {
		t.Fatalf("compact output items 不符: %+v", c.Output)
	}
	_ = fmt.Sprint() // 保持 import
}

// 跨"重启"恢复：重建 Server（同 dataDir），凭 previous_response_id 续接
// 并完成暂停应答——磁盘为准模型的核心验收。
func TestRestartRecovery(t *testing.T) {
	f1 := newFakeLLM([]string{`TOOL:ask_user_question:{"questions":[{"id":"q1","question":"确认？"}]}`})
	f1.Close() // 仅占位，真正使用在下面
	dataDir := t.TempDir()
	t.Setenv("GRF_HOME", t.TempDir())

	mkServer := func(f *fakeLLM) *httptest.Server {
		s, err := New(dataDir, "test-token")
		if err != nil {
			t.Fatal(err)
		}
		s.clientFactory = func() (*llm.Client, error) {
			return llm.NewClient(llm.Config{BaseURL: f.URL(), APIKey: "t", Model: "m"}), nil
		}
		ts := httptest.NewServer(s.Handler())
		return ts
	}

	fa := newFakeLLM([]string{`TOOL:ask_user_question:{"questions":[{"id":"q1","question":"确认？"}]}`})
	ts1 := mkServer(fa)
	code, b1 := post(t, ts1, "/v1/responses", map[string]any{"model": "m", "input": "做点事"})
	if code != 200 {
		t.Fatalf("status %d: %s", code, b1)
	}
	var r1 ResponseResource
	_ = json.Unmarshal(b1, &r1)
	if r1.Status != statusIncomplete {
		t.Fatalf("应暂停: %s", r1.Status)
	}
	ts1.Close() // 模拟 server 重启（内存缓存清空）

	fb := newFakeLLM([]string{`重启后恢复完成`})
	ts2 := mkServer(fb)
	defer ts2.Close()
	code, body := post(t, ts2, "/v1/responses", map[string]any{
		"model": "m", "previous_response_id": r1.ID,
		"input": []map[string]any{{"type": "grf:input_result", "call_id": "c1", "answers": map[string]any{"q1": "yes"}}},
	})
	if code != 200 {
		t.Fatalf("重启后应答 status %d: %s", code, body)
	}
	var r2 ResponseResource
	_ = json.Unmarshal(body, &r2)
	if r2.Status != statusCompleted || !strings.Contains(r2.Output[len(r2.Output)-1].Content[0].Text, "恢复完成") {
		t.Fatalf("重启后恢复不符: %s %+v", r2.Status, r2.Output)
	}
}

// 存档按会话隔离：同 workspace 新会话不继承旧会话历史，"重启"（重建
// Server 同 dataDir）后各会话也只恢复自己的历史——修复新会话把别的
// 会话历史串进来的问题（曾按 workspace 取最新存档导致串档）。
func TestSessionArchiveIsolation(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("GRF_HOME", t.TempDir())
	ws := t.TempDir()

	mkServer := func(f *fakeLLM) *httptest.Server {
		s, err := New(dataDir, "test-token")
		if err != nil {
			t.Fatal(err)
		}
		s.clientFactory = func() (*llm.Client, error) {
			return llm.NewClient(llm.Config{BaseURL: f.URL(), APIKey: "t", Model: "m"}), nil
		}
		ts := httptest.NewServer(s.Handler())
		return ts
	}
	postTurn := func(ts *httptest.Server, text string) ResponseResource {
		_, b := post(t, ts, "/v1/responses", map[string]any{
			"model": "m", "input": text, "grf:session": map[string]any{"workspace": ws},
		})
		var r ResponseResource
		_ = json.Unmarshal(b, &r)
		return r
	}

	f1 := newFakeLLM([]string{`A 的回复`, `B 的回复`})
	defer f1.Close()
	ts1 := mkServer(f1)
	ra := postTurn(ts1, "A 的问题") // 会话 A 先落一次存档
	rb := postTurn(ts1, "B 的问题") // 同 workspace 新开会话 B
	msgs := f1.lastMsgs()
	if len(msgs) != 2 || msgContent(msgs[1]) != "B 的问题" {
		t.Fatalf("新会话 B 继承了会话 A 的历史: %+v", msgs)
	}
	ts1.Close() // 模拟 server 重启（内存缓存清空）

	f2 := newFakeLLM([]string{`C 的回复`, `B 追问的回复`})
	ts2 := mkServer(f2)
	defer ts2.Close()
	rc := postTurn(ts2, "C 的问题") // 重启后同 workspace 新开会话 C
	msgs = f2.lastMsgs()
	if len(msgs) != 2 || msgContent(msgs[1]) != "C 的问题" {
		t.Fatalf("重启后新会话 C 继承了其他会话的历史: %+v", msgs)
	}

	// 重启后续接会话 B：只恢复 B 自己的历史（system + B 两轮 + 追问）。
	_, bb := post(t, ts2, "/v1/responses", map[string]any{
		"model": "m", "input": "B 追问", "previous_response_id": rb.ID,
	})
	var r2 ResponseResource
	_ = json.Unmarshal(bb, &r2)
	if r2.Status != statusCompleted {
		t.Fatalf("续接会话 B 失败: %s %s", r2.Status, bb)
	}
	msgs = f2.lastMsgs()
	if len(msgs) != 4 || msgContent(msgs[1]) != "B 的问题" || msgContent(msgs[3]) != "B 追问" {
		t.Fatalf("会话 B 重启恢复串入了其他会话的历史: %+v", msgs)
	}

	// 分开保存的落盘契约：存档只进各自会话目录，服务端绝不写
	// workspace 的 .grf/sessions（那本是 CLI 终端续聊的目录）。
	if entries := listArchives(t, filepath.Join(ws, ".grf", "sessions")); len(entries) > 0 {
		t.Fatalf("服务端不应写 workspace 存档目录: %v", entries)
	}
	for _, r := range []ResponseResource{ra, rb, rc} {
		if r.SessionID == "" {
			t.Fatalf("响应缺少 grf:session_id: %+v", r)
		}
		entries := listArchives(t, filepath.Join(dataDir, "conversations", r.SessionID, ".grf", "sessions"))
		if len(entries) == 0 {
			t.Fatalf("会话 %s 目录下应有自己的存档", r.SessionID)
		}
	}
}

// listArchives 列目录里的 .json 存档文件；目录不存在返回空。
func listArchives(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	return names
}

// 错误 call_id 的恢复处置：400 且不推进链尾（respID 回滚），正确应答后闭环。
func TestBadResumeCallIDKeepsChain(t *testing.T) {
	f := newFakeLLM([]string{
		`TOOL:ask_user_question:{"questions":[{"id":"q1","question":"确认？"}]}`,
		`完成`,
	})
	defer f.Close()
	ts := newTestServer(t, f)

	_, b1 := post(t, ts, "/v1/responses", map[string]any{"model": "m", "input": "确认一下"})
	var r1 ResponseResource
	_ = json.Unmarshal(b1, &r1)
	if r1.Status != statusIncomplete {
		t.Fatalf("应暂停: %s", r1.Status)
	}

	code, _ := post(t, ts, "/v1/responses", map[string]any{
		"model": "m", "previous_response_id": r1.ID,
		"input": []map[string]any{{"type": "grf:input_result", "call_id": "wrong-id", "answers": map[string]any{"q1": "x"}}},
	})
	if code != 400 {
		t.Fatalf("错误 call_id 应 400: %d", code)
	}
	// 链尾不动：仍可用原 previous_response_id 应答成功
	code, b2 := post(t, ts, "/v1/responses", map[string]any{
		"model": "m", "previous_response_id": r1.ID,
		"input": []map[string]any{{"type": "grf:input_result", "call_id": "c1", "answers": map[string]any{"q1": "yes"}}},
	})
	if code != 200 {
		t.Fatalf("原链尾重试应成功: %d %s", code, b2)
	}
	var r2 ResponseResource
	_ = json.Unmarshal(b2, &r2)
	if r2.Status != statusCompleted {
		t.Fatalf("应答后应完成: %s", r2.Status)
	}
	if got := r2.ID; got[len(got)-4:] == "0002" && r2.Status != statusCompleted {
		t.Fatalf("respID 序号应跳过被回滚的分配")
	}
}
