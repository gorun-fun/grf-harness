// agentapi.go — gh server 的智能体配置 API(/v1/agent/*):让网页/嵌入形态
// 的设置页也能管理 gh 的本地配置(模型供应商、Harness 常用项、插件/MCP、
// 技能),与 goder 桌面端服务(internal/desktop 的 ModelConfig/AgentConfig)
// 同一套文件语义(GRF_HOME 可重定位;settings.json 其余键原样保留)。
//
// 密钥安全:API Key / MCP Token 绝不下发浏览器 —— GET 恒回空串并以
// hasKey/hasToken 标注是否已设置;PUT 空值 = 保留现值(按名称,改名经
// keepKeyFrom 指回改名前的条目),非空 = 更换。用户因此可换自己的 Key,
// 但拿不到(如平台播种的)已有 Key 明文。
//
// 本文件只放 DTO 定义与跨域共享的小工具;handlers 按域拆在
// agentapi_settings/models/plugins/skills.go。
package server

import (
	"sort"
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

// ---------- 跨域共享小工具 ----------

// truncateRunes 按 rune 截断(多字节安全),超长加省略号。create.go 的
// data URL 报错截断也用它。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func sortedKeys[F any](m map[string]F) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
