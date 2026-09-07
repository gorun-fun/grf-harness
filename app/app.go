// Package app 应用组装层：作为库嵌入与 CLI 共用的入口。提供
// App（根 harness 的门面）与会话管理（保存/恢复/切换模型）。
//
// 按功能线条分文件：render.go（单任务直出渲染）、taskhost.go（stdin/
// stdout 交互宿主）、repl.go（非 TTY 行模式 REPL）。
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"git.gorun.fun/share/grf-harness/agent"
	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/llm"
	"git.gorun.fun/share/grf-harness/store"
	"git.gorun.fun/share/grf-harness/tools"
)

// Version 版本号，事实来源在 config.Version（构建时经 build.sh 注入，见 config/version.go）。
var Version = config.Version

// Options 应用构建参数（库使用方入口）。
//
// 多租户用户隔离推荐用法：Workspace 设为该用户的专属文件夹，Policy 显式
// 设为 "read-workspace"（默认 off 不设读边界；写边界本就无条件生效），bash 由
// DisabledTools 剔除，升级通道用自定义 Host 的 ApproveEscalation 返回
// false 焊死（用户不可信、无审批方）。
type Options struct {
	Workspace     string                    // 工作区；默认 cwd
	Client        *llm.Client               // 指定客户端；nil 时从配置加载
	Host          tools.Host                // 交互宿主；nil 时按交互性自动选择
	MaxSteps      int                       // 循环上限；0 用配置/默认
	SystemPrompt  string                    // 自定义静态提示词；空用配置文件/内置
	DepthLimit    int                       // 子代理深度上限
	DisabledTools []string                  // 按场景禁用的工具名
	Skills        *tools.SkillCatalog       // 技能目录；nil 自动加载默认目录，非 nil 完全替换（空目录即关闭技能）
	VerboseEvents func(kind, detail string) // 全部事件回调（挂到根 agent；RunTask 会覆盖，见其注释）
	OnToolEvent   func(agent.ToolEvent)     // 结构化工具事件回调（挂到根 agent，子代理事件也回流；见 docs/integration.md §5）

	// 运行策略覆盖：仅本次构建生效、不写回配置文件；空/nil 沿用
	// ~/.grf/settings.json（嵌入方可用它摆脱对机器全局配置的依赖）。
	Policy      string // 统一规则串："write-workspace|read-workspace" | "write-workspace" | "read-workspace" | "off"（空值默认 off），见 config.ParsePolicy
	AutoApprove *bool  // 非交互自动批准沙箱升级/计划
}

// App 应用门面。
type App struct {
	Harness *agent.Harness
	opts    Options
}

// New 构建应用。
func New(opts Options) (*App, error) {
	client := opts.Client
	settings, _ := config.LoadFileForUpdate()
	if client == nil {
		eff, err := config.Load()
		if err != nil {
			return nil, err
		}
		client = llm.NewClient(llm.Config{BaseURL: eff.BaseURL, APIKey: eff.APIKey, Model: eff.Model})
	}
	applyPolicyOverrides(settings, opts)
	// 提示词优先级：Options.SystemPrompt > 配置文件 system_prompt（gh
	// config prompt set 落盘）> 内置默认。
	sysPrompt := opts.SystemPrompt
	if sysPrompt == "" && settings != nil {
		sysPrompt = settings.SystemPrompt
	}
	h, err := agent.Build(agent.Options{
		Workspace:     opts.Workspace,
		Client:        client,
		Settings:      settings,
		Host:          opts.Host,
		MaxSteps:      opts.MaxSteps,
		SystemPrompt:  sysPrompt,
		DepthLimit:    opts.DepthLimit,
		DisabledTools: opts.DisabledTools,
		Skills:        opts.Skills,
	})
	if err != nil {
		return nil, err
	}
	if opts.VerboseEvents != nil {
		h.Root.SetOnEvent(opts.VerboseEvents)
	}
	if opts.OnToolEvent != nil {
		h.Root.SetOnToolEvent(opts.OnToolEvent)
	}
	return &App{Harness: h, opts: opts}, nil
}

