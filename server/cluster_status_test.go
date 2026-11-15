// cluster_status_test.go /v1/agent/cluster/status 端点：形状与鉴权。
// 数据面（成员/收件箱/句柄）由 cluster 包回环覆盖，这里只验装配。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"git.gorun.fun/share/grf-harness/cluster"
	"git.gorun.fun/share/grf-harness/config"
)

func TestAgentClusterStatus(t *testing.T) {
	t.Setenv("GRF_HOME", t.TempDir())
	s, err := New(t.TempDir(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	ccfg := &config.Cluster{Enabled: true, Role: "commander", Token: "ct", Sync: "full", Discover: "lan"}
	router := cluster.NewRouter(ccfg)
	router.Table().Register(cluster.Member{NodeID: "n1", Name: "phone"})
	s.EnableCluster(router, ccfg)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	get := func(token string) (int, map[string]any) {
		req, _ := http.NewRequest("GET", ts.URL+"/v1/agent/cluster/status", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if code, _ := get("wrong"); code != 401 {
		t.Fatalf("错令牌应 401：%d", code)
	}
	code, st := get("tok")
	if code != 200 {
		t.Fatalf("status：%d", code)
	}
	if st["enabled"] != true || st["role"] != "commander" || st["sync"] != "full" || st["discover"] != "lan" {
		t.Fatalf("status 基本字段不符：%v", st)
	}
	if _, ok := st["self"].(map[string]any); !ok {
		t.Fatalf("self 段缺失：%v", st)
	}
	if members, ok := st["members"].([]any); !ok || len(members) != 1 {
		t.Fatalf("members 应含注册成员：%v", st["members"])
	}

	// 未启用集群的 server：enabled=false 而非 404（面板先探测再引导配置）
	s2, err := New(t.TempDir(), "tok2")
	if err != nil {
		t.Fatal(err)
	}
	ts2 := httptest.NewServer(s2.Handler())
	t.Cleanup(ts2.Close)
	req, _ := http.NewRequest("GET", ts2.URL+"/v1/agent/cluster/status", nil)
	req.Header.Set("Authorization", "Bearer tok2")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st2 map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&st2)
	if resp.StatusCode != 200 || st2["enabled"] != false {
		t.Fatalf("未启用应 200 + enabled=false：%d %v", resp.StatusCode, st2)
	}
}
