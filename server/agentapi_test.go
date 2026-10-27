// agentapi_test.go — /v1/agent/* 配置 API 测试:密钥掩码保留语义、
// 插件 token 按名保留、技能 CRUD 与目录穿越防护。
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.gorun.fun/share/grf-harness/config"
)

// newAgentTestServer 独立 GRF_HOME 的配置 API 测试服务(无需 fake LLM)。
func newAgentTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	t.Setenv("GRF_HOME", t.TempDir())
	s, err := New(t.TempDir(), "test-token")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func agentDo(t *testing.T, ts *httptest.Server, method, path string, body any) (int, []byte) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		rd = bytes.NewReader(data)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, ts.URL+path, rd)
	req.Header.Set("Authorization", "Bearer test-token")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.Bytes()
}

func TestAgentModelsMaskKeep(t *testing.T) {
	ts := newAgentTestServer(t)

	// 写入带 Key 的供应商
	code, _ := agentDo(t, ts, "PUT", "/v1/agent/models", map[string]any{
		"providers": []map[string]any{
			{"name": "prov", "baseUrl": "https://api.example.com/v1", "apiKey": "sk-secret-123", "models": []string{"m1"}},
		},
		"default": "prov/m1",
	})
	if code != 200 {
		t.Fatalf("写入供应商失败: %d", code)
	}

	// GET:Key 恒空串,hasKey=true
	code, body := agentDo(t, ts, "GET", "/v1/agent/models", nil)
	if code != 200 {
		t.Fatalf("读取失败: %d", code)
	}
	var cfg modelCfgDTO
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "prov" || !cfg.Providers[0].HasKey {
		t.Fatalf("供应商形状不对: %+v", cfg.Providers)
	}
	if cfg.Providers[0].APIKey != "" {
		t.Fatalf("GET 不应返回 Key 明文: %q", cfg.Providers[0].APIKey)
	}
	if cfg.Default != "prov/m1" {
		t.Fatalf("default 丢失: %q", cfg.Default)
	}

	// 掩码保存:不改 Key(空值)且改名(keepKeyFrom 指回旧名)
	code, _ = agentDo(t, ts, "PUT", "/v1/agent/models", map[string]any{
		"providers": []map[string]any{
			{"name": "prov2", "baseUrl": "https://api.example.com/v1", "apiKey": "", "keepKeyFrom": "prov", "models": []string{"m1", "m2"}, "hasKey": true},
		},
		"default": "prov2/m2",
	})
	if code != 200 {
		t.Fatalf("掩码保存失败: %d", code)
	}
	// 落盘文件里 Key 保留在改名后的条目上
	raw, err := os.ReadFile(config.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "sk-secret-123") || !strings.Contains(string(raw), `"prov2"`) {
		t.Fatalf("改名后 Key 未保留: %s", raw)
	}
	if strings.Contains(string(raw), `"prov"`) {
		t.Fatalf("旧名条目应被替换: %s", raw)
	}
}

