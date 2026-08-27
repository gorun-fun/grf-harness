// Package app 应用组装层：作为库嵌入与 CLI 共用的入口。提供
// App（根 harness 的门面）、单任务直出渲染（RunTask）、非 TTY 回退
// REPL，以及交互宿主（TaskHost）实现。
package app

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"git.gorun.fun/share/grf-harness/agent"
	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/llm"
	"git.gorun.fun/share/grf-harness/store"
	"git.gorun.fun/share/grf-harness/term"
	"git.gorun.fun/share/grf-harness/tools"
)

// Version 版本号。
const Version = "0.0.1-alpha.2"

// Options 应用构建参数（库使用方入口）。
type Options struct {
	Workspace     string                    // 工作区；默认 cwd
	Client        *llm.Client               // 指定客户端；nil 时从配置加载
	Host          tools.Host                // 交互宿主；nil 时按交互性自动选择
	MaxSteps      int                       // 循环上限；0 用配置/默认
	SystemPrompt  string                    // 自定义静态提示词；空用配置文件/内置
	DepthLimit    int                       // 子代理深度上限
	DisabledTools []string                  // 按场景禁用的工具名
	VerboseEvents func(kind, detail string) // 全部事件回调（挂到根 agent；RunTask 会覆盖，见其注释）
	OnToolEvent   func(agent.ToolEvent)     // 结构化工具事件回调（挂到根 agent，子代理事件也回流；见 docs/integration.md §5）

	// 运行策略覆盖：仅本次构建生效、不写回配置文件；空/nil 沿用
	// ~/.grf/settings.json（嵌入方可用它摆脱对机器全局配置的依赖）。
	Sandbox     string // bash 文件沙箱："off" | "workspace-write"
	FsPolicy    string // 写前先读策略："observe" | "off"
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
	if opts.Sandbox != "" {
		s.Sandbox = opts.Sandbox
	}
	if opts.FsPolicy != "" {
		s.FsPolicy = opts.FsPolicy
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
	} else if !containsStr(p.Models, model) {
		return fmt.Errorf("模型 %q 未配置（%s 可用: %v）", model, provider, p.Models)
	}
	nc := llm.NewClient(llm.Config{BaseURL: p.BaseURL, APIKey: p.APIKey, Model: model})
	nc.SetCacheKey(a.Harness.CacheKey()) // 缓存路由键随会话迁移到新 client
	a.Root().SetClient(nc)
	return nil
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ---- 直写 stdout 的流式渲染（单任务 / 非 TTY REPL 共用） ----

// stdoutUI 流式直写：思考进度行（\r 覆写）+ 正文原文增量输出 + 事件行。
// 不做 markdown 渲染——按原始文本输出（需求 5）。
type stdoutUI struct {
	tty    bool
	w      io.Writer
	start  time.Time
	chars  int
	active bool
	mdOpen bool // 正文已开始输出（事件前需换行）
}

func (s *stdoutUI) reasoning(delta string) {
	if !s.active {
		s.active = true
		s.start = time.Now()
	}
	s.chars += len([]rune(delta))
	if !s.tty {
		return
	}
	fmt.Fprintf(s.w, "\r\033[2K\033[2m💭 思考中… %ds · %d 字\033[0m", int(time.Since(s.start).Seconds()), s.chars)
}

func (s *stdoutUI) content(delta string) {
	s.fold()
	if !s.mdOpen {
		s.mdOpen = true
	}
	fmt.Fprint(s.w, delta)
}

func (s *stdoutUI) fold() {
	if !s.active {
		return
	}
	s.active = false
	summary := fmt.Sprintf("💭 已深度思考 %ds，%d 字", int(time.Since(s.start).Seconds()), s.chars)
	if s.tty {
		fmt.Fprintf(s.w, "\r\033[2K\033[2m%s\033[0m\n", summary)
	} else {
		fmt.Fprintln(s.w, summary)
	}
}

func (s *stdoutUI) printEvent(kind, detail string) {
	s.fold()
	if s.mdOpen {
		fmt.Fprintln(s.w)
		s.mdOpen = false
	}
	prefix := "🔧"
	switch kind {
	case "tool":
		prefix = "[tool]"
	case "result":
		prefix = "[->]  "
	case "todos":
		prefix = "[todo]"
	case "goal":
		prefix = "[goal]"
	case "phase":
		prefix = "[flow]"
	case "notice":
		prefix = "[info]"
	}
	if s.tty {
		fmt.Fprintf(s.w, "\033[2m%s %s\033[0m\n", prefix, detail)
	} else {
		fmt.Fprintf(s.w, "%s %s\n", prefix, detail)
	}
}

// attach 挂到 agent。
func (s *stdoutUI) attach(ag *agent.Agent) {
	ag.SetStreamHooks(s.content, s.reasoning)
	ag.SetOnEvent(func(kind, detail string) {
		switch kind {
		case "tool", "result", "todos", "goal", "phase", "notice":
			s.printEvent(kind, detail)
		case "done":
			s.fold()
			if s.mdOpen {
				fmt.Fprintln(s.w)
				s.mdOpen = false
			}
		}
	})
}

// RunTask 单次任务模式：执行任务、流式直出到 stdout、保存会话。
// 注意：内部会为根 agent 挂上自己的 stdout 渲染（覆盖之前设置的
// OnEvent/StreamHooks 回调）——需要自定义回调的嵌入方直接用
// Root().Run / RunAutoGoal 驱动，不要经过本方法。
func (a *App) RunTask(ctx context.Context, task string) error {
	ui := &stdoutUI{tty: term.IsTTY(int(os.Stdout.Fd())), w: os.Stdout}
	ui.attach(a.Root())
	host := a.opts.Host
	if host == nil {
		host = NewTaskHost(a.Harness.Settings.AutoApprove)
		a.Root().SetHost(host)
	}
	_, err := a.Root().RunAutoGoal(ctx, task)
	if err != nil {
		return err
	}
	// 后台子代理收尾：短暂等待并吸收通知
	if notices := a.Root().RunAndWaitBackground(10 * time.Second); len(notices) > 0 {
		for _, n := range notices {
			ui.printEvent("notice", firstLineOf(n))
		}
		if !ui.mdOpen {
			fmt.Fprintln(os.Stdout)
		}
		_, err = a.Root().RunAutoGoal(ctx, "Collect the settled background results above and give the user your final consolidated answer now.")
		if err != nil {
			return err
		}
	}
	a.Harness.Artifacts.Cleanup()
	if _, serr := a.SaveSession(); serr != nil {
		fmt.Fprintf(os.Stderr, "保存会话失败: %v\n", serr)
	}
	return nil
}

func firstLineOf(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// ---- 交互宿主（任务模式）----

// TaskHost stdin/stdout 交互宿主：提问打印到终端、从 stdin 读一行作答；
// 沙箱升级/计划审批按 AutoApprove（或 TTY 下 y/n 询问）。
//
// 用 NewTaskHost 构造（自动探测 stdout 是否 TTY）。直接构造零值时视为
// 非 TTY：AskUser 在 stdin 可交互时仍可用，三类审批一律按 AutoApprove
// 处理（false 即静默拒绝）。
type TaskHost struct {
	AutoApprove bool
	ttyOut      bool
}

// NewTaskHost 构建 stdin/stdout 交互宿主，按当前 stdout 是否 TTY 决定
// 审批是否可交互询问。
func NewTaskHost(autoApprove bool) *TaskHost {
	return &TaskHost{AutoApprove: autoApprove, ttyOut: term.IsTTY(int(os.Stdout.Fd()))}
}

func (h *TaskHost) AskUser(ctx context.Context, qs []tools.Question) (map[string]any, error) {
	if !term.IsTTY(int(os.Stdin.Fd())) {
		return nil, fmt.Errorf("stdin is not interactive")
	}
	answers := map[string]any{}
	rd := bufio.NewReader(os.Stdin)
	for _, q := range qs {
		fmt.Println()
		if q.Header != "" {
			fmt.Printf("【%s】\n", q.Header)
		}
		fmt.Println(q.Question)
		if len(q.Options) == 0 {
			fmt.Print("> ")
			line, err := rd.ReadString('\n')
			if err != nil && line == "" {
				return nil, fmt.Errorf("user input ended")
			}
			answers[q.ID] = strings.TrimSpace(line)
			continue
		}
		for i, opt := range q.Options {
			desc := ""
			if opt.Description != "" {
				desc = " — " + opt.Description
			}
			fmt.Printf("  %d. %s%s\n", i+1, opt.Label, desc)
		}
		if q.MultiSelect {
			fmt.Print("多选（逗号分隔序号，可附自定义文字）> ")
		} else {
			fmt.Print("序号或自定义文字 > ")
		}
		line, err := rd.ReadString('\n')
		if err != nil && line == "" {
			return nil, fmt.Errorf("user input ended")
		}
		answers[q.ID] = optionAnswer(q, line)
	}
	return answers, nil
}

// optionAnswer 把选项题的一行输入解析成答案：合法序号映射为选项标签，
// 其余非空内容视作用户自定义文字（多选时两者可混用），空输入默认第一项。
func optionAnswer(q tools.Question, line string) any {
	if !q.MultiSelect {
		t := strings.TrimSpace(line)
		if n, err := strconv.Atoi(t); err == nil && n >= 1 && n <= len(q.Options) {
			return q.Options[n-1].Label
		}
		if t != "" {
			return t
		}
		return q.Options[0].Label
	}
	var labels []string
	hasNum := false
	for _, f := range strings.FieldsFunc(line, func(r rune) bool {
		return r == ',' || r == '，' || r == ' ' || r == '\n'
	}) {
		if n, err := strconv.Atoi(f); err == nil && n >= 1 && n <= len(q.Options) {
			labels = append(labels, q.Options[n-1].Label)
			hasNum = true
		} else if t := strings.TrimSpace(f); t != "" {
			labels = append(labels, t)
		}
	}
	if len(labels) == 0 {
		return []string{q.Options[0].Label}
	}
	if !hasNum {
		// 纯自定义回复：整行原样作一条答案，避免被空白拆散
		return []string{strings.TrimSpace(line)}
	}
	return labels
}

func (h *TaskHost) ApproveEscalation(ctx context.Context, tool, command, mode, justification string) bool {
	if h.AutoApprove || !h.ttyOut || !term.IsTTY(int(os.Stdin.Fd())) {
		return h.AutoApprove
	}
	fmt.Printf("\n⚠ 沙箱升级请求 [%s → %s]\n命令: %s\n理由: %s\n批准? [y/N] ", tool, mode, command, justification)
	rd := bufio.NewReader(os.Stdin)
	line, _ := rd.ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

func (h *TaskHost) ApproveTool(ctx context.Context, name, args string) tools.ApproveDecision {
	if h.AutoApprove || !h.ttyOut || !term.IsTTY(int(os.Stdin.Fd())) {
		return tools.ApproveDecision{Approved: h.AutoApprove}
	}
	fmt.Printf("\n⚠ 工具调用审批 [%s]\n参数: %s\n", name, args)
	fmt.Print("批准? [y/N]（其他输入作为反馈拒绝并回给模型）> ")
	rd := bufio.NewReader(os.Stdin)
	line, _ := rd.ReadString('\n')
	line = strings.TrimSpace(line)
	if strings.EqualFold(line, "y") || strings.EqualFold(line, "yes") {
		return tools.ApproveDecision{Approved: true}
	}
	if line == "" {
		return tools.ApproveDecision{}
	}
	return tools.ApproveDecision{Feedback: line}
}

func (h *TaskHost) ApprovePlan(ctx context.Context, plan string) (bool, string) {
	if h.AutoApprove || !h.ttyOut || !term.IsTTY(int(os.Stdin.Fd())) {
		if h.AutoApprove {
			return true, ""
		}
		return false, ""
	}
	fmt.Println("\n=== 计划审批 ===")
	fmt.Println(plan)
	fmt.Print("\n批准? [y/N]（其他输入作为反馈）> ")
	rd := bufio.NewReader(os.Stdin)
	line, _ := rd.ReadString('\n')
	line = strings.TrimSpace(line)
	if strings.EqualFold(line, "y") || strings.EqualFold(line, "yes") {
		return true, ""
	}
	if line == "" {
		return false, "（用户未批准且无反馈）"
	}
	return false, line
}

func (h *TaskHost) NotifyEvent(kind, detail string) {}

// SetHost 运行期替换根 agent 的宿主（TUI 启动后接管交互）。
func (a *App) SetHost(h tools.Host) {
	a.Root().SetHost(h)
	a.Harness.Fs.Host = h
	a.opts.Host = h
}
