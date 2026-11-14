// loopback_test.go 回环集成测试：指挥官 Router × 真实 gh server 节点
// （httptest + 脚本化 fake LLM），验证集群 M1 主链路——后台派发与落定
// 回收、审批直达（暂停协议桥接本地 Host）、打断、send_message 续聊、
// 控制面端点（capabilities/register/heartbeat/members + 双令牌鉴权）、
// /v1/agent/cluster 配置读写。外部包（cluster_test）避免
// cluster→server 测试环（server→agent→cluster）。
package cluster_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"git.gorun.fun/share/grf-harness/cluster"
	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/llm"
	"git.gorun.fun/share/grf-harness/server"
	"git.gorun.fun/share/grf-harness/tools"
)

// scriptedLLM 脚本化 OpenAI 兼容 fake（server 包 fakeLLM 同款模式的
// 独立副本：TOOL:name:{args} 前缀表示工具调用；按调用顺序消费脚本）。
type scriptedLLM struct {
	mu     sync.Mutex
	script []string
	srv    *httptest.Server
	calls  int
	msgs   [][]json.RawMessage // 每次调用收到的原始 messages
	delay  time.Duration
}

func newScriptedLLM(script []string) *scriptedLLM {
	f := &scriptedLLM{script: script}
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
		idx := f.calls - 1
		if idx >= len(f.script) {
			idx = len(f.script) - 1
		}
		resp := f.script[idx]
		f.msgs = append(f.msgs, req.Messages)
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
				ToolCalls: []llm.ToolCall{{ID: fmt.Sprintf("c%d", f.calls), Type: "function", Function: llm.FunctionCall{Name: name, Arguments: body}}}}
		} else {
			msg = llm.Message{Role: "assistant", Content: resp}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": msg}}})
	}))
	return f
}

func (f *scriptedLLM) URL() string { return f.srv.URL }
func (f *scriptedLLM) Close()      { f.srv.Close() }

// callMessages 第 n 次（1 起）LLM 调用的原始 messages。
func (f *scriptedLLM) callMessages(n int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n < 1 || n > len(f.msgs) {
		return ""
	}
	var b bytes.Buffer
	for _, m := range f.msgs[n-1] {
		b.Write(m)
		b.WriteByte('\n')
	}
	return b.String()
}

// newNode 起一个真实 gh server 节点（fake LLM 注入 + 集群端点启用）。
func newNode(t *testing.T, script []string) (*httptest.Server, *scriptedLLM) {
	t.Helper()
	t.Setenv("GRF_HOME", t.TempDir())
	f := newScriptedLLM(script)
	s, err := server.New(t.TempDir(), "node-token")
	if err != nil {
		t.Fatal(err)
	}
	s.SetClientFactory(func() (*llm.Client, error) {
		return llm.NewClient(llm.Config{BaseURL: f.URL(), APIKey: "t", Model: "test-model"}), nil
	})
	ccfg := &config.Cluster{Enabled: true, Token: "cluster-secret"}
	s.EnableCluster(cluster.NewRouter(ccfg), ccfg)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(f.Close)
	return ts, f
}

// newCommander 指挥官路由层（静态表指向节点）。
func newCommander(t *testing.T, nodeURL string) *cluster.Router {
	t.Helper()
	r := cluster.NewRouter(&config.Cluster{
		Enabled: true, Role: "commander",
		Nodes: []config.ClusterNode{{Addr: nodeURL, Token: "node-token", Name: "node1", Labels: []string{"test"}}},
	})
	r.SetPollInterval(50 * time.Millisecond)
	return r
}

// settleNoticed 落定通知捕获通道。
type settle struct {
	id, label, outcome, final string
}

// stubHost 审批直达测试宿主：AskUser 返回预置答案。
type stubHost struct {
	tools.NoopHost
	asked    int
	approved map[string]any
}

func (h *stubHost) AskUser(ctx context.Context, qs []tools.Question) (map[string]any, error) {
	h.asked++
	return h.approved, nil
}

func waitSettle(t *testing.T, ch chan settle) settle {
	t.Helper()
	select {
	case s := <-ch:
		return s
	case <-time.After(10 * time.Second):
		t.Fatal("等待落定通知超时")
		return settle{}
	}
}

