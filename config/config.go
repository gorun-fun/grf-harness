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
	// Orchestrator 编排者模式：根会话不注册内联调研工具（read/grep/
	// glob/web_search），采集经子代理、根只做综合（GRF_ORCHESTRATOR=1
	// 环境覆盖）。
	Orchestrator bool `json:"orchestrator,omitempty"`
	// CompactThreshold 会话自动压缩阈值（估算 token）：0 用默认
	// （agent.DefaultCompactThreshold），正数显式覆盖，负数关闭自动压缩。
	CompactThreshold int `json:"compact_threshold,omitempty"`
	// Plugins MCP 插件接入配置。nil（配置文件未写 plugins 键）= 自动发现
	// ~/.grf/plugins 下的可执行文件；[]（显式空数组）= 关闭自动发现；
	// 非空 = 仅使用列出的插件（本地 command / 远程 url，见 Plugin）。
	Plugins []Plugin `json:"plugins,omitempty"`
	// Weixin 微信渠道（gh connect weixin）的安全配置；nil = 全默认
	// （白名单空 = 拒绝所有消息，策略 = write-workspace|read-workspace）。
	Weixin *Weixin `json:"weixin,omitempty"`
	// Cluster 集群模式配置（多设备 gh 组成"指挥官-节点"集群）；nil = 不启用。
	Cluster *Cluster `json:"cluster,omitempty"`
}

// Cluster 集群模式配置。节点 = 开集群开关的 gh server（身份/能力上报、
// 接收派发任务）；指挥官 = role=commander 的 gh 实例（orchestrator 委派 +
// 路由层，把 subagent 任务派往远端节点，审批经暂停协议直达本地 Host）。
// nil 或 Enabled=false 时全部集群行为关闭（端点不注册、路由层不介入）。
type Cluster struct {
	Enabled bool `json:"enabled,omitempty"`
	// Role 角色：commander（指挥官：可路由任务到节点、收注册/心跳）或
	// node（节点：上报心跳、接收任务）。空 = node。commander 自身也是
	// 可执行任务的节点（角色可叠加，路由回退本地即指挥官自己执行）。
	Role string `json:"role,omitempty"`
	// Name 节点显示名 / 路由名（提示词里 @name 引用）；空 = hostname。
	Name string `json:"name,omitempty"`
	// Token 集群共享密钥：commander 的 /cluster/* 端点接受它鉴权（与
	// server 自身 token 并列）；节点侧凭它注册/心跳。
	Token string `json:"token,omitempty"`
	// InviteCode 加入邀请码（指挥官侧）：非空时 register/心跳新节点必须
	// 携带匹配的 invite 才准入（防局域网内任意 gh 实例接入）；节点侧把它
	// 作为注册凭据随 register/heartbeat 上送。空 = 开放注册（LAN 信任）。
	InviteCode string `json:"invite_code,omitempty"`
	// CommanderURL 节点侧 call-home 地址（http://host:port）；配置后节点
	// 定期向指挥官注册并心跳（NAT 友好，无需指挥官反向可达），并保持
	// /cluster/inbox SSE 指令通道（指挥官经中继向 NAT 后的节点派发任务）。
	CommanderURL string `json:"commander_url,omitempty"`
	// Discover 局域网自动发现开关：off | lan。空 = off。lan = mDNS 子集
	//（_grf-cluster._udp.local.）双向通告/发现：本机服务对外应答，指挥官
	// 侧浏览并把发现的节点并入成员表（能力清单用 cluster.token 拉取）。
	Discover string `json:"discover,omitempty"`
	// Nodes 手动节点表（指挥官侧静态种子，goder 设置页编辑对象）。
	// 派发任务经既有 ORS 端点直达（addr + 该节点的 server token）。
	Nodes []ClusterNode `json:"nodes,omitempty"`
	// Policy 派发任务的默认策略（每次任务的 grf:session.policy）；
	// 空 = off（节点侧默认）。
	Policy string `json:"policy,omitempty"`
	// Sync 任务工作区同步策略：none | seed | git | full。空/none = 关闭
	//（任务自包含）；seed = 模型经 subagent 的 seed_files 显式点名下发；
	// git = 派发时自动打包工作区 git 跟踪文件（未显式 seed_files 时）；
	// full = 自动打包整个工作区（跳过 .git）。档位只影响"打包什么"，
	// 传输/解包/产物回传通道一致。
	Sync string `json:"sync,omitempty"`
	// Route 路由参数；nil = 全默认。
	Route *ClusterRoute `json:"route,omitempty"`
}

