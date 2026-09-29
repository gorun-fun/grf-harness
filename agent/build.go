// build.go 组装完整的 harness：注册全部 28 个工具（与内嵌 spec
// 一一对齐）、构建根 agent 与子代理工厂。根会话与子代理共享 job/fs 实例
// 的边界：子代理拥有独立 JobManager/FsPolicy（其工作区一致）；goal 与
// ralph 仅根可用。
package agent

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/llm"
	"git.gorun.fun/share/grf-harness/plugin"
	"git.gorun.fun/share/grf-harness/store"
	"git.gorun.fun/share/grf-harness/tools"
)

// Harness 一次会话的完整组装结果。
type Harness struct {
	Root      *Agent
	Manager   *Manager
	Goals     *GoalState
	Todos     *tools.TodoState
	Fs        *tools.FsPolicy
	Artifacts *tools.Artifacts
	Skills    *tools.SkillCatalog
	Plugins   *plugin.Manager // MCP 插件（进程级共享；NoPlugins 时为 nil）
	Store     *store.Store

	clientMu sync.Mutex // client 引用的并发读写（运行中切换模型，见 SwitchClient）
	client   *llm.Client

	Settings *config.Settings
	root     string
	seq      atomic.Int64
	cacheKey string   // 会话级 prompt_cache_key；随会话文件持久化，resume 复用
	disabled []string // 按场景禁用的工具名（Options.DisabledTools）
	defSkill string   // 默认技能名（Options.DefaultSkill；空 = 不注入默认技能，gh CLI 终端形态配 grf-usage）
	plugWarn []string // 插件状态警告（构建期取一次；子代理提示词复用）
}

// Options 组装参数。
type Options struct {
	Workspace     string
	Client        *llm.Client
	Settings      *config.Settings // 可为 nil（用默认）
	Host          tools.Host       // 交互宿主；nil 用 NoopHost
	MaxSteps      int
	SystemPrompt  string              // 用户自定义 custom 段（身份/工作方式）；空用内置默认。inner prompt（内置工具说明）恒定拼装，不被它替换
	DepthLimit    int                 // 子代理深度上限；0 用默认
	DisabledTools []string            // 按场景禁用的工具名（根与子代理统一剔除）
	Skills        *tools.SkillCatalog // 技能目录；nil 自动加载两级默认目录，非 nil 完全替换（空目录即关闭技能）
	DefaultSkill  string              // 默认技能名（目录中存在时全文注入系统提示词）；空 = 不注入，gh CLI 终端形态配 DefaultSkill 常量（grf-usage）
	NoPlugins     bool                // 关闭 MCP 插件接入（嵌入方裁剪；go test 下插件本就自动空转）
	// StoreRoot 会话存档根目录（<root>/.grf/sessions）；空则用 Workspace。
	// 多会话进程（如 gh server）必须按会话隔离，否则新会话会恢复到
	// workspace 里最新一次存档——把别的会话的历史带进来。
	StoreRoot string
}

