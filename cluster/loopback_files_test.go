// loopback_files_test.go 集群文件传输 M2 回环：种子下发（指挥官打包 →
// 节点解包进会话工作区）、产物回传（节点落定打包 → 推回指挥官共享区 →
// Artifacts 回调解包）、门禁拒绝（sync 未开启时报错引导）。拓扑与生产
// 一致：真 gh server 双实例（节点 + 指挥官），共享密钥鉴权，后台派发。
package cluster_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.gorun.fun/share/grf-harness/cluster"
	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/llm"
	"git.gorun.fun/share/grf-harness/server"
	"git.gorun.fun/share/grf-harness/tools"
)

// m2Cluster 生产拓扑双实例：指挥官（真 server + 路由层，sync=seed）与
// 节点（真 server，call-home 配置指向指挥官——产物回传的通道）。
type m2Cluster struct {
	cmdrSrv *httptest.Server
	cmdr    *cluster.Router
	nodeSrv *httptest.Server
	nodeLLM *scriptedLLM
}

func newM2Cluster(t *testing.T, nodeScript []string) *m2Cluster {
	t.Helper()
	t.Setenv("GRF_HOME", t.TempDir())

	// 指挥官：真 server（收产物推送）+ 指挥官路由层（派发/解包共用实例）
	cmdrRouter := cluster.NewRouter(&config.Cluster{
		Enabled: true, Role: "commander", Token: "cluster-secret", Sync: "seed",
	})
	cmdrRouter.SetPollInterval(50 * time.Millisecond)
	cs, err := server.New(t.TempDir(), "cmdr-token")
	if err != nil {
		t.Fatal(err)
	}
	cs.EnableCluster(cmdrRouter, &config.Cluster{
		Enabled: true, Role: "commander", Token: "cluster-secret", Sync: "seed",
	})
	cmdrTS := httptest.NewServer(cs.Handler())

	// 节点：真 server，cluster 配置带 commander_url（产物回传地址）
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
		CommanderURL: cmdrTS.URL, // 产物回传通道（call-home 同款地址）
	}
	ns.EnableCluster(cluster.NewRouter(nodeCfg), nodeCfg)
	nodeTS := httptest.NewServer(ns.Handler())

	// 指挥官路由层的静态节点表事后注入（地址在两实例都起好后才已知）
	cmdrRouter.AddNode(config.ClusterNode{Addr: nodeTS.URL, Token: "node-token", Name: "node1"})

	t.Cleanup(cmdrTS.Close)
	t.Cleanup(nodeTS.Close)
	t.Cleanup(f.Close)
	return &m2Cluster{cmdrSrv: cmdrTS, cmdr: cmdrRouter, nodeSrv: nodeTS, nodeLLM: f}
}

