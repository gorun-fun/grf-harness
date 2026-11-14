// cluster_test.go 集群配置段：ClusterRuntime 环境覆盖与角色/路由默认值。
package config

import (
	"os"
	"testing"
)

func TestClusterRuntimeDisabledByDefault(t *testing.T) {
	if c := ClusterRuntime(nil); c != nil {
		t.Fatalf("nil settings 应返回 nil，得到 %+v", c)
	}
	if c := ClusterRuntime(&Settings{}); c != nil {
		t.Fatalf("无 cluster 段应返回 nil，得到 %+v", c)
	}
	disabled := &Settings{Cluster: &Cluster{Enabled: false, Role: "commander"}}
	if c := ClusterRuntime(disabled); c != nil {
		t.Fatalf("Enabled=false 应返回 nil，得到 %+v", c)
	}
}

func TestClusterRuntimeEnabled(t *testing.T) {
	s := &Settings{Cluster: &Cluster{Enabled: true, Role: "commander", Nodes: []ClusterNode{{Addr: "h:1"}}}}
	c := ClusterRuntime(s)
	if c == nil || !c.Enabled || !c.IsCommander() {
		t.Fatalf("应启用指挥官配置，得到 %+v", c)
	}
	// 值拷贝：改运行时副本不回写调用方 Settings
	c.Nodes[0].Addr = "changed"
	if s.Cluster.Nodes[0].Addr != "h:1" {
		t.Fatal("ClusterRuntime 应返回值拷贝，不应共享底层数组")
	}
	// node 角色不是指挥官
	if (&Cluster{Enabled: true}).IsCommander() {
		t.Fatal("默认 node 角色不应判为指挥官")
	}
}

func TestClusterRuntimeEnvOverride(t *testing.T) {
	cases := []struct {
		env  string
		want bool
	}{
		{"1", true}, {"true", true}, {"YES", true}, {"on", true},
		{"0", false}, {"false", false}, {"No", false}, {"off", false},
		{"", true},      // 未设置：跟随 settings（本例 Enabled=true）
		{"weird", true}, // 未识别值忽略
	}
	s := &Settings{Cluster: &Cluster{Enabled: true}}
	t.Setenv("GRF_CLUSTER_ENABLED", "")
	for _, c := range cases {
		os.Unsetenv("GRF_CLUSTER_ENABLED")
		if c.env != "" {
			t.Setenv("GRF_CLUSTER_ENABLED", c.env)
		}
		if got := ClusterRuntime(s) != nil; got != c.want {
			t.Fatalf("GRF_CLUSTER_ENABLED=%q 启用=%v, want %v", c.env, got, c.want)
		}
	}
	// 无 cluster 段 + 环境强开：合成空配置
	os.Unsetenv("GRF_CLUSTER_ENABLED")
	t.Setenv("GRF_CLUSTER_ENABLED", "1")
	c := ClusterRuntime(&Settings{})
	if c == nil || !c.Enabled || c.IsCommander() {
		t.Fatalf("环境强开无段配置应得 &Cluster{Enabled:true}，得到 %+v", c)
	}
}

func TestClusterRouteDefaults(t *testing.T) {
	if !(&Cluster{}).RouteAllowLocal() {
		t.Fatal("默认应回退本地")
	}
	no := false
	if (&Cluster{Route: &ClusterRoute{AllowLocal: &no}}).RouteAllowLocal() {
		t.Fatal("显式 false 应关闭本地回退")
	}
	yes := true
	if !(&Cluster{Route: &ClusterRoute{AllowLocal: &yes}}).RouteAllowLocal() {
		t.Fatal("显式 true 应保持回退")
	}
	if n := (&Cluster{}).RouteMaxInflight(); n != 4 {
		t.Fatalf("默认单节点在跑上限 = 4，得到 %d", n)
	}
	if n := (&Cluster{Route: &ClusterRoute{MaxInflightPerNode: 7}}).RouteMaxInflight(); n != 7 {
		t.Fatalf("显式上限应生效，得到 %d", n)
	}
}