// Build 组装根 harness（全部 28 个工具就位）。
func Build(opts Options) (*Harness, error) {
	if opts.Workspace == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		opts.Workspace = wd
	}
	st := opts.Settings
	if st == nil {
		s, err := config.LoadFileForUpdate()
		if err != nil {
			return nil, fmt.Errorf("加载配置: %w", err)
		}
		st = s
	}
	host := opts.Host
	if host == nil {
		host = &tools.NoopHost{AutoApprove: st.AutoApprove}
	}
	artifacts, err := tools.NewArtifacts()
	if err != nil {
		return nil, err
	}

	skills := opts.Skills
	if skills == nil {
		skills = tools.LoadSkills(opts.Workspace)
	}
	// MCP 插件（进程级共享；go test 下自动空转）。连接失败只降级为警告。
	var pm *plugin.Manager
	if !opts.NoPlugins {
		pm = plugin.Shared(st)
	}
	storeRoot := opts.StoreRoot
	if storeRoot == "" {
		storeRoot = opts.Workspace
	}
	h := &Harness{
		Goals:     &GoalState{},
		Todos:     &tools.TodoState{},
		Artifacts: artifacts,
		Skills:    skills,
		Plugins:   pm,
		Store:     store.New(storeRoot),
		client:    opts.Client,
		Settings:  st,
		root:      opts.Workspace,
		cacheKey:  newCacheKey(),
		disabled:  opts.DisabledTools,
		defSkill:  opts.DefaultSkill,
	}
	h.bindClient(opts.Client)
	h.Fs = h.newFsPolicy(host)
	// 插件连接失败不致命（警告进入系统提示词，见 BuildSystemPrompt），
	// 但要告知模型：相关工具调用会失败。构建期取一次，子代理工厂复用。
	if pm != nil {
		h.plugWarn = pm.Warnings()
	}

	root := NewAgent("agent_root", opts.Client, tools.NewRegistry(), "")
	root.host = host
	root.Todos = h.Todos
	root.Fs = h.Fs
	root.Jobs = tools.NewJobManager()
	if opts.MaxSteps > 0 {
		root.SetMaxSteps(opts.MaxSteps)
	} else if st.MaxSteps > 0 {
		root.SetMaxSteps(st.MaxSteps)
	}
	// 自动压缩阈值：会话级配置（0=默认 / 正数覆盖 / 负数关闭），仅根
	// agent——子代理历史短生命周期，压缩收益不抵风险
	root.SetCompactThreshold(st.CompactThreshold)

	depth := opts.DepthLimit
	if depth <= 0 {
		depth = 3
	}
	h.Manager = NewManager(root, depth)
	root.mgr = h.Manager
	root.goals = h.Goals
	h.Root = root

	// 根工具链（28 个全部注册）
	h.registerTools(root, true)
	// 提示词在工具注册后组装：inner prompt 按实际注册结果（含
	// DisabledTools 剔除）拼装，说明段与真实可用工具集一致。
	root.SetSystemText(h.systemPromptFor(root.Reg(), false, "", opts.SystemPrompt))
	// 子代理工厂
	h.Manager.NewChild = func(parent *Agent, label string, seed []llm.Message) *Agent {
		return h.newChild(parent, label, seed)
	}

	h.Todos.OnChange = func(list []tools.Todo) {
		root.emit("todos", tools.MarshalTodos(list))
	}
	return h, nil
}

// CacheKey 当前会话的 prompt_cache_key（构建期随机生成，resume 时换为
// 会话文件里存的值）。绑定在 client 上随每次请求发送：支持该字段的
// 服务端会把同一会话的请求路由到同一缓存分片，提高前缀缓存命中。
func (h *Harness) CacheKey() string { return h.cacheKey }

// SetCacheKey 替换会话缓存键并同步到当前 client（resume 续接旧会话用）。
func (h *Harness) SetCacheKey(key string) {
	if key == "" {
		return
	}
	h.cacheKey = key
	h.bindClient(h.getClient())
}

// bindClient 把缓存键绑定到 client（client 可为 nil，测试用）。
func (h *Harness) bindClient(c *llm.Client) {
	if c != nil {
		c.SetCacheKey(h.cacheKey)
	}
}

// getClient 取当前 client 引用（副本）。运行中切换模型（SwitchClient）与
// 子代理工厂（newChild）、bash 事实变量（managedEnv）、提示词装配
// （systemPromptFor）并发，引用经 clientMu 保护。
func (h *Harness) getClient() *llm.Client {
	h.clientMu.Lock()
	defer h.clientMu.Unlock()
	return h.client
}

// SwitchClient 切换会话当前模型：换 client 引用、迁移缓存路由键，并重写
// 系统提示词的模型身份行。h.client 与 root 的 client 必须一起换——子代理
// （newChild）按 h.client 取 client，bash 的 GRF_MODEL 又懒取自它（Env 为
// 函数），只换 root 会让切换后新生的子代理仍跑旧模型；身份行是会话构建期
// 烘焙的，不重写则模型以旧身份自我认知（实际调新模型却自称旧模型名）。
// 并发安全（引用经锁）：可与运行中的回合并发调用，但建议在回合边界切换，
// 避免同一回合前后半落在不同模型上。
func (h *Harness) SwitchClient(c *llm.Client) {
	h.clientMu.Lock()
	h.client = c
	h.clientMu.Unlock()
	h.bindClient(c)
	h.Root.SetClient(c)
	h.Root.UpdateModelIdentity(c.Model())
}

