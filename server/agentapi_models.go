// agentapi_models.go — /v1/agent/models 域:模型供应商与 Harness 常用项
// 的读写、供应商模型列表探测(OpenAI 兼容 GET /models)。
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// handleAgentModelsGet GET /v1/agent/models。
func (s *Server) handleAgentModelsGet(w http.ResponseWriter, r *http.Request) {
	path, _ := settingsPath()
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
	ps := map[string]ghProvider{}
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
	cur := map[string]ghProvider{}
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
	ps := map[string]ghProvider{}
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
		ps[name] = ghProvider{
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
// 的模型列表(OpenAI 兼容 GET /models)。探测用 Key 直接随请求携带(用户
// 正在表单里输入的新 Key,尚不属于任何已存条目)。
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
		ps := map[string]ghProvider{}
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
	req, err := http.NewRequest(http.MethodGet, base+"/models", nil)
	if err != nil {
		writeError(w, 400, errInvalid, err.Error(), "")
		return
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		writeError(w, 502, errServer, "探测失败: "+err.Error(), "")
		return
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		writeError(w, 502, errServer, fmt.Sprintf("供应商返回 %d: %s", resp.StatusCode, truncateRunes(string(rb), 200)), "")
		return
	}
	// 裸数组 [{id}] 也兼容;上下文窗口按社区常见字段名依次尝试
	// (token-hub 三者同值;OpenRouter/SiliconFlow 用 context_length,
	// vLLM 用 max_model_len),与 goder 桌面侧 modelconfig.go 同一套字段。
	var wrapped struct {
		Data []map[string]any `json:"data"`
	}
	var bare []map[string]any
	if err := json.Unmarshal(rb, &wrapped); err == nil && len(wrapped.Data) > 0 {
		bare = wrapped.Data
	} else if err := json.Unmarshal(rb, &bare); err != nil {
		writeError(w, 502, errServer, "响应不是 OpenAI 兼容模型列表: "+err.Error(), "")
		return
	}
	type richModel struct {
		ID     string `json:"id"`
		CtxLen int    `json:"ctxLen"`
	}
	models := make([]string, 0, len(bare))
	rich := make([]richModel, 0, len(bare))
	for _, item := range bare {
		id, _ := item["id"].(string)
		if id == "" {
			continue
		}
		models = append(models, id)
		rich = append(rich, richModel{ID: id, CtxLen: modelCtxLen(item)})
	}
	// models 维持纯 id 数组(旧客户端兼容);modelsRich 带 ctxLen 供
	// goder 网页形态缓存真实上下文窗口(与桌面态 wails 探测同构)。
	writeJSON(w, 200, map[string]any{"models": models, "modelsRich": rich})
}

// modelCtxLen 从模型对象里取上下文窗口,数字或数字字符串均可,取首个 >0 的值。
func modelCtxLen(item map[string]any) int {
	for _, key := range []string{"context_length", "context_window", "max_model_len"} {
		switch v := item[key].(type) {
		case float64:
			if v > 0 {
				return int(v)
			}
		case string:
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}
