// callhome_test.go CallHome 回归：注册请求必须携带**本节点**能力清单
// （caps 回调注入，不能从指挥官拉取——那会把指挥官的身份/模型当成自己的
// 上报，实测在双机集群中暴露）；心跳随后到达；caps 为 nil 也能注册。
package cluster

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeCommander 指挥官桩：记录收到的注册/心跳请求。
type fakeCommander struct {
	srv        *httptest.Server
	registered chan RegisterRequest
	hearts     chan HeartbeatRequest
}

func newFakeCommander(t *testing.T) *fakeCommander {
	t.Helper()
	fc := &fakeCommander{
		registered: make(chan RegisterRequest, 4),
		hearts:     make(chan HeartbeatRequest, 4),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/cluster/register", func(w http.ResponseWriter, r *http.Request) {
		var req RegisterRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		fc.registered <- req
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/cluster/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		var req HeartbeatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		fc.hearts <- req
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	fc.srv = httptest.NewServer(mux)
	t.Cleanup(fc.srv.Close)
	return fc
}

func TestCallHomeRegistersOwnCaps(t *testing.T) {
	fc := newFakeCommander(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	self := Identity{ID: "0123456789abcdef", Name: "mynode"}
	capsCalls := 0
	caps := func() (*Capabilities, error) {
		capsCalls++
		return &Capabilities{NodeID: self.ID, Name: self.Name, Models: []string{"p/m"}}, nil
	}
	go CallHome(ctx, fc.srv.URL, "tok", self, caps, func() int { return 2 })

	select {
	case reg := <-fc.registered:
		if reg.NodeID != self.ID || reg.Name != self.Name {
			t.Fatalf("注册身份 = %s/%s, want %s/%s", reg.NodeID, reg.Name, self.ID, self.Name)
		}
		if reg.Capabilities == nil {
			t.Fatal("注册应携带能力清单（caps 回调）")
		}
		// 回归点：上报的必须是本节点能力，而非指挥官的
		if reg.Capabilities.NodeID != self.ID || reg.Capabilities.Name != self.Name {
			t.Errorf("能力清单身份 = %s/%s, want 本节点 %s/%s",
				reg.Capabilities.NodeID, reg.Capabilities.Name, self.ID, self.Name)
		}
		if len(reg.Capabilities.Models) != 1 || reg.Capabilities.Models[0] != "p/m" {
			t.Errorf("能力清单模型 = %v, want [p/m]", reg.Capabilities.Models)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CallHome 未在 5s 内注册")
	}
	if capsCalls == 0 {
		t.Error("caps 回调未被调用")
	}
	select {
	case hb := <-fc.hearts:
		if hb.Running != 2 {
			t.Errorf("心跳负载 = %d, want 2", hb.Running)
		}
	case <-time.After(callHomeInterval + 5*time.Second):
		t.Fatal("CallHome 注册后未心跳")
	}
}

func TestCallHomeNilCaps(t *testing.T) {
	fc := newFakeCommander(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	self := Identity{ID: "0123456789abcdef", Name: "bare"}
	go CallHome(ctx, fc.srv.URL, "tok", self, nil, nil)
	select {
	case reg := <-fc.registered:
		if reg.Capabilities != nil {
			t.Errorf("caps 为 nil 时不应携带能力清单，得到 %+v", reg.Capabilities)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CallHome（无 caps）未在 5s 内注册")
	}
}
