// agentapi.go — gh server 的智能体配置 API(/v1/agent/*):让网页/嵌入形态
// 的设置页也能管理 gh 的本地配置(模型供应商、Harness 常用项、插件/MCP、
// 技能),与 goder 桌面端服务(internal/desktop 的 ModelConfig/AgentConfig)
// 同一套文件语义(GRF_HOME 可重定位;settings.json 其余键原样保留)。
//
// 密钥安全:API Key / MCP Token 绝不下发浏览器 —— GET 恒回空串并以
// hasKey/hasToken 标注是否已设置;PUT 空值 = 保留现值(按名称,改名经
// keepKeyFrom 指回改名前的条目),非空 = 更换。用户因此可换自己的 Key,
// 但拿不到(如平台播种的)已有 Key 明文。
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ---------- DTO(与 goder 前端 host/bridge.ts 对齐,camelCase) ----------

// providerDTO 供应商项。apiKey:GET 恒空(hasKey 标注);PUT 空=保留。
type providerDTO struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	BaseURL string   `json:"baseUrl"`
	APIKey  string   `json:"apiKey"`
	Models  []string `json:"models"`
	// GET:是否已设置 Key;PUT:客户端不填(服务端回填)。
	HasKey bool `json:"hasKey"`
	// PUT 专用:改名时指向改名前的供应商名,空 Key 从它继承。
	KeepKeyFrom string `json:"keepKeyFrom,omitempty"`
}

// modelCfgDTO 模型与 Harness 常用项(maxSteps/mouse/policy)。
type modelCfgDTO struct {
	Path      string        `json:"path"`
	Providers []providerDTO `json:"providers"`
	Default   string        `json:"default"`
	Writable  bool          `json:"writable"`
	MaxSteps  *int          `json:"maxSteps"`
	Mouse     *bool         `json:"mouse"`
	Policy    string        `json:"policy"`
}

// pluginDTO settings.json plugins 项。token 掩码语义同 providerDTO.apiKey。
type pluginDTO struct {
	Name    string   `json:"name"`
	Command []string `json:"command"`
	URL     string   `json:"url"`
	Token   string   `json:"token"`
	Timeout int      `json:"timeout"`
	HasTok  bool     `json:"hasToken"`
}

// pluginsDTO 插件/MCP 配置。
type pluginsDTO struct {
	Path       string      `json:"path"`
	Dir        string      `json:"dir"`
	Explicit   bool        `json:"explicit"`
	Plugins    []pluginDTO `json:"plugins"`
	Discovered []string    `json:"discovered"`
	Writable   bool        `json:"writable"`
}

// skillDTO 技能项(SKILL.md / SKILL.md.disabled)。
type skillDTO struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Path        string `json:"path"`
	Disabled    bool   `json:"disabled"`
}

// skillsDTO 技能列表。
type skillsDTO struct {
	Path     string     `json:"path"`
	Skills   []skillDTO `json:"skills"`
	Writable bool       `json:"writable"`
}

// ---------- 文件形状(settings.json 原生 snake_case) ----------

// ghProvider settings.json 中单个供应商的原始形状。
type ghProvider struct {
	Type    string   `json:"type,omitempty"`
	BaseURL string   `json:"base_url"`
	APIKey  string   `json:"api_key,omitempty"`
	Models  []string `json:"models,omitempty"`
}

// ghPlugin settings.json plugins 项的原始形状。
type ghPlugin struct {
	Name    string   `json:"name,omitempty"`
	Command []string `json:"command,omitempty"`
	URL     string   `json:"url,omitempty"`
	Token   string   `json:"token,omitempty"`
	Timeout int      `json:"timeout,omitempty"`
}

