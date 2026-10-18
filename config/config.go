// Package config 管理 ~/.grf/settings.json 配置（GRF_HOME 可重定位），
// 支持多模型提供商；取值优先级：环境变量 > 配置文件 > 内置默认值。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Provider 一个模型提供商。
type Provider struct {
	Type    string   `json:"type,omitempty"`
	BaseURL string   `json:"base_url"`
	APIKey  string   `json:"api_key,omitempty"`
	Models  []string `json:"models,omitempty"`
}

// Settings 配置文件结构。Default 形如 "provider/model"（按第一个 / 拆分）。
// Policy: 统一规则串（竖线组合，sandbox 与文件策略共用一套 token）；
//
//	write-workspace  仅写工作区：bash OS 级沙箱（macOS）+ 写前先读
//	                 （文件工具的写边界本就无条件生效，不受此控制）
//	read-workspace   仅读工作区：read/glob/grep/read_image 的读边界
//	off              不禁止（写边界除外，架构性保留）
//
// 空 = 默认 "off"（全关）；解析不正确回退 write-workspace（保写侧）。
// 解析见 ParsePolicy / NormalizePolicy。
type Settings struct {
	Providers    map[string]*Provider `json:"providers"`
	Default      string               `json:"default,omitempty"`
	Mouse        *bool                `json:"mouse,omitempty"`
	SystemPrompt string               `json:"system_prompt,omitempty"`
	MaxSteps     int                  `json:"max_steps,omitempty"`
	Policy       string               `json:"policy,omitempty"`
	AutoApprove  bool                 `json:"auto_approve,omitempty"` // 非交互时自动批准沙箱升级/计划
	// CompactThreshold 会话自动压缩阈值（估算 token）：0 用默认
	// （agent.DefaultCompactThreshold），正数显式覆盖，负数关闭自动压缩。
	CompactThreshold int `json:"compact_threshold,omitempty"`
	// Plugins MCP 插件接入配置。nil（配置文件未写 plugins 键）= 自动发现
	// ~/.grf/plugins 下的可执行文件；[]（显式空数组）= 关闭自动发现；
	// 非空 = 仅使用列出的插件（本地 command / 远程 url，见 Plugin）。
	Plugins []Plugin `json:"plugins,omitempty"`
}

// Plugin 一个 MCP 插件接入项：本地可执行文件（stdio 模式，harness 直接
// spawn）与远程端点（Streamable HTTP 模式，远端以 `插件 serve` 启动）二选一。
type Plugin struct {
	Name    string   `json:"name,omitempty"`    // 展示名；缺省取可执行文件名 / URL 主机
	Command []string `json:"command,omitempty"` // 本地模式：可执行文件 + 参数
	URL     string   `json:"url,omitempty"`     // 远程模式：MCP Streamable HTTP 端点
	Token   string   `json:"token,omitempty"`   // 远程模式 Bearer token（可选）
	Timeout int      `json:"timeout,omitempty"` // 单次工具调用超时秒数；0 用默认
}

// ParsePolicy 解析统一规则串（竖线组合）：write-workspace（写侧保护）、
// read-workspace（读边界）。空值 = 默认 off（全关）；"off" 同样全关；含至少
// 一个合法 token 时按 token 显式生效；一个都不认识视为解析出错，回退
// write-workspace（保写侧，读侧放开）。
func ParsePolicy(v string) (writeLimit, readLimit bool) {
	v = strings.TrimSpace(v)
	if v == "" || v == "off" {
		return false, false
	}
	seen := false
	for _, t := range strings.Split(v, "|") {
		switch strings.TrimSpace(t) {
		case "write-workspace":
			writeLimit = true
			seen = true
		case "read-workspace":
			readLimit = true
			seen = true
		}
	}
	if !seen {
		return true, false // 解析出错回退 write-workspace
	}
	return
}

// NormalizePolicy 归一规则串（非法回退默认），用于展示与落盘。
func NormalizePolicy(v string) string {
	w, r := ParsePolicy(v)
	switch {
	case w && r:
		return "write-workspace|read-workspace"
	case w:
		return "write-workspace"
	case r:
		return "read-workspace"
	default:
		return "off"
	}
}