// newCacheKey 生成会话缓存键（随机 16 字节 hex）。
func newCacheKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("grf-%d", time.Now().UnixNano())
	}
	return "grf-" + hex.EncodeToString(b[:])
}

// systemPromptFor 构建指定角色（根/子代理）的系统提示词；根可用
// Options.SystemPrompt 覆盖内置默认 custom 段（身份/工作方式），inner
// prompt（内置工具说明）两者恒定拼装。根与子代理的提示词装配共用此入口
// （插件警告两者都携带——插件工具对子代理同样注册）。reg 为该角色注册
// 完成后的工具注册表，inner prompt 只为其中实际可用的内置工具拼说明段。
func (h *Harness) systemPromptFor(reg *tools.Registry, subagent bool, label, override string) string {
	model := ""
	if c := h.getClient(); c != nil {
		model = c.Model()
	}
	var names []string
	if reg != nil {
		names = reg.Registered()
	}
	if subagent {
		// ask_user_question 在子代理注册的是拒绝桩（不可用），不拼说明段
		names = slices.DeleteFunc(slices.Clone(names), func(n string) bool {
			return n == "ask_user_question"
		})
	}
	return BuildSystemPrompt(PromptOptions{
		Workspace:      h.root,
		Model:          model,
		Skills:         h.Skills,
		Subagent:       subagent,
		Label:          label,
		PluginWarnings: h.plugWarn, // 构建期取好，根与子代理同一份
		Tools:          names,
		DefaultSkill:   h.defSkill,
	}, override)
}

// policyLimit 解析统一规则串（config.ParsePolicy），一次取回写/读限制开关。
func (h *Harness) policyLimit() (writeLimit, readLimit bool) {
	return config.ParsePolicy(h.Settings.Policy)
}

// newFsPolicy 按统一规则串构建文件策略（见 config.ParsePolicy）：
// write-workspace → 写前先读；read-workspace → 读边界；空值默认 off 全关。
func (h *Harness) newFsPolicy(host tools.Host) *tools.FsPolicy {
	writeLimit, readLimit := h.policyLimit()
	return tools.NewFsPolicy(h.root, writeLimit, readLimit, host)
}

// applyPolicy 应用新 policy（set_policy 经用户批准后调用）：更新根 FsPolicy
// 与内存 Settings——不落盘，仅本会话生效；后续生成的子代理按更新后的
// Settings 构建，已存在的子代理策略不变。
func (h *Harness) applyPolicy(normalized string) {
	h.Settings.Policy = normalized
	writeLimit, readLimit := config.ParsePolicy(normalized)
	h.Fs.SetPolicy(writeLimit, readLimit)
}

// SetPolicy 宿主（GUI 等）直接变更本会话 policy：规范化后走与 set_policy
// 批准落地同一条路径，立即生效、仅本会话、不落盘。返回规范化结果。
func (h *Harness) SetPolicy(policy string) string {
	n := config.NormalizePolicy(policy)
	h.applyPolicy(n)
	return n
}

// Policy 当前生效的 policy（规范化串；运行时变更后即时反映）。
func (h *Harness) Policy() string {
	return config.NormalizePolicy(h.Settings.Policy)
}

