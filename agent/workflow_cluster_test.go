// workflow_cluster_test.go workflow 跨节点（M5c）：agent() 的 opts.node
// 命中集群节点时派往远端。假节点 = 极简 ORS 端点（POST 建任务即 completed
// + GET 终态快照），只实现路由层消费的 wire 子集。
package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"time"
	"testing"

	"git.gorun.fun/share/grf-harness/cluster"
	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/llm"
	"git.gorun.fun/share/grf-harness/tools"
)

// newFakeFarNode 极简远端节点：每次 POST /v1/responses 返回新 resp id，
// GET 该 id 返回 completed + 固定最终文本。
func newFakeFarNode(t *testing.T, finalText string, hits *int32) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer far-token" {
			w.WriteHeader(401)
			return
		}
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/responses":
			atomic.AddInt32(hits, 1)
			var req struct {
				Input string `json:"input"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "resp_far_1", "status": "queued",
			})
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/responses/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "resp_far_1", "status": "completed",
				"output": []any{map[string]any{
					"type": "message", "role": "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": finalText}},
				}},
			})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestWorkflowAgentCrossNode(t *testing.T) {
	var hits int32
	far := newFakeFarNode(t, "remote-done-42", &hits)
	router := cluster.NewRouter(&config.Cluster{
		Enabled: true, Role: "commander",
		Nodes: []config.ClusterNode{{Addr: far.URL, Token: "far-token", Name: "far"}},
	})
	router.SetPollInterval(20 * time.Millisecond)

	f := newFakeLLM([]string{"never-called"}) // 本地 LLM 不该被 agent() 触发
	h, err := Build(Options{
		Workspace: t.TempDir(),
		Client:    llm.NewClient(llm.Config{BaseURL: f.URL(), APIKey: "t", Model: "m"}),
		Settings:  &config.Settings{},
		Host:      &tools.NoopHost{},
		Cluster:   router,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	t.Cleanup(func() { h.Close() })

	script := `
const r = await agent("collect on the gpu box", {node: "@far"});
return r;
`
	args := `{"script":` + quoteJSON(script) + `,"meta":{"name":"cross","description":"cross-node agent"}}`
	res := h.Root.reg.Execute(context.Background(), "workflow", args)
	if !strings.Contains(res.Text, "remote-done-42") {
		t.Fatalf("workflow 跨节点结果应含远端文本：%s", res.Text)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("远端应恰好一次建任务，实际 %d", n)
	}
}

func TestWorkflowAgentUnknownNodeOpt(t *testing.T) {
	// opts.node 显式点名不存在的节点：走本地（ShouldRoute 不命中即本地）
	// ——默认本地语义不变，不因带了 node 字段而报错
	h, _ := wfHarness(t, []string{"local-fallback"})
	script := `
const r = await agent("local work", {node: "@nowhere"});
return r;
`
	args := `{"script":` + quoteJSON(script) + `,"meta":{"name":"n","description":"d"}}`
	res := h.Root.reg.Execute(context.Background(), "workflow", args)
	if !strings.Contains(res.Text, "local-fallback") {
		t.Fatalf("未知节点应回退本地：%s", res.Text)
	}
}