// Effective 运行时生效的配置。
type Effective struct {
	Provider string
	BaseURL  string
	APIKey   string
	Model    string
}

const (
	DefaultBaseURL = "https://api.openai.com/v1"
	DefaultModel   = "gpt-4o"
)

// Home 返回配置根目录（~/.grf，GRF_HOME 覆盖）。
func Home() string {
	if v := os.Getenv("GRF_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".grf"
	}
	return filepath.Join(home, ".grf")
}

// Path 配置文件路径。
func Path() string {
	return filepath.Join(Home(), "settings.json")
}

// LoadFileForUpdate 读取纯文件内容（不做 env 合并）；不存在返回空配置。
func LoadFileForUpdate() (*Settings, error) {
	s := &Settings{Providers: map[string]*Provider{}}
	path := Path()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if s.Providers == nil {
		s.Providers = map[string]*Provider{}
	}
	if s.Policy == "" {
		s.Policy = "off"
	} else if n := NormalizePolicy(s.Policy); n != strings.TrimSpace(s.Policy) {
		// 解析出错（回退 write-workspace）或含多余空白：修正后回写文件，
		// 避免每次加载都重新走回退
		s.Policy = n
		_ = s.Save()
	}
	return s, nil
}

// Load 返回运行时生效的配置（应用环境变量覆盖）。
func Load() (Effective, error) {
	var eff Effective
	s, err := LoadFileForUpdate()
	if err != nil {
		return eff, err
	}
	name, p, model := s.Active()
	eff.Provider = name
	eff.BaseURL, eff.Model = DefaultBaseURL, DefaultModel
	if p != nil {
		if p.BaseURL != "" {
			eff.BaseURL = p.BaseURL
		}
		eff.APIKey = p.APIKey
		if model != "" {
			eff.Model = model
		}
	}
	if v := os.Getenv("OPENAI_BASE_URL"); v != "" {
		eff.BaseURL = v
	}
	if v := os.Getenv("OPENAI_API_KEY"); v != "" {
		eff.APIKey = v
	}
	if v := os.Getenv("GRF_MODEL"); v != "" {
		eff.Model = v
	}
	return eff, nil
}

// Active 解析默认提供商：Default 指向的 "provider/model"，否则字典序第一个。
func (s *Settings) Active() (string, *Provider, string) {
	if s.Providers == nil {
		s.Providers = map[string]*Provider{}
	}
	if pn, model := SplitDefault(s.Default); pn != "" {
		if p, ok := s.Providers[pn]; ok {
			if model == "" && len(p.Models) > 0 {
				model = p.Models[0]
			}
			return pn, p, model
		}
	}
	names := s.SortedProviderNames()
	if len(names) == 0 {
		return "default", nil, ""
	}
	name := names[0]
	p := s.Providers[name]
	model := ""
	if len(p.Models) > 0 {
		model = p.Models[0]
	}
	return name, p, model
}

// SplitDefault 拆分 "provider/model"。
func SplitDefault(def string) (provider, model string) {
	parts := strings.SplitN(def, "/", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return parts[0], ""
}

// SortedProviderNames 提供商名列表（字典序）。
func (s *Settings) SortedProviderNames() []string {
	names := make([]string, 0, len(s.Providers))
	for name := range s.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ModelEntry 一个可选模型条目（列表/切换 UI 共用的规范枚举）。
type ModelEntry struct {
	Provider string
	Model    string
	Default  bool // settings 的 default 指向该条目
}

// ListModels 全部可选 provider/model 条目（提供商字典序、模型配置序）。
func (s *Settings) ListModels() []ModelEntry {
	var out []ModelEntry
	for _, pn := range s.SortedProviderNames() {
		for _, m := range s.Providers[pn].Models {
			out = append(out, ModelEntry{Provider: pn, Model: m, Default: s.Default == pn+"/"+m})
		}
	}
	return out
}

// AddModel 向提供商添加模型（去重）。
func (p *Provider) AddModel(model string) bool {
	for _, m := range p.Models {
		if m == model {
			return false
		}
	}
	p.Models = append(p.Models, model)
	return true
}

// Save 写入配置文件（含 api_key，权限 0600）。
func (s *Settings) Save() error {
	path := Path()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, append(data, '\n'), 0o600)
}
