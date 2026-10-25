package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func resolveFixture() *Settings {
	return &Settings{
		Default: "GRF/glm-5.3-flash",
		Providers: map[string]*Provider{
			"GRF":   {BaseURL: "https://grf.example/v1", APIKey: "grf-key", Models: []string{"glm-5.3-flash", "glm-5.3"}},
			"proxy": {BaseURL: "https://proxy.example/v1", APIKey: "proxy-key", Models: []string{"ds-example-chat"}},
		},
	}
}

func TestResolveModel(t *testing.T) {
	s := resolveFixture()
	cases := []struct {
		name  string // 输入
		prov  string // 期望 provider（空 = 期望 ok=false）
		model string
	}{
		{"GRF/glm-5.3-flash", "GRF", "glm-5.3-flash"},             // 限定名命中
		{"proxy/ds-example-chat", "proxy", "ds-example-chat"}, // 限定名切供应商
		{"GRF/never-registered", "", ""},                          // 限定名模型不在表内
		{"ghost/model", "", ""},                                   // 限定名供应商未知
		{"glm-5.3", "GRF", "glm-5.3"},                             // 裸名在默认供应商
		{"ds-example-chat", "proxy", "ds-example-chat"},       // 裸名恰一家提供
		{"no-such-model", "", ""},                                 // 裸名零命中
		{"", "", ""},                                              // 空名
	}
	for _, c := range cases {
		pn, m, ok := s.ResolveModel(c.name)
		if c.prov == "" {
			if ok {
				t.Errorf("ResolveModel(%q) 应失败，得 (%q,%q,true)", c.name, pn, m)
			}
			continue
		}
		if !ok || pn != c.prov || m != c.model {
			t.Errorf("ResolveModel(%q) = (%q,%q,%v), 期望 (%q,%q,true)", c.name, pn, m, ok, c.prov, c.model)
		}
	}
	// 裸名歧义：两家提供且默认供应商不含 → 放弃（默认供应商仍含时优先默认，
	// 上面 "glm-5.3" 用例已覆盖）
	s2 := resolveFixture()
	s2.Providers["GRF"].Models = []string{"glm-5.3-flash"} // 默认供应商去掉 glm-5.3
	s2.Providers["proxy"].Models = append(s2.Providers["proxy"].Models, "glm-5.3")
	s2.Providers["third"] = &Provider{Models: []string{"glm-5.3"}} // 第二家同名 → 歧义
	if pn, _, ok := s2.ResolveModel("glm-5.3"); ok || pn != "" {
		t.Errorf("裸名歧义应放弃，得 (%q,%v)", pn, ok)
	}
}

// TestLoadGRFModel GRF_MODEL 环境覆盖的三类写法与回退（限定名曾整串
// 透传导致网关收到 "GRF/xxx" 被拒，本表锁定修复语义）。
func TestLoadGRFModel(t *testing.T) {
	writeFixture := func(t *testing.T, home string) {
		t.Helper()
		data, _ := json.Marshal(resolveFixture())
		os.WriteFile(filepath.Join(home, "settings.json"), data, 0o600)
	}
	cases := []struct {
		env      string
		provider string
		model    string
		baseURL  string
	}{
		{"GRF/glm-5.3-flash", "GRF", "glm-5.3-flash", "https://grf.example/v1"},               // 限定名：剥前缀（修复点）
		{"proxy/ds-example-chat", "proxy", "ds-example-chat", "https://proxy.example/v1"}, // 切供应商
		{"GRF/brand-new", "GRF", "brand-new", "https://grf.example/v1"},                       // 已知供应商+未登记：信任覆盖
		{"glm-5.3-flash", "GRF", "glm-5.3-flash", "https://grf.example/v1"},                   // 裸名在默认供应商：等价现状
		{"ds-example-chat", "proxy", "ds-example-chat", "https://proxy.example/v1"},       // 裸名恰一家：切换
		{"ghost/model", "GRF", "ghost/model", "https://grf.example/v1"},                       // 未知供应商：整串原样（老行为）
		{"whatever", "GRF", "whatever", "https://grf.example/v1"},                             // 零命中：原样（老行为）
	}
	for _, c := range cases {
		t.Run(c.env, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("GRF_HOME", home)
			writeFixture(t, home)
			t.Setenv("GRF_MODEL", c.env)
			eff, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if eff.Provider != c.provider || eff.Model != c.model || eff.BaseURL != c.baseURL {
				t.Fatalf("GRF_MODEL=%q → provider=%q model=%q base=%q, 期望 %q/%q/%q",
					c.env, eff.Provider, eff.Model, eff.BaseURL, c.provider, c.model, c.baseURL)
			}
		})
	}
}