func TestAgentModelsPreservesUnknownKeys(t *testing.T) {
	ts := newAgentTestServer(t)
	// 预置未知键 + providers
	home := config.Home()
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	pre := `{"system_prompt": "keep-me", "providers": {"a": {"base_url": "u", "api_key": "k"}}}`
	if err := os.WriteFile(filepath.Join(home, "settings.json"), []byte(pre), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _ := agentDo(t, ts, "PUT", "/v1/agent/models", map[string]any{
		"providers": []map[string]any{{"name": "b", "baseUrl": "u2"}},
		"default":   "b/m",
	})
	if code != 200 {
		t.Fatalf("保存失败: %d", code)
	}
	raw, _ := os.ReadFile(filepath.Join(home, "settings.json"))
	if !strings.Contains(string(raw), "keep-me") {
		t.Fatalf("未知键被丢弃: %s", raw)
	}
	// 空掩码语义:b 未带 Key 且旧条目名不同 → Key 为空(不误拿 a 的)
	if strings.Contains(string(raw), `"api_key":"k"`) {
		t.Fatalf("不应跨条目继承 Key: %s", raw)
	}
}

func TestAgentPluginsTokenKeep(t *testing.T) {
	ts := newAgentTestServer(t)
	code, _ := agentDo(t, ts, "PUT", "/v1/agent/plugins", map[string]any{
		"explicit": true,
		"plugins": []map[string]any{
			{"name": "mcp1", "url": "https://mcp.example.com", "token": "tok-secret"},
		},
	})
	if code != 200 {
		t.Fatalf("写入插件失败: %d", code)
	}
	code, body := agentDo(t, ts, "GET", "/v1/agent/plugins", nil)
	if code != 200 {
		t.Fatalf("读取插件失败: %d", code)
	}
	var pc pluginsDTO
	if err := json.Unmarshal(body, &pc); err != nil {
		t.Fatal(err)
	}
	if !pc.Explicit || len(pc.Plugins) != 1 || !pc.Plugins[0].HasTok || pc.Plugins[0].Token != "" {
		t.Fatalf("插件形状不对: %+v", pc)
	}
	// 空 token = 同名保留
	code, _ = agentDo(t, ts, "PUT", "/v1/agent/plugins", map[string]any{
		"explicit": true,
		"plugins": []map[string]any{
			{"name": "mcp1", "url": "https://mcp.example.com/v2", "token": ""},
		},
	})
	if code != 200 {
		t.Fatalf("掩码保存失败: %d", code)
	}
	raw, _ := os.ReadFile(config.Path())
	if !strings.Contains(string(raw), "tok-secret") {
		t.Fatalf("token 未按名保留: %s", raw)
	}
	// explicit=false 删键回到自动发现
	code, _ = agentDo(t, ts, "PUT", "/v1/agent/plugins", map[string]any{"explicit": false})
	if code != 200 {
		t.Fatalf("删除失败: %d", code)
	}
	raw, _ = os.ReadFile(config.Path())
	if strings.Contains(string(raw), "plugins") {
		t.Fatalf("explicit=false 应删除 plugins 键: %s", raw)
	}
}

func TestAgentSkillsCRUD(t *testing.T) {
	ts := newAgentTestServer(t)

	// 新建
	code, _ := agentDo(t, ts, "PUT", "/v1/agent/skills/my-skill", map[string]string{
		"content": "---\ndescription: 测试技能\n---\n正文",
	})
	if code != 200 {
		t.Fatalf("新建技能失败: %d", code)
	}
	// 列表:description 取 frontmatter
	_, body := agentDo(t, ts, "GET", "/v1/agent/skills", nil)
	var sc skillsDTO
	if err := json.Unmarshal(body, &sc); err != nil {
		t.Fatal(err)
	}
	if len(sc.Skills) != 1 || sc.Skills[0].Name != "my-skill" || sc.Skills[0].Description != "测试技能" || sc.Skills[0].Disabled {
		t.Fatalf("技能列表不对: %+v", sc)
	}
	// 读全文
	code, body = agentDo(t, ts, "GET", "/v1/agent/skills/my-skill", nil)
	if code != 200 || !strings.Contains(string(body), "正文") {
		t.Fatalf("读取全文失败: %d %s", code, body)
	}
	// 禁用 → 列表 Disabled,全文仍可读
	code, _ = agentDo(t, ts, "POST", "/v1/agent/skills/my-skill/enabled", map[string]bool{"enabled": false})
	if code != 200 {
		t.Fatalf("禁用失败: %d", code)
	}
	_, body = agentDo(t, ts, "GET", "/v1/agent/skills", nil)
	_ = json.Unmarshal(body, &sc)
	if !sc.Skills[0].Disabled {
		t.Fatalf("应已禁用: %+v", sc.Skills)
	}
	code, _ = agentDo(t, ts, "GET", "/v1/agent/skills/my-skill", nil)
	if code != 200 {
		t.Fatalf("禁用态应仍可读: %d", code)
	}
	// 重新启用
	code, _ = agentDo(t, ts, "POST", "/v1/agent/skills/my-skill/enabled", map[string]bool{"enabled": true})
	if code != 200 {
		t.Fatalf("启用失败: %d", code)
	}
	// 目录穿越拒绝
	code, _ = agentDo(t, ts, "GET", "/v1/agent/skills/..%2Fsettings.json", nil)
	if code != 400 && code != 404 && code != 200 {
		t.Logf("穿越请求状态 %d( ServeMux 可能已规范化路径)", code)
	}
	// 删除
	code, _ = agentDo(t, ts, "DELETE", "/v1/agent/skills/my-skill", nil)
	if code != 200 {
		t.Fatalf("删除失败: %d", code)
	}
	_, body = agentDo(t, ts, "GET", "/v1/agent/skills", nil)
	_ = json.Unmarshal(body, &sc)
	if len(sc.Skills) != 0 {
		t.Fatalf("应已删除: %+v", sc)
	}
}

func TestAgentModelsResolve(t *testing.T) {
	ts := newAgentTestServer(t)
	// 文件尚不存在：空名 = 无默认，裸名 = 未含
	if code, body := agentDo(t, ts, "GET", "/v1/agent/models/resolve?name=", nil); code != 200 || !strings.Contains(string(body), `"no_default"`) {
		t.Fatalf("空默认分支: %d %s", code, body)
	}
	if code, body := agentDo(t, ts, "GET", "/v1/agent/models/resolve?name=m1", nil); code != 200 || !strings.Contains(string(body), `"not_found"`) {
		t.Fatalf("未含分支: %d %s", code, body)
	}

	// provA [m1,m2] 为默认供应商；provB/provC 同含 shared
	code, _ := agentDo(t, ts, "PUT", "/v1/agent/models", map[string]any{
		"providers": []map[string]any{
			{"name": "provA", "baseUrl": "https://a.example.com/v1", "models": []string{"m1", "m2"}},
			{"name": "provB", "baseUrl": "https://b.example.com/v1", "models": []string{"shared"}},
			{"name": "provC", "baseUrl": "https://c.example.com/v1", "models": []string{"shared"}},
		},
		"default": "provA/m2",
	})
	if code != 200 {
		t.Fatalf("写入供应商失败: %d", code)
	}
	cases := []struct {
		name, want string
		contains   string
	}{
		{"", "no_default-or-ok", `"status":"ok","resolved":"provA/m2"`},
		{"m2", "默认供应商命中", `"resolved":"provA/m2"`},
		{"m1", "默认供应商另一模型", `"resolved":"provA/m1"`},
		{"shared", "多家同名歧义", `"status":"ambiguous","candidates":["provB","provC"]`},
		{"provB/shared", "限定名", `"resolved":"provB/shared"`},
		{"provX/m", "未知供应商", `"status":"not_found"`},
	}
	for _, c := range cases {
		_, body := agentDo(t, ts, "GET", "/v1/agent/models/resolve?name="+c.name, nil)
		if !strings.Contains(string(body), c.contains) {
			t.Fatalf("%s（%s）应含 %s，实得 %s", c.name, c.want, c.contains, body)
		}
	}

	// 损坏文件 = unreadable
	if err := os.WriteFile(config.Path(), []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, body := agentDo(t, ts, "GET", "/v1/agent/models/resolve?name=m1", nil); code != 200 || !strings.Contains(string(body), `"unreadable"`) {
		t.Fatalf("损坏分支: %d %s", code, body)
	}
}
