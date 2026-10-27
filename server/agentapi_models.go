// agentapi_models.go — /v1/agent/models 域:模型供应商与 Harness 常用项
// 的读写、供应商模型列表探测(OpenAI 兼容 GET /models)。
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/llm"
	"git.gorun.fun/share/grf-harness/term"
)

// handleAgentModelsGet GET /v1/agent/models。
func (s *Server) handleAgentModelsGet(w http.ResponseWriter, r *http.Request) {
	path := config.Path()
	out := modelCfgDTO{Path: path, Providers: []providerDTO{}, Writable: settingsWritable(path)}
	all, err := readSettingsRaw()
	if err != nil {
		writeError(w, 500, errServer, err.Error(), "")
		return
	}
	if v, ok := all["default"]; ok {
		var d string
		if json.Unmarshal(v, &d) == nil {
			out.Default = d
		}
	}
	if v, ok := all["max_steps"]; ok {
		var n int
		if json.Unmarshal(v, &n) == nil {
			out.MaxSteps = &n
		}
	}
	if v, ok := all["mouse"]; ok {
		var b bool
		if json.Unmarshal(v, &b) == nil {
			out.Mouse = &b
		}
	}
	if v, ok := all["policy"]; ok {
		var p string
		if json.Unmarshal(v, &p) == nil {
			out.Policy = p
		}
	}
	ps := map[string]config.Provider{}
	if v, ok := all["providers"]; ok {
		_ = json.Unmarshal(v, &ps)
	}
	for _, n := range sortedKeys(ps) {
		p := ps[n]
		if p.Models == nil {
			p.Models = []string{} // 空列表必须 [],序列化 null 会让前端 for..of 崩
		}
		out.Providers = append(out.Providers, providerDTO{
			Name: n, Type: p.Type, BaseURL: p.BaseURL, Models: p.Models,
			HasKey: strings.TrimSpace(p.APIKey) != "",
		})
	}
	writeJSON(w, 200, out)
}