// ClusterNode 手动节点表条目。
type ClusterNode struct {
	Addr   string   `json:"addr"`             // gh server 地址 host:port
	Token  string   `json:"token,omitempty"`  // 该节点的 server 访问令牌
	Name   string   `json:"name,omitempty"`   // 路由名（@name 引用；空取 addr 主机名）
	Labels []string `json:"labels,omitempty"` // 能力标签（description 词汇匹配路由）
	// Pin TLS 证书指纹（sha256 hex，叶子证书 DER）：https 节点自签部署时
	// 的 TOFU 信任锚——设置后跳过系统 CA 校验、只比对该指纹（未设置则走
	// 系统默认校验）。内网自签 + pin 即可免搭建私有 CA。
	Pin string `json:"pin,omitempty"`
}

// ClusterRoute 路由参数。
type ClusterRoute struct {
	// AllowLocal 无匹配节点时回退本地子代理（nil/true = 回退，false =
	// 无匹配即报错，强制显式路由）。指针语义区分"未设置"与"显式 false"。
	AllowLocal *bool `json:"allow_local,omitempty"`
	// MaxInflightPerNode 单节点在跑远端任务上限；0 = 默认 4。
	MaxInflightPerNode int `json:"max_inflight_per_node,omitempty"`
}

// IsCommander 是否指挥官角色（Enabled 前提）。
func (c *Cluster) IsCommander() bool {
	return c != nil && c.Enabled && strings.EqualFold(strings.TrimSpace(c.Role), "commander")
}

// RouteAllowLocal 无匹配是否回退本地（默认 true）。
func (c *Cluster) RouteAllowLocal() bool {
	return c == nil || c.Route == nil || c.Route.AllowLocal == nil || *c.Route.AllowLocal
}

// RouteMaxInflight 单节点在跑上限（默认 4）。
func (c *Cluster) RouteMaxInflight() int {
	if c == nil || c.Route == nil || c.Route.MaxInflightPerNode <= 0 {
		return 4
	}
	return c.Route.MaxInflightPerNode
}

// ClusterRuntime 运行时生效的集群配置：GRF_CLUSTER_ENABLED 环境覆盖
// （1/true/yes/on 开，0/false/no/off 关，其他值忽略）优先于 settings 的
// cluster.enabled。返回 nil = 集群未启用（一切集群行为关闭）。与
// OrchestratorRuntime 同理经独立函数判定而非 Load()：防止读改写链路把
// 覆盖值持久化进 settings.json。
func ClusterRuntime(s *Settings) *Cluster {
	enabled := s != nil && s.Cluster != nil && s.Cluster.Enabled
	if v := os.Getenv("GRF_CLUSTER_ENABLED"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			enabled = true
		case "0", "false", "no", "off":
			enabled = false
		}
	}
	if !enabled {
		return nil
	}
	if s == nil || s.Cluster == nil {
		return &Cluster{Enabled: true}
	}
	c := *s.Cluster // 值拷贝：运行时覆盖不回写调用方持有的 Settings
	c.Enabled = true
	// 节点表深拷贝（含 labels）：运行期对副本的增删不泄漏回 Settings
	c.Nodes = append([]ClusterNode(nil), s.Cluster.Nodes...)
	for i := range c.Nodes {
		c.Nodes[i].Labels = append([]string(nil), s.Cluster.Nodes[i].Labels...)
	}
	return &c
}