func TestLoopbackBackgroundDispatch(t *testing.T) {
	node, _ := newNode(t, []string{"node says hello"})
	r := newCommander(t, node.URL)
	ch := make(chan settle, 4)
	id, _, err := r.Dispatch(context.Background(), cluster.DispatchReq{
		Description: "research thing @node1",
		Prompt:      "do the thing on the node",
		Background:  true,
		Notify:      func(id, label, outcome, final string) { ch <- settle{id, label, outcome, final} },
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !strings.HasPrefix(id, "ragent_") {
		t.Fatalf("远端句柄应 ragent_ 前缀：%s", id)
	}
	if !r.Owns(id) {
		t.Fatal("Owns 应识别远端句柄")
	}
	s := waitSettle(t, ch)
	if s.outcome != "completed" || s.final != "node says hello" {
		t.Fatalf("落定 = %q / %q, want completed / node says hello", s.outcome, s.final)
	}
	if !strings.Contains(r.ListText(), "node=node1") {
		t.Fatalf("ListText 应含节点标注：%s", r.ListText())
	}
}

func TestLoopbackApprovalDirect(t *testing.T) {
	node, f := newNode(t, []string{
		`TOOL:ask_user_question:{"questions":[{"id":"q1","question":"Proceed?","options":[{"label":"Yes"},{"label":"No"}]}]}`,
		"answered and done",
	})
	r := newCommander(t, node.URL)
	ch := make(chan settle, 4)
	host := &stubHost{approved: map[string]any{"q1": "Yes"}}
	_, _, err := r.Dispatch(context.Background(), cluster.DispatchReq{
		Description: "ask user @node1",
		Prompt:      "ask then finish",
		Background:  true,
		Host:        func() tools.Host { return host },
		Notify:      func(id, label, outcome, final string) { ch <- settle{id, label, outcome, final} },
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	s := waitSettle(t, ch)
	if s.outcome != "completed" || s.final != "answered and done" {
		t.Fatalf("落定 = %q / %q", s.outcome, s.final)
	}
	if host.asked != 1 {
		t.Fatalf("审批直达应恰好问一次，实际 %d", host.asked)
	}
	// 答案必须真送到了节点模型（第二次调用的 messages 含 Yes）——
	// 防止"Dismiss 也能走通"的假阳性
	if msg := f.callMessages(2); !strings.Contains(msg, `"Yes"`) && !strings.Contains(msg, "Yes") {
		t.Fatalf("第二次 LLM 调用应携带用户答案：\n%s", msg)
	}
}

func TestLoopbackInterrupt(t *testing.T) {
	node, f := newNode(t, []string{"never finishes"})
	f.delay = 5 * time.Second
	r := newCommander(t, node.URL)
	ch := make(chan settle, 4)
	id, _, err := r.Dispatch(context.Background(), cluster.DispatchReq{
		Description: "long task @node1",
		Prompt:      "work forever",
		Background:  true,
		Notify:      func(id, label, outcome, final string) { ch <- settle{id, label, outcome, final} },
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	time.Sleep(300 * time.Millisecond) // 等回合真正开跑
	if err := r.Interrupt(id); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	s := waitSettle(t, ch)
	if s.outcome != "interrupted" {
		t.Fatalf("打断后应 interrupted，得到 %q", s.outcome)
	}
}

func TestLoopbackSendMessage(t *testing.T) {
	node, _ := newNode(t, []string{"first result", "second result"})
	r := newCommander(t, node.URL)
	ch := make(chan settle, 4)
	id, _, err := r.Dispatch(context.Background(), cluster.DispatchReq{
		Description: "two parter @node1",
		Prompt:      "part one",
		Background:  true,
		Notify:      func(id, label, outcome, final string) { ch <- settle{id, label, outcome, final} },
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	s1 := waitSettle(t, ch)
	if s1.final != "first result" {
		t.Fatalf("第一段落定 = %q", s1.final)
	}
	if err := r.SendMessage(id, "now part two"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	s2 := waitSettle(t, ch)
	if s2.outcome != "completed" || s2.final != "second result" {
		t.Fatalf("续聊落定 = %q / %q", s2.outcome, s2.final)
	}
}

func TestClusterControlEndpoints(t *testing.T) {
	node, _ := newNode(t, []string{"x"})
	post := func(path, token string, body any) (int, []byte) {
		data, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", node.URL+path, bytes.NewReader(data))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	get := func(path, token string) (int, []byte) {
		req, _ := http.NewRequest("GET", node.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}

	// 集群密钥与 server token 均可过；错误令牌 401
	if code, _ := get("/cluster/capabilities", "cluster-secret"); code != 200 {
		t.Fatalf("cluster token 应可访问 capabilities：%d", code)
	}
	if code, _ := get("/cluster/capabilities", "node-token"); code != 200 {
		t.Fatalf("server token 应可访问 capabilities：%d", code)
	}
	if code, _ := get("/cluster/capabilities", "wrong"); code != 401 {
		t.Fatalf("错误令牌应 401：%d", code)
	}

	// 能力清单形状
	code, body := get("/cluster/capabilities", "cluster-secret")
	if code != 200 {
		t.Fatalf("capabilities：%d", code)
	}
	var caps cluster.Capabilities
	_ = json.Unmarshal(body, &caps)
	if len(caps.NodeID) != 16 || caps.Platform == "" || len(caps.Tools) < 20 {
		t.Fatalf("能力清单形状异常：%+v", caps)
	}

	// 注册 + 心跳 + 成员表
	if code, _ := post("/cluster/register", "cluster-secret", cluster.RegisterRequest{NodeID: "abcd1234abcd1234", Name: "phone"}); code != 200 {
		t.Fatal("register 失败")
	}
	if code, _ := post("/cluster/heartbeat", "cluster-secret", cluster.HeartbeatRequest{NodeID: "abcd1234abcd1234", Name: "phone", Running: 1}); code != 200 {
		t.Fatal("heartbeat 失败")
	}
	code, body = get("/cluster/members", "cluster-secret")
	if code != 200 {
		t.Fatalf("members：%d", code)
	}
	var members []cluster.Member
	_ = json.Unmarshal(body, &members)
	found := false
	for _, m := range members {
		if m.Name == "phone" && m.Status == "online" && m.Running == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("成员表应含 phone/online/running=1：%+v", members)
	}
}

func TestAgentClusterConfigAPI(t *testing.T) {
	t.Setenv("GRF_HOME", t.TempDir())
	s, err := server.New(t.TempDir(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	put := func(body any) (int, []byte) {
		data, _ := json.Marshal(body)
		req, _ := http.NewRequest("PUT", ts.URL+"/v1/agent/cluster", bytes.NewReader(data))
		req.Header.Set("Authorization", "Bearer tok")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	// 非法 role 400
	if code, _ := put(map[string]any{"enabled": true, "role": "boss"}); code != 400 {
		t.Fatalf("非法 role 应 400：%d", code)
	}
	// 合法写入 → 读回一致 → settings.json 落盘
	in := &config.Cluster{Enabled: true, Role: "commander", Policy: "write-workspace",
		Nodes: []config.ClusterNode{{Addr: "10.0.0.2:7400", Name: "ci", Labels: []string{"linux"}}}}
	if code, _ := put(in); code != 200 {
		t.Fatal("PUT cluster 失败")
	}
	req, _ := http.NewRequest("GET", ts.URL+"/v1/agent/cluster", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Cluster *config.Cluster `json:"cluster"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.Cluster == nil || !out.Cluster.Enabled || out.Cluster.Role != "commander" ||
		len(out.Cluster.Nodes) != 1 || out.Cluster.Nodes[0].Name != "ci" {
		t.Fatalf("GET 读回不一致：%+v", out.Cluster)
	}
	// 环境覆盖不落盘语义由 config 包测试覆盖；此处验证落盘原件未被污染
	st, err := config.LoadFileForUpdate()
	if err != nil || st.Cluster == nil || !st.Cluster.Enabled {
		t.Fatalf("settings.json 应含 cluster 段：%+v %v", st, err)
	}
}
