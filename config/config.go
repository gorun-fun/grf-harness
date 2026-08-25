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
// Sandbox: "off"（默认，不做文件沙箱）| "workspace-write"（限制写工作区）。
// FsPolicy: "observe"（默认，写/改前需先 read）| "off"。
type Settings struct {
	Providers    map[string]*Provider `json:"providers"`
	Default      string               `json:"default,omitempty"`
	Mouse        *bool                `json:"mouse,omitempty"`
	SystemPrompt string               `json:"system_prompt,omitempty"`
	MaxSteps     int                  `json:"max_steps,omitempty"`
	Sandbox      string               `json:"sandbox,omitempty"`
	FsPolicy     string               `json:"fs_policy,omitempty"`
	AutoApprove  bool                 `json:"auto_approve,omitempty"` // 非交互时自动批准沙箱升级/计划
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
func Path() (string, error) {
	return filepath.Join(Home(), "settings.json"), nil
}

// LoadFileForUpdate 读取纯文件内容（不做 env 合并）；不存在返回空配置。
func LoadFileForUpdate() (*Settings, error) {
	s := &Settings{Providers: map[string]*Provider{}}
	path, _ := Path()
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
	if s.Sandbox == "" {
		s.Sandbox = "off"
	}
	if s.FsPolicy == "" {
		s.FsPolicy = "observe"
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
	for _, name := range s.SortedProviderNames() {
		p := s.Providers[name]
		model := ""
		if len(p.Models) > 0 {
			model = p.Models[0]
		}
		return name, p, model
	}
	return "default", nil, ""
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
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}
