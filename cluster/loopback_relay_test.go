// loopback_relay_test.go 中继链路回环（M3b/M5b）：NAT 节点拓扑——节点
// 不进指挥官静态表、不可直连，仅靠 call-home（注册无 addr）+ 收件箱 SSE
// 指令通道 + /cluster/events 落定回投完成整个任务闭环。真 gh server 双
// 实例 + 真 InboxLoop（SSE 客户端），与生产唯一的差别是指令官/节点同进程。
package cluster_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.gorun.fun/share/grf-harness/cluster"
	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/llm"
	"git.gorun.fun/share/grf-harness/server"
	"git.gorun.fun/share/grf-harness/tools"
)

// relayCluster NAT 拓扑双实例：指挥官（静态表为空——节点不可直连）+
// NAT 节点（真 server，commander_url 指向指挥官；InboxLoop 挂指挥官收件箱）。
type relayCluster struct {
	cmdrSrv *httptest.Server
	cmdr    *cluster.Router
	nodeLLM *scriptedLLM
}

func newRelayCluster(t *testing.T, nodeScript []string) *relayCluster {
	t.Helper()
	t.Setenv("GRF_HOME", t.TempDir())

	// 指挥官：真 server + 路由层（静态表空 = 一切派发只能中继）
	cmdrRouter := cluster.NewRouter(&config.Cluster{
		Enabled: true, Role: "commander", Token: "cluster-secret",
	})
	cmdrRouter.SetPollInterval(50 * time.Millisecond)
	cs, err := server.New(t.TempDir(), "cmdr-token")
	if err != nil {
		t.Fatal(err)
	}
	cs.EnableCluster(cmdrRouter, &config.Cluster{Enabled: true, Role: "commander", Token: "cluster-secret"})
	cmdrTS := httptest.NewServer(cs.Handler())

	// NAT 节点：真 server，commander_url 指回指挥官（回投通道）
	f := newScriptedLLM(nodeScript)
	ns, err := server.New(t.TempDir(), "node-token")
	if err != nil {
		t.Fatal(err)
	}
	ns.SetClientFactory(func() (*llm.Client, error) {
		return llm.NewClient(llm.Config{BaseURL: f.URL(), APIKey: "t", Model: "test-model"}), nil
	})
	nodeCfg := &config.Cluster{
		Enabled: true, Token: "cluster-secret",
		CommanderURL: cmdrTS.URL,
	}
	ns.EnableCluster(cluster.NewRouter(nodeCfg), nodeCfg)
	nodeTS := httptest.NewServer(ns.Handler()) // 节点自己的监听存在但指挥官不知道地址（NAT）
	t.Cleanup(nodeTS.Close)

	// 节点 call-home 注册（无 addr）+ 指令收件箱（真 SSE 客户端）
	self := cluster.Identity{ID: "ffff000011112222", Name: "natnode"}
	ctx, cancel := context.WithCancel(context.Background())
	c := cluster.NewNodeClient(cmdrTS.URL, "cluster-secret")
	if err := c.Register(ctx, cluster.RegisterRequest{NodeID: self.ID, Name: self.Name}); err != nil {
		cancel()
		t.Fatalf("register: %v", err)
	}
	go cluster.InboxLoop(ctx, cmdrTS.URL, "cluster-secret", self, ns.ExecuteClusterCommand)

	// 等收件箱真正挂上（SSE 握手完成），否则首个派发会撞"未连接"
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cmdrRouter.InboxConnected("natnode") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !cmdrRouter.InboxConnected("natnode") {
		cancel()
		t.Fatal("节点收件箱 5s 内未挂上指挥官")
	}

	t.Cleanup(f.Close)
	t.Cleanup(cmdrTS.Close)
	// cancel 最后注册（LIFO 最先执行）：先断 SSE 再关服务，否则
	// httptest.Close 会等 inbox 长连等到超时
	t.Cleanup(cancel)
	return &relayCluster{cmdrSrv: cmdrTS, cmdr: cmdrRouter, nodeLLM: f}
}

