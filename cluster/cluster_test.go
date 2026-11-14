// cluster_test.go 集群包单元测试：路由匹配、身份持久化、成员表。
package cluster

import (
	"strings"
	"testing"

	"git.gorun.fun/share/grf-harness/config"
)

func testCfg() *config.Cluster {
	return &config.Cluster{
		Enabled: true,
		Role:    "commander",
		Nodes: []config.ClusterNode{
			{Addr: "10.0.0.2:7400", Name: "linux-ci", Labels: []string{"linux", "ci"}},
			{Addr: "10.0.0.3:7400", Labels: []string{"windows"}},
		},
	}
}

func TestPickMatching(t *testing.T) {
	r := NewRouter(testCfg())
	cases := []struct {
		desc string
		want string // 期望命中的路由名；"" = 不路由
	}{
		{"deploy the build @linux-ci please", "linux-ci"},
		{"@Linux-CI uppercase", "linux-ci"},        // 大小写不敏感
		{"run on @10.0.0.3", "10.0.0.3"},           // 无名节点回退 addr 主机名
		{"this needs the windows box", "10.0.0.3"}, // label 整词命中
		{"a ci job for the build", "linux-ci"},     // label "ci"
		{"just a local task", ""},
		{"linuxy wording should not match", ""}, // 非整词（子串）不命中
	}
	for _, c := range cases {
		n := r.pick(c.desc)
		got := ""
		if n != nil {
			got = NodeName(*n)
		}
		if got != c.want {
			t.Errorf("pick(%q) = %q, want %q", c.desc, got, c.want)
		}
	}
	if r.ShouldRoute("do it @linux-ci") != true || r.ShouldRoute("no node here") != false {
		t.Error("ShouldRoute 与 pick 不一致")
	}
}

func TestPromptLines(t *testing.T) {
	r := NewRouter(testCfg())
	lines := r.PromptLines()
	if len(lines) != 2 {
		t.Fatalf("PromptLines 应 2 行，得到 %d", len(lines))
	}
	if !strings.Contains(lines[0], "linux-ci") || !strings.Contains(lines[0], "labels=linux/ci") {
		t.Errorf("首行应含节点名与 labels：%s", lines[0])
	}
	if !strings.Contains(lines[1], "10.0.0.3:7400") {
		t.Errorf("次行应含 addr：%s", lines[1])
	}
}

func TestIdentityPersist(t *testing.T) {
	t.Setenv("GRF_HOME", t.TempDir())
	a, err := LoadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if len(a.ID) != 16 || a.Name == "" {
		t.Fatalf("身份应 16 hex + 非空名，得到 %+v", a)
	}
	b, err := LoadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID || a.Name != b.Name {
		t.Fatalf("身份应跨调用稳定：%+v vs %+v", a, b)
	}
}

func TestTableRegisterHeartbeat(t *testing.T) {
	tb := NewTable([]config.ClusterNode{{Addr: "h:1", Name: "seed", Labels: []string{"x"}}})
	tb.Register(Member{NodeID: "n1", Name: "phone", Addr: "h:9"})
	tb.Heartbeat("n1", "phone", 2)

	snap := tb.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("成员应 2 个，得到 %d", len(snap))
	}
	byName := map[string]Member{}
	for _, m := range snap {
		byName[m.Name] = m
	}
	if m := byName["seed"]; m.Source != SourceStatic || m.Status != StatusUnknown {
		t.Errorf("静态成员应 unknown：%+v", m)
	}
	if m := byName["phone"]; m.Source != SourceRegistered || m.Status != StatusOnline || m.Running != 2 || m.NodeID != "n1" {
		t.Errorf("注册成员应 online/running=2：%+v", m)
	}
	// 未知节点心跳 = 自动入表
	tb.Heartbeat("n2", "tablet", 0)
	if got := len(tb.Snapshot()); got != 3 {
		t.Fatalf("匿名心跳应自动入表，成员 %d", got)
	}
	// 同名再注册：保留静态属性、刷新状态
	tb.Register(Member{NodeID: "n3", Name: "seed"})
	for _, m := range tb.Snapshot() {
		if m.Name == "seed" {
			if m.Source != SourceStatic || m.NodeID != "n3" || m.Status != StatusOnline {
				t.Errorf("同名注册应保留 static 来源并刷新状态：%+v", m)
			}
		}
	}
}

func TestHostOf(t *testing.T) {
	cases := map[string]string{
		"10.0.0.3:7400":        "10.0.0.3",
		"http://10.0.0.3:7400": "10.0.0.3",
		"https://a.b/c":        "a.b",
		"host":                 "host",
	}
	for in, want := range cases {
		if got := HostOf(in); got != want {
			t.Errorf("HostOf(%q) = %q, want %q", in, got, want)
		}
	}
}
