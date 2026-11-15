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
		tgt := r.pickTarget(c.desc)
		got := ""
		if tgt != nil {
			got = tgt.name()
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

// TestPromptLinesLiveMerge 能力注入：注册成员（带心跳/能力）后静态行
// 显示实时状态与能力摘要；call-home 独有成员单独成行并标注中继可派发性。
func TestPromptLinesLiveMerge(t *testing.T) {
	r := NewRouter(testCfg())
	// 静态节点收到心跳 + 能力
	r.Table().Register(Member{
		NodeID: "n1", Name: "linux-ci",
		Caps: &Capabilities{Models: []string{"a/x", "b/y"}, Plugins: []PluginCaps{{Name: "grf-browser"}}, Platform: "linux/arm64"},
	})
	r.Table().Heartbeat("n1", "linux-ci", 2)
	// call-home 独有节点（无静态条目）
	r.Table().Register(Member{NodeID: "n2", Name: "phone", Addr: "192.168.3.9:7400",
		Caps: &Capabilities{Models: []string{"c/z"}}})
	lines := r.PromptLines()
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "status=online") {
		t.Errorf("静态行应显示在线状态：%s", joined)
	}
	if !strings.Contains(joined, "running=2") {
		t.Errorf("静态行应显示负载：%s", joined)
	}
	if !strings.Contains(joined, "models=2") || !strings.Contains(joined, "plugin=grf-browser") || !strings.Contains(joined, "platform=linux/arm64") {
		t.Errorf("静态行应含能力摘要：%s", joined)
	}
	if !strings.Contains(joined, "phone") || !strings.Contains(joined, "discovered via registered") {
		t.Errorf("call-home 成员应单独成行：%s", joined)
	}
	if !strings.Contains(joined, "not dispatchable yet") {
		t.Errorf("未挂收件箱的发现成员应标注不可派发：%s", joined)
	}
	// 挂上收件箱 → 标注翻转
	_, unsub := r.InboxSubscribe("phone")
	defer unsub()
	joined = strings.Join(r.PromptLines(), "\n")
	if !strings.Contains(joined, "dispatchable via the command inbox relay") {
		t.Errorf("挂上收件箱后应标注中继可派发：%s", joined)
	}
}

// TestPickTargetRelay 中继路由：无静态条目的注册成员可 @name 命中，
// 走 member 分支；同名静态条目（有 addr）优先直连。
func TestPickTargetRelay(t *testing.T) {
	r := NewRouter(testCfg()) // 静态含 linux-ci（有 addr）
	r.Table().Register(Member{NodeID: "n9", Name: "phone"})
	tgt := r.pickTarget("call @phone")
	if tgt == nil || tgt.member == nil || tgt.member.Name != "phone" {
		t.Fatalf("@phone 应命中中继成员：%+v", tgt)
	}
	if tgt := r.pickTarget("call @linux-ci"); tgt == nil || tgt.static == nil {
		t.Fatalf("@linux-ci 应命中静态直连：%+v", tgt)
	}
	if r.ShouldRoute("ping @phone") != true {
		t.Fatal("ShouldRoute 应覆盖中继成员")
	}
	// 无 addr 静态条目 + 有成员 → 中继
	r2 := NewRouter(&config.Cluster{Enabled: true, Nodes: []config.ClusterNode{{Name: "ghost", Token: "t"}}})
	if tgt := r2.pickTarget("@ghost"); tgt == nil || tgt.member == nil {
		t.Fatalf("无 addr 静态条目应回退成员中继：%+v", tgt)
	}
}

// TestNotifySettleMatch 事件匹配：task_id 命中中继句柄投递、response_id
// 命中直连句柄投递、未知事件安全忽略。
func TestNotifySettleMatch(t *testing.T) {
	r := NewRouter(testCfg())
	h := newRemoteAgent("node1", NewNodeClient("http://127.0.0.1:1", "t"), "", "", "l", nil, nil, nil)
	h.setRespID("resp_9")
	r.register(h)
	r.NotifySettle(InboxEvent{Type: "turn.settled", Node: "node1", ResponseID: "resp_9", Status: "completed", FinalText: "ok"})
	select {
	case ev := <-h.settle:
		if ev.Status != "completed" || ev.FinalText != "ok" {
			t.Fatalf("直连唤醒事件 = %+v", ev)
		}
	default:
		t.Fatal("response_id 匹配应唤醒直连句柄")
	}
	// 未知 response_id：安静忽略（不 panic 不投递）
	r.NotifySettle(InboxEvent{Type: "turn.settled", ResponseID: "resp_none", Status: "completed"})
	select {
	case <-h.settle:
		t.Fatal("未知事件不应投递")
	default:
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
