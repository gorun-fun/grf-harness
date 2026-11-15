// cluster_files_test.go 集群文件传输 server 侧单元测试：对象端点鉴权与
// 集群未启用 404、无效 seed_ref 建会话失败（400 回派发方——路由层把
// 错误转成派发失败，不留悬挂回合）。
package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.gorun.fun/share/grf-harness/cluster"
	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/llm"
)

// newClusterTestServer 集群启用的测试节点（EnableCluster 必须在
// Handler() 之前，见 server_cmd 装配说明）。
func newClusterTestServer(t *testing.T, f *fakeLLM) (*httptest.Server, *Server) {
	t.Helper()
	t.Setenv("GRF_HOME", t.TempDir())
	s, err := New(t.TempDir(), "test-token")
	if err != nil {
		t.Fatal(err)
	}
	s.clientFactory = func() (*llm.Client, error) {
		return llm.NewClient(llm.Config{BaseURL: f.URL(), APIKey: "t", Model: "test-model"}), nil
	}
	cfg := &config.Cluster{Enabled: true, Token: "cluster-secret"}
	s.EnableCluster(cluster.NewRouter(cfg), cfg)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, s
}

func TestClusterObjAuthAndDisabled(t *testing.T) {
	t.Setenv("GRF_HOME", t.TempDir())
	f := newFakeLLM([]string{"unused"})
	// 集群未启用：/cluster/files/obj 未注册（404 由 mux 兜底）
	ts := newTestServer(t, f)
	resp, err := http.Post(ts.URL+"/cluster/files/obj", "application/gzip", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("未启用集群应 404，得到 %d", resp.StatusCode)
	}

	// 启用后：无令牌 401；集群共享密钥 200 + ref
	ts2, _ := newClusterTestServer(t, f)
	resp, err = http.Post(ts2.URL+"/cluster/files/obj", "application/gzip", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("无令牌应 401，得到 %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodPost, ts2.URL+"/cluster/files/obj", strings.NewReader("payload"))
	req.Header.Set("Authorization", "Bearer cluster-secret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(buf.String(), `"ref"`) {
		t.Fatalf("集群密钥推送应 200 + ref，得到 %d %s", resp.StatusCode, buf.String())
	}
}

func TestClusterSeedInvalid(t *testing.T) {
	f := newFakeLLM([]string{"never"})
	ts, _ := newClusterTestServer(t, f)
	// 无效种子句柄：建会话阶段即失败（400），错误信息含句柄便于排障
	body := `{"input":"hi","background":true,"grf:session":{"seed_ref":"deadbeefdeadbeef","return_files":["out/*"]}}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/responses", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("无效 seed_ref 应 400，得到 %d", resp.StatusCode)
	}
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	if !strings.Contains(buf.String(), "deadbeefdeadbeef") {
		t.Fatalf("错误应含句柄：%s", buf.String())
	}
}
