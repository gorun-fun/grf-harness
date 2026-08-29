// Package agent 实现 harness 的核心 agent 循环：用户输入 → 模型（含工具
// 调用）→ 执行工具 → 结果回传 → 循环。支持全部 26 个内置工具（定义
// 嵌入于 tools 包 spec）、异步通知（后台子代理落定注入）、图像工具结果
// 的多模态回传、目标（goal）自动续跑与计划模式。
//
// 按功能线条分文件：turn.go（核心回合循环）与 pause.go（暂停/恢复协议）、
// run.go（Run* 入口族）、history.go（历史与回放）、notice.go（异步通知）、
// events.go（事件回调）。
package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"git.gorun.fun/share/grf-harness/llm"
	"git.gorun.fun/share/grf-harness/store"
	"git.gorun.fun/share/grf-harness/term"
	"git.gorun.fun/share/grf-harness/tools"
)

// DefaultMaxSteps 工具调用循环默认轮次上限。
const DefaultMaxSteps = 100

// Agent 单个 agent 实例（根会话或子代理共用同一结构）。
type Agent struct {
	ID    string
	Label string

	client *llm.Client
	reg    *tools.Registry
	st     *store.Store
	root   string
	system string

	mu      sync.Mutex
	history []llm.Message

	onEvent     OnEvent
	onToolEvent OnToolEvent
	onContent   func(string)
	onReason    func(string)
	maxSteps    int
	humanTurn   bool
	running     bool
	turnCancel  context.CancelFunc
	notices     []string
	planMode    bool
	approveEach bool
	planGuard   func() bool   // 子代理生效计划模式判定（指向根；根为 nil 时用自身）
	toolCalls   int           // 已执行的工具调用数
	toolTime    time.Duration // 工具执行累计耗时
	bgReminded  bool          // 已发过后台作业收割提醒（每会话至多一次）
	pausedSteps int           // 暂停时已消耗的工具循环步数（Resume 续用，见 pause.go）

	mgr        *Manager
	goals      *GoalState
	Todos      *tools.TodoState
	Fs         *tools.FsPolicy
	Jobs       *tools.JobManager
	host       tools.Host
	subThreads int // 并发子代理上限守卫

	// OnIdleNotice：agent 空闲时收到通知（后台子代理落定）——由上层
	//（TUI/app）注册以触发自动续跑；未注册时通知并入下次请求。
	OnIdleNotice func(text string)
	noticeCh     chan struct{}
	noticeOnce   sync.Once
}

// NewAgent 创建根 agent（工具链由 BuildRegistry 组装完成后注入）。
func NewAgent(id string, client *llm.Client, reg *tools.Registry, st *store.Store, root, system string) *Agent {
	a := &Agent{
		ID:        id,
		Label:     "main",
		client:    client,
		reg:       reg,
		st:        st,
		root:      root,
		system:    system,
		maxSteps:  DefaultMaxSteps,
		Jobs:      tools.NewJobManager(),
		noticeCh:  make(chan struct{}, 1),
		humanTurn: true,
	}
	a.history = []llm.Message{{Role: "system", Content: system}}
	return a
}

// ---- 基础访问 ----

func (a *Agent) Model() string { return a.client.Model() }

func (a *Agent) SetClient(c *llm.Client) { a.client = c }

func (a *Agent) SetMaxSteps(n int) {
	if n > 0 {
		a.mu.Lock()
		a.maxSteps = n
		a.mu.Unlock()
	}
}

func (a *Agent) MaxSteps() int { a.mu.Lock(); defer a.mu.Unlock(); return a.maxSteps }

func (a *Agent) SetOnEvent(fn OnEvent) { a.onEvent = fn }

// SetOnToolEvent 注册结构化工具事件回调。根 agent 注册即可覆盖整棵
// 子代理树（子代理未单独注册时事件回流到根）。
func (a *Agent) SetOnToolEvent(fn OnToolEvent) { a.onToolEvent = fn }

// Reg 工具注册表访问器（嵌入方替换 Handler / 运行期 Disable 用）。
func (a *Agent) Reg() *tools.Registry { return a.reg }

// SetHost 运行期替换交互宿主（TUI 接管时）。
func (a *Agent) SetHost(h tools.Host) { a.host = h }

func (a *Agent) SetStreamHooks(content, reasoning func(string)) {
	a.onContent, a.onReason = content, reasoning
}

// SystemText 当前 system prompt。
func (a *Agent) SystemText() string { return a.system }

// InPlanMode / 计划模式。
func (a *Agent) InPlanMode() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.planMode }

// inPlan 生效的计划模式：子代理跟随根（写操作拦截用，见 turn.go）。
func (a *Agent) inPlan() bool {
	if a.planGuard != nil {
		return a.planGuard()
	}
	return a.InPlanMode()
}

// SetPlanMode 切换计划模式（用户主动操作，或模型经 enter_plan_mode 申请、用户批准）。
func (a *Agent) SetPlanMode(on bool) {
	a.mu.Lock()
	a.planMode = on
	a.mu.Unlock()
	a.emit("plan", fmt.Sprintf("%v", on))
}

// ApproveEach / SetApproveMode 审批模式：每次工具调用前经 Host.ApproveTool
// 征求用户同意（仅根会话有交互宿主；子代理不启用）。
func (a *Agent) ApproveEach() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.approveEach
}

func (a *Agent) SetApproveMode(on bool) {
	a.mu.Lock()
	a.approveEach = on
	a.mu.Unlock()
}

// LLMStats 根客户端累计用量统计（UI 底栏展示）。
func (a *Agent) LLMStats() llm.StatsSnapshot { return a.client.Stats() }

// ToolStats 已执行工具调用数与累计耗时。
func (a *Agent) ToolStats() (calls int, d time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.toolCalls, a.toolTime
}

// IsRunning 当前是否有 Run 进行中。
func (a *Agent) IsRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running
}

// firstLine 取首个非空行（trim 后、120 rune 截断），用于事件摘要；空文本
// 返回占位符（截断与选取逻辑复用 term.FirstLine）。
func firstLine(s string) string {
	if t := term.FirstLine(s, 120); t != "" {
		return t
	}
	return "(empty)"
}