// Weixin 微信渠道安全配置：微信消息是任意外部输入，渠道默认必须收紧。
type Weixin struct {
	// AllowSenders 允许驱动 agent 的微信用户 id（from_user_id）白名单。
	// 空 = 拒绝所有消息并回执指引（默认拒绝）。
	AllowSenders []string `json:"allow_senders,omitempty"`
	// Policy 渠道策略覆盖；空 = write-workspace|read-workspace（比全局
	// 默认 off 更紧——远程消息渠道驱动的是本机 bash 与文件工具）。
	Policy string `json:"policy,omitempty"`
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
	// GRF_MODEL 环境覆盖支持三类写法（与会话层模型切换同一套解析，
	// 见 Settings.ResolveModel）：裸名（默认供应商含之则等价现状，否则
	// "恰一家提供"时切换）、限定名 provider/model（供应商+模型表内校验）、
	// 限定名但模型未登记（已知供应商即信任显式覆盖——测试新模型不必先改
	// settings）。均不命中时原样透传（老行为：整串当模型名发给当前供应商）。
	if v := os.Getenv("GRF_MODEL"); v != "" {
		pn, m, ok := s.ResolveModel(v)
		if !ok {
			// 整体解析未命中：限定名且供应商已知时仍信任显式覆盖
			//（模型名免登记，方便对新模型做临时测试）
			if p2, m2 := SplitDefault(v); p2 != "" && m2 != "" {
				if _, exists := s.Providers[p2]; exists {
					pn, m, ok = p2, m2, true
				}
			}
		}
		if ok {
			eff.Provider, eff.Model = pn, m
		} else {
			eff.Model = v
		}
		if eff.Provider != name { // 环境变量切换了供应商：端点与密钥随之
			if p2 := s.Providers[eff.Provider]; p2 != nil {
				if p2.BaseURL != "" {
					eff.BaseURL = p2.BaseURL
				}
				eff.APIKey = p2.APIKey
			}
		}
	}
	// OPENAI_* 在 GRF_MODEL 之后应用：显式指定的端点/密钥拥有最终决定权
	if v := os.Getenv("OPENAI_BASE_URL"); v != "" {
		eff.BaseURL = v
	}
	if v := os.Getenv("OPENAI_API_KEY"); v != "" {
		eff.APIKey = v
	}
	return eff, nil
}

// OrchestratorRuntime 运行时编排者模式判定：GRF_ORCHESTRATOR 环境覆盖
// （1/true/yes/on 开，0/false/no/off 关，其他值忽略）优先于 settings 的
// orchestrator 字段。进程级开关、不落盘——经此判定而非 Load()：Effective
// 不进 Harness，且 LoadFileForUpdate 的读改写链路若被环境变量污染会把
// 覆盖值持久化进 settings.json。
func OrchestratorRuntime(s *Settings) bool {
	if v := os.Getenv("GRF_ORCHESTRATOR"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return s != nil && s.Orchestrator
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

// ResolveModel 把模型名解析为 (provider, model)：带 provider/ 前缀的
// 限定名要求供应商存在且模型在其表内；裸模型名按"默认供应商含该模型
// 优先，否则全表恰一家命中"解析。零命中、歧义（多供应商同名且默认
// 供应商不含）或供应商未知返回 ok=false——调用方自行决定回退语义。
// GRF_MODEL 环境覆盖与会话层模型切换（server 的 trySwitchModel）共用。
// 选解核心在 ResolveModelEx（含歧义候选表），勿在别处复制选解逻辑。
func (s *Settings) ResolveModel(name string) (provider, model string, ok bool) {
	provider, model, ok, _ = s.ResolveModelEx(name)
	return provider, model, ok
}

// ResolveModelEx ResolveModel 的选解核心，另带回歧义候选供应商表
// （多家同名命中且默认供应商不含时，candidates 即命中清单，字典序）——
// /v1/agent/models/resolve 端点消费，宿主（goder-serve 管理面转发）据此
// 做无头任务的 fail-fast 判定。
func (s *Settings) ResolveModelEx(name string) (provider, model string, ok bool, candidates []string) {
	if name == "" {
		return "", "", false, nil
	}
	if s.Providers == nil {
		return "", "", false, nil
	}
	// 限定名以斜杠存在性判断——SplitDefault 对无斜杠输入返回非空
	// provider（整个字符串），按 provider=="" 判定会让裸名分支永远
	// 走不到（曾使 server 的裸模型名解析成为死代码）。
	if strings.Contains(name, "/") {
		pn, m := SplitDefault(name)
		if m == "" {
			return "", "", false, nil
		}
		p, exists := s.Providers[pn]
		if !exists {
			return "", "", false, nil
		}
		for _, x := range p.Models {
			if x == m {
				return pn, m, true, nil
			}
		}
		return "", "", false, nil
	}
	defProvider, _ := SplitDefault(s.Default)
	if p, exists := s.Providers[defProvider]; exists {
		for _, x := range p.Models {
			if x == name {
				return defProvider, name, true, nil
			}
		}
	}
	// 全表扫描收集命中（同供应商表内重复登记只计一家；跨供应商多家
	// 命中且默认供应商不含 = 歧义，候选表如实上报）。
	var hits []string
	for _, pn := range s.SortedProviderNames() {
		for _, x := range s.Providers[pn].Models {
			if x == name {
				hits = append(hits, pn)
				break
			}
		}
	}
	switch len(hits) {
	case 0:
		return "", "", false, nil // 零命中
	case 1:
		return hits[0], name, true, nil
	default:
		return "", "", false, hits // 歧义
	}
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