// registerTools 为 agent 注册其可用工具。root=true 注册全部 28 个；
// 子代理不注册 goal 三件套与 ralph（文档语义：非人类/子代理权限拒绝，
// 直接不给入口；ask_user 子代理同样不可用）。
func (h *Harness) registerTools(a *Agent, root bool) {
	r := a.reg

	tools.RegisterFs(r, h.root, h.Fs, h.Artifacts)
	tools.RegisterReadImage(r, h.Fs)
	// 沙箱开关经 FsPolicy 实时读取（set_policy 批准后立即生效）
	tools.RegisterBash(r, tools.BashDeps{
		Workspace: h.root,
		Host:      func() tools.Host { return a.host }, // 懒取：SetHost 后生效
		Fs:        h.Fs,
		Jobs:      a.Jobs,
		Artifacts: h.Artifacts,
		Env:       h.managedEnv, // 懒取：切换模型后 GRF_MODEL 即时反映
	})
	tools.RegisterJobTools(r, a.Jobs)
	tools.RegisterWebSearch(r)
	tools.RegisterSkill(r, h.Skills)
	tools.RegisterTodoWrite(r, h.Todos)
	tools.RegisterMemory(r, h.memoryDeps())
	RegisterAgentTools(r, a, h.Manager)
	RegisterWorkflow(r, a, h.Manager)
	if root {
		tools.RegisterAskUser(r, func() tools.Host { return a.host })
		// set_policy 仅根：策略是会话级安全边界，子代理不可申请变更
		tools.RegisterSetPolicy(r, func() tools.Host { return a.host }, h.applyPolicy)
		RegisterGoalTools(r, a, h.Goals)
		RegisterRalph(r, a, h.Manager)
		// enter_plan_mode 仅根：模式切换是根会话状态，子代理不可申请
		tools.RegisterEnterPlanMode(r, planCtl{a})
	} else {
		// 子代理问用户：明确拒绝（不可达最终用户）
		tools.RegisterAskUserDenied(r)
	}
	tools.RegisterExitPlanMode(r, planCtl{a}, func() tools.Host { return a.host }) // 懒取：SetHost 后生效
	// 插件工具（MCP）：根与子代理都注册（底层连接进程级共享）；
	// 未标只读的插件工具在计划模式下按改动类拦截
	if h.Plugins != nil {
		a.planExtra = h.Plugins.RegisterInto(r)
	}
	if len(h.disabled) > 0 {
		r.Disable(h.disabled...)
	}
}

// managedEnv bash 子进程注入的 harness 事实变量。注册时以方法值懒取
// （BashDeps.Env 为函数）：切换模型后新建 bash 进程的 GRF_MODEL 即时反映。
func (h *Harness) managedEnv() []string {
	model := ""
	if c := h.getClient(); c != nil {
		model = c.Model()
	}
	return []string{
		"DSH_WORKSPACE=" + h.root,
		"DSH_MODEL=" + model,
		"GRF_WORKSPACE=" + h.root,
		"GRF_MODEL=" + model,
	}
}

// memoryDeps memory 工具的两级落点：project = 工作区 .grf/memory.json
// （终端交互的记忆主体），user = ~/.grf/memory.json（config.Home()，
// GRF_HOME 可重定位）。JSON 条目库增删改查，根与子代理共用同一对文件。
func (h *Harness) memoryDeps() tools.MemoryDeps {
	return tools.MemoryDeps{
		ProjectPath: filepath.Join(h.root, ".grf", tools.MemoryFile),
		UserPath:    filepath.Join(config.Home(), tools.MemoryFile),
	}
}

// newChild 构建子代理（独立历史与工具链；goal/ralph/ask_user 不可用）。
func (h *Harness) newChild(parent *Agent, label string, seed []llm.Message) *Agent {
	child := NewAgent("pending", h.getClient(), tools.NewRegistry(), "")
	child.host = &tools.NoopHost{AutoApprove: h.Settings.AutoApprove, OnEvent: nil}
	child.Todos = &tools.TodoState{}
	child.Fs = h.newFsPolicy(child.host)
	child.Jobs = tools.NewJobManager()
	child.SetMaxSteps(parent.MaxSteps())
	h.registerTools(child, false)
	// 提示词在工具注册后组装（inner prompt 按子代理实际工具集拼装）
	system := h.systemPromptFor(child.Reg(), true, label, "")
	child.SetSystemText(system)
	if len(seed) > 0 {
		child.mu.Lock()
		child.history = append([]llm.Message{{Role: "system", Content: system}}, seed[1:]...)
		child.mu.Unlock()
	}
	// 计划模式开关仅根可切换，但改动拦截对子代理同样生效
	child.planGuard = h.Root.InPlanMode
	return child
}

// Close 清理（产物目录等）。
func (h *Harness) Close() {
	if h.Artifacts != nil {
		h.Artifacts.Cleanup()
	}
}

// VerifyAllToolsRegistered 校验根 agent 工具齐备（测试/自检用；
// Options.DisabledTools 列出的除外）。
func (h *Harness) VerifyAllToolsRegistered() error {
	reg := h.Root.reg
	skip := map[string]bool{}
	for _, n := range h.disabled {
		skip[n] = true
	}
	for _, name := range tools.SpecNames() {
		if skip[name] {
			continue
		}
		if !reg.Has(name) {
			return fmt.Errorf("tool %q not registered", name)
		}
	}
	return nil
}