// TestLoopbackSeedDispatch 种子下发：指挥官工作区文件经 seed_files 打包
// 推到节点、解包进远端会话工作区——节点 agent 用 read 读到同一内容。
func TestLoopbackSeedDispatch(t *testing.T) {
	c := newM2Cluster(t, []string{
		`TOOL:read:{"file_path":"seed.txt"}`,
		"read done",
	})
	// 指挥官工作区 + 读边界策略（顺带验证 CheckRead 参与种子打包）
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "seed.txt"), []byte("seed-payload-42"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "outside-secret.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	fsp := tools.NewFsPolicy(ws, false, true, nil)

	ch := make(chan settle, 4)
	_, _, err := c.cmdr.Dispatch(context.Background(), cluster.DispatchReq{
		Description: "read seeded @node1",
		Prompt:      "read seed.txt and report",
		Background:  true,
		SeedFiles:   []string{"seed.txt"},
		Fs:          fsp,
		Notify:      func(id, label, outcome, final string) { ch <- settle{id, label, outcome, final} },
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	s := waitSettle(t, ch)
	if s.outcome != "completed" {
		t.Fatalf("落定 outcome = %q（final=%q）", s.outcome, s.final)
	}
	// 节点第二次 LLM 调用（read 工具结果之后）必须含种子内容
	msgs := c.nodeLLM.callMessages(2)
	if !strings.Contains(msgs, "seed-payload-42") {
		t.Fatalf("节点第二轮未见种子内容：\n%s", msgs)
	}
	if strings.Contains(msgs, "outside-secret.txt") || strings.Contains(msgs, "nope") {
		t.Fatalf("未列入 seed_files 的文件不应被带到节点")
	}
}

// TestLoopbackArtifactsReturn 产物回传：节点 write 落文件 + return_files
// 声明 → 落定后打包推回指挥官共享区 → Artifacts 回调拿到 ref 并可解包。
func TestLoopbackArtifactsReturn(t *testing.T) {
	c := newM2Cluster(t, []string{
		`TOOL:write:{"file_path":"out/report.txt","content":"artifact-body-7"}`,
		"wrote the report",
	})
	ws := t.TempDir()

	ch := make(chan settle, 4)
	arts := make(chan []cluster.ArtifactMeta, 4)
	_, _, err := c.cmdr.Dispatch(context.Background(), cluster.DispatchReq{
		Description: "produce report @node1",
		Prompt:      "write out/report.txt with the artifact body",
		Background:  true,
		ReturnFiles: []string{"out/*.txt"},
		Fs:          tools.NewFsPolicy(ws, false, false, nil),
		Notify:      func(id, label, outcome, final string) { ch <- settle{id, label, outcome, final} },
		Artifacts: func(id string, list []cluster.ArtifactMeta) {
			arts <- list
		},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	s := waitSettle(t, ch)
	if s.outcome != "completed" {
		t.Fatalf("落定 outcome = %q（final=%q）", s.outcome, s.final)
	}
	var list []cluster.ArtifactMeta
	select {
	case list = <-arts:
	case <-time.After(5 * time.Second):
		t.Fatal("Artifacts 回调未触发")
	}
	if len(list) != 1 || list[0].Ref == "" || list[0].Error != "" {
		t.Fatalf("产物清单异常：%+v", list)
	}
	if len(list[0].Files) != 1 || list[0].Files[0] != "out/report.txt" {
		t.Fatalf("产物文件清单不符：%v", list[0].Files)
	}
	// 指挥官本地解包（agent 侧 unpackArtifacts 的核心动作）
	f, _, err := c.cmdr.Share().Open(list[0].Ref)
	if err != nil {
		t.Fatalf("Open 产物对象：%v", err)
	}
	dest := filepath.Join(ws, "cluster-artifacts", "ragent_test")
	n, err := cluster.UnpackTo(f, dest)
	f.Close()
	if err != nil || n != 1 {
		t.Fatalf("解包 = %d, %v", n, err)
	}
	body, err := os.ReadFile(filepath.Join(dest, "out", "report.txt"))
	if err != nil || string(body) != "artifact-body-7" {
		t.Fatalf("产物内容不符：%q %v", body, err)
	}
}

// TestLoopbackSeedGateOff 门禁：sync 未开启时 seed_files 被拒并引导回退
// 自包含 prompt。
func TestLoopbackSeedGateOff(t *testing.T) {
	node, _ := newNode(t, []string{"never runs"})
	r := cluster.NewRouter(&config.Cluster{
		Enabled: true, Role: "commander",
		Nodes: []config.ClusterNode{{Addr: node.URL, Token: "node-token", Name: "node1"}},
	})
	ws := t.TempDir()
	_ = os.WriteFile(filepath.Join(ws, "seed.txt"), []byte("x"), 0o644)
	_, _, err := r.Dispatch(context.Background(), cluster.DispatchReq{
		Description: "gated @node1",
		Prompt:      "p",
		SeedFiles:   []string{"seed.txt"},
		Fs:          tools.NewFsPolicy(ws, false, false, nil),
	})
	if err == nil || !strings.Contains(err.Error(), "cluster.sync") {
		t.Fatalf("门禁应拒绝并提示 cluster.sync，得到：%v", err)
	}
}

// TestLoopbackSeedBadRef 种子句柄无效：节点建会话失败，派发侧拿到明确
// 错误（而非悬挂）。
func TestLoopbackSeedBadRef(t *testing.T) {
	node, _ := newNode(t, []string{"never runs"})
	r := cluster.NewRouter(&config.Cluster{
		Enabled: true, Role: "commander", Sync: "seed",
		Nodes: []config.ClusterNode{{Addr: node.URL, Token: "node-token", Name: "node1"}},
	})
	_, out, err := r.Dispatch(context.Background(), cluster.DispatchReq{
		Description: "bad seed @node1",
		Prompt:      "p",
		SeedFiles:   []string{"../escape.txt"}, // 越出工作区 → 打包即拒
		Fs:          tools.NewFsPolicy(t.TempDir(), false, true, nil),
	})
	if err == nil {
		t.Fatalf("越界种子应打包失败（out=%q）", out)
	}
}

// TestClusterObjEndpoint 集群对象端点鉴权：无令牌 401、集群密钥 200。
func TestClusterObjEndpoint(t *testing.T) {
	c := newM2Cluster(t, nil)
	// 无令牌
	resp, err := http.Post(c.cmdrSrv.URL+"/cluster/files/obj", "application/gzip", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("无令牌应 401，得到 %d", resp.StatusCode)
	}
	// 集群共享密钥推一个对象
	req, _ := http.NewRequest(http.MethodPost, c.cmdrSrv.URL+"/cluster/files/obj", strings.NewReader("payload"))
	req.Header.Set("Authorization", "Bearer cluster-secret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"ref"`) {
		t.Fatalf("推送应 200 + ref，得到 %d %s", resp.StatusCode, body)
	}
}
