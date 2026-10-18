// hostctx_model_test.go — 裸模型名解析(resolveBareModel)与回合终态
// webhook 签名投递的语义锁定。
package server

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"git.gorun.fun/share/grf-harness/config"
)

func testCfg() *config.Settings {
	return &config.Settings{
		Default: "openai/gpt-x",
		Providers: map[string]*config.Provider{
			"openai": {Models: []string{"gpt-x", "gpt-y"}},
			"other":  {Models: []string{"gpt-y", "other-m"}},
		},
	}
}

func TestResolveModel(t *testing.T) {
	cfg := testCfg()
	// 默认供应商含该模型 → 默认供应商。
	p, m, ok := cfg.ResolveModel("gpt-x")
	if !ok || p != "openai" || m != "gpt-x" {
		t.Fatalf("默认供应商命中失败: %s/%s/%v", p, m, ok)
	}
	// gpt-y 两家都有,但默认供应商含 → 仍默认供应商。
	p, m, ok = cfg.ResolveModel("gpt-y")
	if !ok || p != "openai" || m != "gpt-y" {
		t.Fatalf("默认供应商优先失败: %s/%s/%v", p, m, ok)
	}
	// 仅一家提供 → 唯一命中。
	p, m, ok = cfg.ResolveModel("other-m")
	if !ok || p != "other" || m != "other-m" {
		t.Fatalf("唯一供应商命中失败: %s/%s/%v", p, m, ok)
	}
	// 限定名：模型表内命中。
	p, m, ok = cfg.ResolveModel("other/other-m")
	if !ok || p != "other" || m != "other-m" {
		t.Fatalf("限定名命中失败: %s/%s/%v", p, m, ok)
	}
	// 限定名：供应商未知/模型不在表内 → 放弃。
	if _, _, ok = cfg.ResolveModel("ghost/gpt-x"); ok {
		t.Fatal("未知供应商应放弃")
	}
	if _, _, ok = cfg.ResolveModel("openai/nope-m"); ok {
		t.Fatal("模型不在表内应放弃")
	}
	// 默认供应商不含 gpt-y、且 openai/other 两家都提供 → 歧义放弃
	// (不静默换到非预期供应商)。
	cfg.Providers["fourth"] = &config.Provider{Models: []string{"fourth-m"}}
	cfg.Default = "fourth/fourth-m"
	if p, m, ok = cfg.ResolveModel("gpt-y"); ok || p != "" || m != "" {
		t.Fatalf("歧义应放弃: %s/%s/%v", p, m, ok)
	}
	// 零命中。
	if p, m, ok = cfg.ResolveModel("nope"); ok || p != "" || m != "" {
		t.Fatalf("零命中应放弃: %s/%s/%v", p, m, ok)
	}
	// 空名。
	if p, m, ok = cfg.ResolveModel(""); ok || p != "" || m != "" {
		t.Fatalf("空名应放弃: %s/%s/%v", p, m, ok)
	}
}

func TestTurnWebhookSignature(t *testing.T) {
	var raw []byte
	hdr := make(chan http.Header, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		raw = b
		hdr <- r.Header.Clone()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	w := turnWebhook{url: srv.URL, secret: "s3cret"}
	w.postTurn(&ResponseResource{ID: "resp_ab12_1", SessionID: "ab12", Status: statusCompleted, Model: "m"})
	select {
	case h := <-hdr:
		var ev struct {
			Type       string `json:"type"`
			ResponseID string `json:"response_id"`
			Status     string `json:"status"`
		}
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatalf("事件体非法 JSON: %v", err)
		}
		if ev.Type != "turn.settled" || ev.ResponseID != "resp_ab12_1" || ev.Status != "completed" {
			t.Fatalf("事件体不符: %+v", ev)
		}
		sig := h.Get("X-GRF-Signature")
		mac := hmac.New(sha256.New, []byte("s3cret"))
		_, _ = mac.Write(bytes.TrimSpace(raw))
		want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		if sig != want {
			t.Fatalf("签名不符:\n got %s\nwant %s", sig, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("webhook 未投递")
	}
}