// TestLoopbackRelayDispatch 中继全链路：task 指令 → 节点跑回合 →
// 落定回投 → 指挥官通知；再 send_message 续聊（continue_of 链式）。
func TestLoopbackRelayDispatch(t *testing.T) {
	c := newRelayCluster(t, []string{"relay says hi", "relay second turn"})
	ch := make(chan settle, 4)
	id, _, err := c.cmdr.Dispatch(context.Background(), cluster.DispatchReq{
		Description: "work behind nat @natnode",
		Prompt:      "do the relay thing",
		Background:  true,
		Notify:      func(id, label, outcome, final string) { ch <- settle{id, label, outcome, final} },
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	s1 := waitSettle(t, ch)
	if s1.outcome != "completed" || s1.final != "relay says hi" {
		t.Fatalf("中继落定 = %q / %q, want completed / relay says hi", s1.outcome, s1.final)
	}
	if !c.cmdr.InboxConnected("natnode") {
		t.Fatal("收件箱应保持连接")
	}
	// 续聊：中继 follow-up 指令 → 节点链式续跑
	if err := c.cmdr.SendMessage(id, "one more thing"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	s2 := waitSettle(t, ch)
	if s2.outcome != "completed" || s2.final != "relay second turn" {
		t.Fatalf("中继续聊落定 = %q / %q", s2.outcome, s2.final)
	}
}

// TestLoopbackRelayApproval 中继暂停直达：节点 ask_user 暂停 → 回投事件
// 带 grf_requests → 指挥官 Host 应答 → resume 指令 → 节点续跑完成。
func TestLoopbackRelayApproval(t *testing.T) {
	c := newRelayCluster(t, []string{
		`TOOL:ask_user_question:{"questions":[{"id":"q1","question":"Proceed?","options":[{"label":"Yes"},{"label":"No"}]}]}`,
		"relay answered",
	})
	ch := make(chan settle, 4)
	host := &stubHost{approved: map[string]any{"q1": "Yes"}}
	_, _, err := c.cmdr.Dispatch(context.Background(), cluster.DispatchReq{
		Description: "ask behind nat @natnode",
		Prompt:      "ask then finish",
		Background:  true,
		Host:        func() tools.Host { return host },
		Notify:      func(id, label, outcome, final string) { ch <- settle{id, label, outcome, final} },
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	s := waitSettle(t, ch)
	if s.outcome != "completed" || s.final != "relay answered" {
		t.Fatalf("中继审批落定 = %q / %q", s.outcome, s.final)
	}
	if host.asked != 1 {
		t.Fatalf("中继暂停直达应恰好问一次，实际 %d", host.asked)
	}
	// 答案真达节点模型（第二次 LLM 调用含 Yes）
	if msg := c.nodeLLM.callMessages(2); !strings.Contains(msg, "Yes") {
		t.Fatalf("节点第二轮应携带答案：\n%s", msg)
	}
}

// TestLoopbackRelayNotConnected 未挂收件箱的中继派发应明确报错（而非
// 静默排队）——模型需要可行动的指引。
func TestLoopbackRelayNotConnected(t *testing.T) {
	r2 := cluster.NewRouter(&config.Cluster{Enabled: true, Role: "commander", Token: "cluster-secret"})
	r2.Table().Register(cluster.Member{NodeID: "id1", Name: "ghost"})
	_, _, err := r2.Dispatch(context.Background(), cluster.DispatchReq{
		Description: "talk @ghost",
		Prompt:      "x",
		Background:  false,
	})
	if err == nil || !strings.Contains(err.Error(), "command inbox") {
		t.Fatalf("未连接中继应报 inbox 错误，得到 %v", err)
	}
}

// TestLoopbackInviteGate 邀请码门（M3a）：配置 invite_code 的指挥官拒绝
// 无码/错码注册与未知节点心跳；对码放行且后续心跳免验。
func TestLoopbackInviteGate(t *testing.T) {
	t.Setenv("GRF_HOME", t.TempDir())
	r := cluster.NewRouter(&config.Cluster{Enabled: true, Role: "commander", Token: "cluster-secret", InviteCode: "open-sesame"})
	cs, err := server.New(t.TempDir(), "cmdr-token")
	if err != nil {
		t.Fatal(err)
	}
	cs.EnableCluster(r, &config.Cluster{Enabled: true, Role: "commander", Token: "cluster-secret", InviteCode: "open-sesame"})
	ts := httptest.NewServer(cs.Handler())
	t.Cleanup(ts.Close)

	post := func(path string, body any) int {
		data, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", ts.URL+path, bytes.NewReader(data))
		req.Header.Set("Authorization", "Bearer cluster-secret")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	// 无码/错码注册拒绝
	if code := post("/cluster/register", cluster.RegisterRequest{NodeID: "ab", Name: "x"}); code != 403 {
		t.Fatalf("无邀请码注册应 403：%d", code)
	}
	if code := post("/cluster/register", cluster.RegisterRequest{NodeID: "ab", Name: "x", Invite: "wrong"}); code != 403 {
		t.Fatalf("错邀请码注册应 403：%d", code)
	}
	// 未知节点心跳无码也拒（防绕过 register）
	if code := post("/cluster/heartbeat", cluster.HeartbeatRequest{NodeID: "ab", Name: "stranger"}); code != 403 {
		t.Fatalf("未知节点无码心跳应 403：%d", code)
	}
	// 对码注册放行
	if code := post("/cluster/register", cluster.RegisterRequest{NodeID: "abcd", Name: "ok-node", Invite: "open-sesame"}); code != 200 {
		t.Fatalf("对码注册应 200：%d", code)
	}
	// 已知节点心跳免验（不带码也过——掉线重连体验）
	if code := post("/cluster/heartbeat", cluster.HeartbeatRequest{NodeID: "abcd", Name: "ok-node", Running: 1}); code != 200 {
		t.Fatalf("已知节点心跳免验应 200：%d", code)
	}
}
