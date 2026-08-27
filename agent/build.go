// build.go 组装完整的 harness：注册全部 25 个工具（与 docs/tools.json
// 一一对齐）、构建根 agent 与子代理工厂。根会话与子代理共享 job/fs 实例
// 的边界：子代理拥有独立 JobManager/FsPolicy（其工作区一致）；goal 与
// ralph 仅根可用。
package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"grf/config"
	"grf/llm"
	"grf/store"
	"grf/tools"
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
	Store     *store.Store
	client    *llm.Client
	Settings  *config.Settings
	root      string
	seq       atomic.Int64
	cacheKey  string   // 会话级 prompt_cache_key；随会话文件持久化，resume 复用
	disabled  []string // 按场景禁用的工具名（Options.DisabledTools）
}

// Options 组装参数。
type Options struct {
	Workspace     string
	Client        *llm.Client
	Settings      *config.Settings // 可为 nil（用默认）
	Host          tools.Host       // 交互宿主；nil 用 NoopHost
	MaxSteps      int
	SystemPrompt  string   // 用户自定义静态提示词；空用内置
	DepthLimit    int      // 子代理深度上限；0 用默认
	DisabledTools []string // 按场景禁用的工具名（根与子代理统一剔除）
}

// Build 组装根 harness（全部 25 个工具就位）。
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

	h := &Harness{
		Goals:     &GoalState{},
		Todos:     &tools.TodoState{},
		Artifacts: artifacts,
		Skills:    tools.LoadSkills(opts.Workspace),
		Store:     store.New(opts.Workspace),
		client:    opts.Client,
		Settings:  st,
		root:      opts.Workspace,
		cacheKey:  newCacheKey(),
		disabled:  opts.DisabledTools,
	}
	h.bindClient(opts.Client)
	observe := st.FsPolicy != "off"
	h.Fs = tools.NewFsPolicy(opts.Workspace, observe, host)

	skills := h.Skills
	model := ""
	if opts.Client != nil {
		model = opts.Client.Model()
	}
	system := BuildSystemPrompt(PromptOptions{
		Workspace: opts.Workspace,
		Model:     model,
		Skills:    skills,
	}, opts.SystemPrompt)

	root := NewAgent("agent_root", opts.Client, tools.NewRegistry(), h.Store, opts.Workspace, system)
	root.host = host
	root.Todos = h.Todos
	root.Fs = h.Fs
	root.Jobs = tools.NewJobManager()
	if opts.MaxSteps > 0 {
		root.SetMaxSteps(opts.MaxSteps)
	} else if st.MaxSteps > 0 {
		root.SetMaxSteps(st.MaxSteps)
	}

	depth := opts.DepthLimit
	if depth <= 0 {
		depth = 3
	}
	h.Manager = NewManager(root, depth)
	root.mgr = h.Manager
	root.goals = h.Goals
	h.Root = root

	// 根工具链（25 个全部注册）
	h.registerTools(root, true)
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
	h.bindClient(h.client)
}

// bindClient 把缓存键绑定到 client（client 可为 nil，测试用）。
func (h *Harness) bindClient(c *llm.Client) {
	if c != nil {
		c.SetCacheKey(h.cacheKey)
	}
}

// newCacheKey 生成会话缓存键（随机 16 字节 hex）。
func newCacheKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("grf-%d", time.Now().UnixNano())
	}
	return "grf-" + hex.EncodeToString(b[:])
}

// registerTools 为 agent 注册其可用工具。root=true 注册全部 25 个；
// 子代理不注册 goal 三件套与 ralph（文档语义：非人类/子代理权限拒绝，
// 直接不给入口；ask_user 子代理同样不可用）。
func (h *Harness) registerTools(a *Agent, root bool) {
	r := a.reg
	env := h.managedEnv()

	tools.RegisterFs(r, h.root, h.Fs, h.Artifacts)
	tools.RegisterReadImage(r)
	tools.RegisterBash(r, tools.BashDeps{
		Workspace: h.root,
		Host:      a.host,
		Jobs:      a.Jobs,
		Artifacts: h.Artifacts,
		SandboxOn: h.Settings.Sandbox == "workspace-write",
		Env:       env,
	})
	tools.RegisterJobTools(r, a.Jobs)
	tools.RegisterWebSearch(r)
	tools.RegisterSkill(r, h.Skills)
	tools.RegisterTodoWrite(r, h.Todos)
	RegisterAgentTools(r, a, h.Manager)
	RegisterWorkflow(r, a, h.Manager)
	if root {
		tools.RegisterAskUser(r, func() tools.Host { return a.host })
		RegisterGoalTools(r, a, h.Goals)
		RegisterRalph(r, a, h.Manager)
	} else {
		// 子代理问用户：明确拒绝（不可达最终用户）
		tools.RegisterAskUserDenied(r)
	}
	tools.RegisterExitPlanMode(r, planCtl{a}, a.host)
	if len(h.disabled) > 0 {
		r.Disable(h.disabled...)
	}
}

// managedEnv bash 子进程注入的 harness 事实变量。
func (h *Harness) managedEnv() []string {
	model := ""
	if h.client != nil {
		model = h.client.Model()
	}
	return []string{
		"DSH_WORKSPACE=" + h.root,
		"DSH_MODEL=" + model,
		"GRF_WORKSPACE=" + h.root,
		"GRF_MODEL=" + model,
	}
}

// newChild 构建子代理（独立历史与工具链；goal/ralph/ask_user 不可用）。
func (h *Harness) newChild(parent *Agent, label string, seed []llm.Message) *Agent {
	model := ""
	if h.client != nil {
		model = h.client.Model()
	}
	system := BuildSystemPrompt(PromptOptions{
		Workspace: h.root,
		Model:     model,
		Skills:    h.Skills,
		Subagent:  true,
		Label:     label,
	}, "")
	child := NewAgent("pending", h.client, tools.NewRegistry(), h.Store, h.root, system)
	child.host = &tools.NoopHost{AutoApprove: h.Settings.AutoApprove, OnEvent: nil}
	child.Todos = &tools.TodoState{}
	child.Fs = tools.NewFsPolicy(h.root, h.Settings.FsPolicy != "off", child.host)
	child.Jobs = tools.NewJobManager()
	child.SetMaxSteps(parent.MaxSteps())
	if len(seed) > 0 {
		child.mu.Lock()
		child.history = append([]llm.Message{{Role: "system", Content: system}}, seed[1:]...)
		child.mu.Unlock()
	}
	h.registerTools(child, false)
	// 计划模式仅根
	return child
}

// planCtl exit_plan_mode 的控制器实现。
type planCtl struct{ a *Agent }

func (p planCtl) InPlanMode() bool { return p.a.InPlanMode() }

func (p planCtl) SubmitPlan(ctx context.Context, plan string) (bool, string) {
	if p.a.host == nil {
		return false, ""
	}
	return p.a.host.ApprovePlan(ctx, plan)
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