// readSettingsRaw 读 settings.json 为通用 map(损坏按空文档处理);不存在
// 返回空 map。其余未知键靠它原样保留。
func readSettingsRaw() (map[string]json.RawMessage, error) {
	path, err := settingsPath()
	if err != nil {
		return nil, err
	}
	all := map[string]json.RawMessage{}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return all, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(raw, &all); err != nil {
		return map[string]json.RawMessage{}, nil // 损坏按空文档,由本次写入修复
	}
	return all, nil
}

// writeSettingsRaw 写回 settings.json(保留未知键;0600)。
func writeSettingsRaw(all map[string]json.RawMessage) error {
	path, err := settingsPath()
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func settingsPath() (string, error) {
	return filepath.Join(configHome(), "settings.json"), nil
}

func configHome() string {
	if h := strings.TrimSpace(os.Getenv("GRF_HOME")); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".grf"
	}
	return filepath.Join(home, ".grf")
}

// writable 目标可写探测(文件或目录;不存在时建临时探针)。
func settingsWritable(path string) bool {
	if f, err := os.OpenFile(path, os.O_WRONLY, 0o600); err == nil {
		f.Close()
		return true
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	f, err := os.CreateTemp(dir, ".grf-write-test")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	_ = os.Remove(name)
	return true
}

// ---------- 模型 / Harness ----------

// handleAgentModels GET /v1/agent/models。
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
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, 400, errInvalid, "read body: "+err.Error(), "")
		return
	}
	var in modelCfgDTO
	if err := json.Unmarshal(body, &in); err != nil {
		writeError(w, 400, errInvalid, "invalid JSON body: "+err.Error(), "")
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
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, 400, errInvalid, "read body: "+err.Error(), "")
		return
	}
	var in struct {
		Name    string `json:"name"`
		BaseURL string `json:"baseUrl"`
		APIKey  string `json:"apiKey"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		writeError(w, 400, errInvalid, "invalid JSON body: "+err.Error(), "")
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
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rb, &parsed); err != nil {
		writeError(w, 502, errServer, "响应不是 OpenAI 兼容模型列表: "+err.Error(), "")
		return
	}
	models := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	writeJSON(w, 200, map[string]any{"models": models})
}

// ---------- 插件 / MCP ----------

// handleAgentPluginsGet GET /v1/agent/plugins。
func (s *Server) handleAgentPluginsGet(w http.ResponseWriter, r *http.Request) {
	path, _ := settingsPath()
	dir := filepath.Join(configHome(), "plugins")
	out := pluginsDTO{
		Path: path, Dir: dir,
		Plugins: []pluginDTO{}, Discovered: []string{},
		Writable: settingsWritable(path),
	}
	all, err := readSettingsRaw()
	if err != nil {
		writeError(w, 500, errServer, err.Error(), "")
		return
	}
	if v, ok := all["plugins"]; ok {
		out.Explicit = true
		list := []ghPlugin{}
		_ = json.Unmarshal(v, &list)
		for _, p := range list {
			if p.Command == nil {
				p.Command = []string{}
			}
			out.Plugins = append(out.Plugins, pluginDTO{
				Name: p.Name, Command: p.Command, URL: p.URL, Timeout: p.Timeout,
				HasTok: strings.TrimSpace(p.Token) != "",
			})
		}
	}
	if ents, err := os.ReadDir(dir); err == nil {
		for _, e := range ents {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") || strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			if info, err := e.Info(); err == nil && info.Mode()&0o111 != 0 {
				out.Discovered = append(out.Discovered, e.Name())
			}
		}
		sort.Strings(out.Discovered)
	}
	writeJSON(w, 200, out)
}

// handleAgentPluginsPut PUT /v1/agent/plugins:explicit=false 删 plugins 键
// (回到自动发现);true 写显式列表。token 空 = 按同名保留现值。
func (s *Server) handleAgentPluginsPut(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, 400, errInvalid, "read body: "+err.Error(), "")
		return
	}
	var in pluginsDTO
	if err := json.Unmarshal(body, &in); err != nil {
		writeError(w, 400, errInvalid, "invalid JSON body: "+err.Error(), "")
		return
	}
	all, err := readSettingsRaw()
	if err != nil {
		writeError(w, 500, errServer, err.Error(), "")
		return
	}
	if !in.Explicit {
		delete(all, "plugins")
	} else {
		cur := []ghPlugin{}
		if v, ok := all["plugins"]; ok {
			_ = json.Unmarshal(v, &cur)
		}
		curTok := func(name string) string {
			for _, p := range cur {
				if strings.TrimSpace(p.Name) == strings.TrimSpace(name) {
					return strings.TrimSpace(p.Token)
				}
			}
			return ""
		}
		list := make([]ghPlugin, 0, len(in.Plugins))
		for _, p := range in.Plugins {
			if strings.TrimSpace(p.Name) == "" && strings.TrimSpace(p.URL) == "" && len(p.Command) == 0 {
				continue
			}
			cmd := make([]string, 0, len(p.Command))
			for _, c := range p.Command {
				if v := strings.TrimSpace(c); v != "" {
					cmd = append(cmd, v)
				}
			}
			tok := strings.TrimSpace(p.Token)
			if tok == "" {
				tok = curTok(p.Name)
			}
			list = append(list, ghPlugin{
				Name: strings.TrimSpace(p.Name), Command: cmd,
				URL: strings.TrimSpace(p.URL), Token: tok, Timeout: p.Timeout,
			})
		}
		raw, err := json.Marshal(list)
		if err != nil {
			writeError(w, 500, errServer, err.Error(), "")
			return
		}
		all["plugins"] = raw
	}
	if err := writeSettingsRaw(all); err != nil {
		writeError(w, 500, errServer, err.Error(), "")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- 技能(GRF_HOME/skills/<name>/SKILL.md) ----------

func skillsDir() string {
	return filepath.Join(configHome(), "skills")
}

// handleAgentSkillsGet GET /v1/agent/skills。
func (s *Server) handleAgentSkillsGet(w http.ResponseWriter, r *http.Request) {
	dir := skillsDir()
	out := skillsDTO{Path: dir, Skills: []skillDTO{}, Writable: settingsWritable(dir)}
	ents, err := os.ReadDir(dir)
	if err != nil {
		writeJSON(w, 200, out) // 目录不存在 = 空列表
		return
	}
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		md := filepath.Join(dir, e.Name(), "SKILL.md")
		disabled := false
		if _, err := os.Stat(md); err != nil {
			if _, err2 := os.Stat(filepath.Join(dir, e.Name(), "SKILL.md.disabled")); err2 != nil {
				continue
			}
			md = filepath.Join(dir, e.Name(), "SKILL.md.disabled")
			disabled = true
		}
		raw, err := os.ReadFile(md)
		if err != nil {
			continue
		}
		out.Skills = append(out.Skills, skillDTO{
			Name:        e.Name(),
			Description: skillDescription(string(raw)),
			Path:        md,
			Disabled:    disabled,
		})
	}
	sort.Slice(out.Skills, func(i, j int) bool { return out.Skills[i].Name < out.Skills[j].Name })
	writeJSON(w, 200, out)
}

// handleAgentSkillGet GET /v1/agent/skills/{name}:技能全文。
func (s *Server) handleAgentSkillGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := validSkillName(name); err != nil {
		writeError(w, 400, errInvalid, err.Error(), "name")
		return
	}
	raw, err := os.ReadFile(filepath.Join(skillsDir(), name, "SKILL.md"))
	if err != nil {
		if os.IsNotExist(err) {
			// 已禁用的技能也可读(编辑不必先启用)
			raw, err = os.ReadFile(filepath.Join(skillsDir(), name, "SKILL.md.disabled"))
		}
		if err != nil {
			writeError(w, 404, errNotFound, "技能不存在: "+name, "")
			return
		}
	}
	writeJSON(w, 200, map[string]string{"content": string(raw)})
}

// handleAgentSkillPut PUT /v1/agent/skills/{name}:新建/覆盖 SKILL.md。
func (s *Server) handleAgentSkillPut(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := validSkillName(name); err != nil {
		writeError(w, 400, errInvalid, err.Error(), "name")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, 400, errInvalid, "read body: "+err.Error(), "")
		return
	}
	var in struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		writeError(w, 400, errInvalid, "invalid JSON body: "+err.Error(), "")
		return
	}
	dir := filepath.Join(skillsDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeError(w, 500, errServer, err.Error(), "")
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(in.Content), 0o600); err != nil {
		writeError(w, 500, errServer, err.Error(), "")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleAgentSkillDelete DELETE /v1/agent/skills/{name}。
func (s *Server) handleAgentSkillDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := validSkillName(name); err != nil {
		writeError(w, 400, errInvalid, err.Error(), "name")
		return
	}
	dir := filepath.Join(skillsDir(), name)
	if _, err := os.Stat(dir); err != nil {
		writeError(w, 404, errNotFound, "技能不存在: "+name, "")
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		writeError(w, 500, errServer, err.Error(), "")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleAgentSkillEnabled POST /v1/agent/skills/{name}/enabled {"enabled":bool}:
// gh 只认 SKILL.md,禁用即改名 SKILL.md.disabled(内容保留),启用改回。
func (s *Server) handleAgentSkillEnabled(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := validSkillName(name); err != nil {
		writeError(w, 400, errInvalid, err.Error(), "name")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, 400, errInvalid, "read body: "+err.Error(), "")
		return
	}
	var in struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		writeError(w, 400, errInvalid, "invalid JSON body: "+err.Error(), "")
		return
	}
	dir := filepath.Join(skillsDir(), name)
	on := filepath.Join(dir, "SKILL.md")
	off := filepath.Join(dir, "SKILL.md.disabled")
	if in.Enabled {
		if _, err := os.Stat(off); err != nil {
			if _, err2 := os.Stat(on); err2 == nil {
				writeJSON(w, 200, map[string]any{"ok": true}) // 已是启用态
				return
			}
			writeError(w, 404, errNotFound, "技能不存在: "+name, "")
			return
		}
		if err := os.Rename(off, on); err != nil {
			writeError(w, 500, errServer, err.Error(), "")
			return
		}
	} else {
		if _, err := os.Stat(on); err != nil {
			if _, err2 := os.Stat(off); err2 == nil {
				writeJSON(w, 200, map[string]any{"ok": true}) // 已是禁用态
				return
			}
			writeError(w, 404, errNotFound, "技能不存在: "+name, "")
			return
		}
		if err := os.Rename(on, off); err != nil {
			writeError(w, 500, errServer, err.Error(), "")
			return
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- 小工具 ----------

// validSkillName 技能名即目录名:非空且不含路径分隔符/向上引用。
func validSkillName(name string) error {
	if name == "" || strings.ContainsRune(name, '/') || strings.ContainsRune(name, '\\') || name == "." || name == ".." {
		return fmt.Errorf("非法技能名: %s", name)
	}
	return nil
}

// skillDescription 提取技能描述:优先 YAML frontmatter 的 description:,
// 否则取正文首个非标题非空行(截断到 80 字)。
func skillDescription(md string) string {
	lines := strings.Split(md, "\n")
	inFront := false
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "---" && !inFront {
			inFront = true
			continue
		}
		if inFront {
			if t == "---" || t == "..." {
				inFront = false
			} else if v, ok := strings.CutPrefix(t, "description:"); ok {
				if s := strings.Trim(strings.TrimSpace(v), `"'`); s != "" {
					return truncateRunes(s, 80)
				}
			}
			continue
		}
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		return truncateRunes(t, 80)
	}
	return ""
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func sortedKeys[T any](m map[string]T) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