// applyPolicyOverrides 把 Options 的运行策略覆盖到 settings（仅内存，
// 不落盘）。
func applyPolicyOverrides(s *config.Settings, opts Options) {
	if s == nil {
		return
	}
	if opts.Policy != "" {
		s.Policy = config.NormalizePolicy(opts.Policy)
	}
	if opts.AutoApprove != nil {
		s.AutoApprove = *opts.AutoApprove
	}
}

// Root 根 agent。
func (a *App) Root() *agent.Agent { return a.Harness.Root }

// Close 清理资源。
func (a *App) Close() { a.Harness.Close() }

// SaveSession 保存会话（结构化消息 JSON + system 原文 + 缓存键），返回路径。
func (a *App) SaveSession() (string, error) {
	a.Harness.Store.SetCacheKey(a.Harness.CacheKey())
	return a.Harness.Store.SaveSession(a.Root().History())
}

// AutoSave 回合结束后自动保存当前会话（空会话不落盘，静默失败）。
func (a *App) AutoSave() {
	if len(a.Root().History()) <= 1 { // 只有 system，视为空会话
		return
	}
	_, _ = a.SaveSession()
}

// ExecBang 直接执行用户键入的 shell 命令（"!cmd" 输入）：走已注册的
// bash 工具（沿用工作区/沙箱/超时策略），不经模型、不进会话历史。
func (a *App) ExecBang(ctx context.Context, cmd string) tools.Result {
	args, _ := json.Marshal(map[string]string{"command": cmd})
	return a.Root().Reg().Execute(ctx, "bash", string(args))
}

// BangOutput 清理直执行输出：去掉面向模型的 exit code 标注（"!cmd"
// 展示给用户看原始输出）。
func BangOutput(text string) string {
	out := strings.TrimRight(text, "\n")
	if i := strings.LastIndex(out, "\n[exit code:"); i >= 0 {
		out = out[:i]
	}
	if strings.HasPrefix(out, "(no output) [exit code:") {
		out = ""
	}
	return strings.TrimRight(out, "\n")
}

// Restore 切新会话并原样回放历史会话（优先回放文件里存的 system 原文
// 与缓存键，保证请求前缀与上一场逐字一致；当前会话若有内容先自动存档，
// 续接的会话另存为新文件）。
func (a *App) Restore(sess store.Session) {
	a.AutoSave()
	a.Root().Reset()
	a.Root().RestoreFull(sess.Messages, sess.System)
	if sess.CacheKey != "" {
		a.Harness.SetCacheKey(sess.CacheKey)
	}
	a.Harness.Store.ResetCurrent()
	a.Harness.Goals.DisarmAfterResume()
}

// NewSession 保存当前会话记录并开启全新会话（/new、/clear 共用）。
func (a *App) NewSession() {
	a.AutoSave()
	a.Root().Reset()
	a.Harness.Store.ResetCurrent()
	a.Harness.Goals.DisarmAfterResume()
}

// LatestSession 最近一次会话的完整恢复材料。
func (a *App) LatestSession() (store.Session, error) { return a.Harness.Store.LatestSessionFull() }

// SwitchModel 切换会话模型并换 client（仅当前会话生效，不写回配置；
// 默认模型只能经 gh model 子命令修改）。
func (a *App) SwitchModel(provider, model string) error {
	s, err := config.LoadFileForUpdate()
	if err != nil {
		return fmt.Errorf("读取配置: %w", err)
	}
	p, ok := s.Providers[provider]
	if !ok {
		return fmt.Errorf("提供商 %q 不存在，可用: %v", provider, s.SortedProviderNames())
	}
	if len(p.Models) == 0 {
		return fmt.Errorf("提供商 %q 未配置任何模型", provider)
	}
	if model == "" {
		model = p.Models[0]
	} else if !slices.Contains(p.Models, model) {
		return fmt.Errorf("模型 %q 未配置（%s 可用: %v）", model, provider, p.Models)
	}
	nc := llm.NewClient(llm.Config{BaseURL: p.BaseURL, APIKey: p.APIKey, Model: model})
	nc.SetCacheKey(a.Harness.CacheKey()) // 缓存路由键随会话迁移到新 client
	a.Root().SetClient(nc)
	return nil
}

// SetHost 运行期替换根 agent 的宿主（TUI 启动后接管交互）。
func (a *App) SetHost(h tools.Host) {
	a.Root().SetHost(h)
	a.Harness.Fs.Host = h
	a.opts.Host = h
}