// handleAgentModelsPut PUT /v1/agent/models:providers/default 全量替换,
// 常用项显式设置了才写(nil/空 = 沿用文件现值),其余键原样保留。
// 密钥:apiKey 空 = 保留(keepKeyFrom 指改名前的源),非空 = 更换。
func (s *Server) handleAgentModelsPut(w http.ResponseWriter, r *http.Request) {
	var in modelCfgDTO
	if !decodeJSON(w, r, &in, bodyLimitSmall) {
		return
	}
	all, err := readSettingsRaw()
	if err != nil {
		writeError(w, 500, errServer, err.Error(), "")
		return
	}
	cur := map[string]config.Provider{}
	if v, ok := all["providers"]; ok {
		_ = json.Unmarshal(v, &cur)
	}
	keepKey := func(from, fallback string) string {
		if p, ok := cur[from]; ok {
			return strings.TrimSpace(p.APIKey)
		}
		if p, ok := cur[fallback]; ok {
			return strings.TrimSpace(p.APIKey)
		}
		return ""
	}
	ps := map[string]config.Provider{}
	for _, p := range in.Providers {
		name := strings.TrimSpace(p.Name)
		if name == "" {
			continue
		}
		models := make([]string, 0, len(p.Models))
		for _, m := range p.Models {
			if v := strings.TrimSpace(m); v != "" {
				models = append(models, v)
			}
		}
		key := strings.TrimSpace(p.APIKey)
		if key == "" {
			key = keepKey(strings.TrimSpace(p.KeepKeyFrom), name)
		}
		ps[name] = config.Provider{
			Type:    strings.TrimSpace(p.Type),
			BaseURL: strings.TrimSpace(p.BaseURL),
			APIKey:  key,
			Models:  models,
		}
	}
	if raw, err := json.Marshal(ps); err == nil {
		all["providers"] = raw
	}
	if raw, err := json.Marshal(strings.TrimSpace(in.Default)); err == nil {
		all["default"] = raw
	}
	if in.MaxSteps != nil {
		if raw, err := json.Marshal(*in.MaxSteps); err == nil {
			all["max_steps"] = raw
		}
	}
	if in.Mouse != nil {
		if raw, err := json.Marshal(*in.Mouse); err == nil {
			all["mouse"] = raw
		}
	}
	if p := strings.TrimSpace(in.Policy); p != "" {
		if raw, err := json.Marshal(p); err == nil {
			all["policy"] = raw
		}
	}
	if err := writeSettingsRaw(all); err != nil {
		writeError(w, 500, errServer, err.Error(), "")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleAgentModelsDetect POST /v1/agent/models/detect:服务端探测供应商
// 的模型列表(OpenAI 兼容 GET /models，探测本体见 llm.Client.ModelsRich)。
// 探测用 Key 直接随请求携带(用户正在表单里输入的新 Key,尚不属于任何
// 已存条目)。
func (s *Server) handleAgentModelsDetect(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name    string `json:"name"`
		BaseURL string `json:"baseUrl"`
		APIKey  string `json:"apiKey"`
	}
	if !decodeJSON(w, r, &in, bodyLimitSmall) {
		return
	}
	key := strings.TrimSpace(in.APIKey)
	if key == "" {
		// 未随请求带 Key:沿用已存同名供应商的 Key(编辑既有条目场景)
		all, _ := readSettingsRaw()
		ps := map[string]config.Provider{}
		if v, ok := all["providers"]; ok {
			_ = json.Unmarshal(v, &ps)
		}
		if p, ok := ps[strings.TrimSpace(in.Name)]; ok {
			key = strings.TrimSpace(p.APIKey)
		}
	}
	base := strings.TrimRight(strings.TrimSpace(in.BaseURL), "/")
	if base == "" {
		writeError(w, 400, errInvalid, "未配置 base_url", "baseUrl")
		return
	}
	// 20 秒上界沿旧探测口径（探测是交互表单的即时反馈，不等长思考）。
	dctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	client := llm.NewFromProvider(strings.TrimSpace(in.Name), config.Provider{BaseURL: base, APIKey: key}, "")
	rich, err := client.ModelsRich(dctx)
	if err != nil {
		writeError(w, 502, errServer, "探测失败: "+term.ClipRunes(err.Error(), 200), "")
		return
	}
	// models 维持纯 id 数组(旧客户端兼容);modelsRich 带 ctxLen 供
	// 宿主网页形态缓存真实上下文窗口(与桌面端探测同构)。
	models := make([]string, 0, len(rich))
	for _, m := range rich {
		models = append(models, m.ID)
	}
	writeJSON(w, 200, map[string]any{"models": models, "modelsRich": rich})
}

// resolveDTO GET /v1/agent/models/resolve 应答。status ∈ ok|no_default|
// not_found|ambiguous|unreadable，与 goder-serve 宿主管理面 resolve-model
// 的应答形状逐字段一致（goder 转发本端点，openos 无头任务据此合成可读
// 错误文案）。resolved 仅 ok 时携带（恒为 provider/model 全限定名，或
// name 为空时默认串原样）。
type resolveDTO struct {
	Status     string   `json:"status"`
	Resolved   string   `json:"resolved,omitempty"`
	Candidates []string `json:"candidates,omitempty"`
}

// handleAgentModelsResolve GET /v1/agent/models/resolve?name= — 模型名
// 结构化解析（裸名/限定名/空=取默认），fail-fast 语义：歧义/未含如实
// 上报，绝不静默换路由。选解逻辑唯一实现在 config.ResolveModelEx。
// 每请求重读 settings.json——宿主（如 openos AI 联动）刚落盘的供应商
// 声明立即可见，无进程内陈旧性；不用 LoadFileForUpdate（其 policy 归一
// 化回写会动 mtime，干扰宿主管理面的 (mtime,size) 指纹幂等）。文件不
// 存在按"无默认/未含"处理，只有读失败或损坏才报 unreadable。
func (s *Server) handleAgentModelsResolve(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	cfg := &config.Settings{}
	if raw, err := os.ReadFile(config.Path()); err == nil {
		if err := json.Unmarshal(raw, cfg); err != nil {
			writeJSON(w, 200, resolveDTO{Status: "unreadable"})
			return
		}
	} else if !os.IsNotExist(err) {
		writeJSON(w, 200, resolveDTO{Status: "unreadable"})
		return
	}
	if name == "" {
		if cfg.Default == "" {
			writeJSON(w, 200, resolveDTO{Status: "no_default"})
			return
		}
		writeJSON(w, 200, resolveDTO{Status: "ok", Resolved: cfg.Default})
		return
	}
	provider, model, ok, cands := cfg.ResolveModelEx(name)
	switch {
	case ok:
		writeJSON(w, 200, resolveDTO{Status: "ok", Resolved: provider + "/" + model})
	case len(cands) > 1:
		writeJSON(w, 200, resolveDTO{Status: "ambiguous", Candidates: cands})
	default:
		writeJSON(w, 200, resolveDTO{Status: "not_found"})
	}
}
