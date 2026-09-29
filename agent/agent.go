// Package agent 实现 harness 的核心 agent 循环：用户输入 → 模型（含工具
// 调用）→ 执行工具 → 结果回传 → 循环。支持全部 28 个内置工具（定义
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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"git.gorun.fun/share/grf-harness/llm"
	"git.gorun.fun/share/grf-harness/term"
	"git.gorun.fun/share/grf-harness/tools"
)

// DefaultMaxSteps 工具调用循环默认轮次上限。
const DefaultMaxSteps = 100

// DefaultMaxParallelTools 同批并行安全工具调用的默认并发上限
//（dsh maxParallelToolCalls 语义；1 为严格串行）。
const DefaultMaxParallelTools = 8

// SetMaxParallelTools 设置同批并行安全工具的并发上限（≥1）。
func (a *Agent) SetMaxParallelTools(n int) {
	if n >= 1 {
		a.mu.Lock()
		a.maxParallel = n
		a.mu.Unlock()
	}
}

// MaxParallelTools 当前并发上限（未设置回退默认）。
func (a *Agent) MaxParallelTools() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.maxParallel <= 0 {
		return DefaultMaxParallelTools
	}
	return a.maxParallel
}

// Agent 单个 agent 实例（根会话或子代理共用同一结构）。
type Agent struct {
	ID    string
	Label string

	client   *llm.Client
	clientMu sync.Mutex // client 引用的并发读写（运行中切换模型，见 SetClient）
	reg      *tools.Registry
	system   string

	mu      sync.Mutex
	history []llm.Message

	onEvent     OnEvent
	onToolEvent OnToolEvent
	onContent   func(string)
	onReason    func(string)
	onToolArgs  llm.ToolArgsHook
	maxSteps    int
	humanTurn   bool
	running     bool
	turnCancel  context.CancelFunc
	notices     []string
	planMode    bool
	approveEach bool
	planGuard   func() bool     // 子代理生效计划模式判定（指向根；根为 nil 时用自身）
	planExtra   map[string]bool // 计划模式额外拦截的工具名（未标只读的插件工具）
	toolCalls   int             // 已执行的工具调用数
	toolTime    time.Duration   // 工具执行累计耗时
	bgReminded  bool            // 已发过后台作业收割提醒（每会话至多一次；仅回合 goroutine 访问，不加锁）
	pausedSteps int             // 暂停时已消耗的工具循环步数（Resume 续用，见 pause.go）
	maxParallel int             // 同批并行安全工具的并发上限（dispatchBatch）
	repeatKey   string          // 连续相同调用（名+参数）的 streak 键（仅回合 goroutine 访问）
	repeatCount int             // 该键的连续次数（仅回合 goroutine 访问）

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
}

// NewAgent 创建根 agent（工具链由 BuildRegistry 组装完成后注入）。
func NewAgent(id string, client *llm.Client, reg *tools.Registry, system string) *Agent {
	a := &Agent{
		ID:          id,
		Label:       "main",
		client:      client,
		reg:         reg,
		system:      system,
		maxSteps:    DefaultMaxSteps,
		maxParallel: DefaultMaxParallelTools,
		Jobs:        tools.NewJobManager(),
		humanTurn:   true,
	}
	a.history = []llm.Message{{Role: "system", Content: system}}
	return a
}

// ---- 基础访问 ----

// getClient 取当前 client 引用（副本）。运行中切换模型（SwitchClient →
// SetClient）与回合循环/统计读取并发，引用经 clientMu 保护；取回副本后
// 的长调用（如 ChatStream）不持锁，落在旧 client 上的在途请求自然走完。
func (a *Agent) getClient() *llm.Client {
	a.clientMu.Lock()
	defer a.clientMu.Unlock()
	return a.client
}

func (a *Agent) Model() string { return a.getClient().Model() }

// ModelRef 模型全名 "provider/model"（server 回报 ORS 客户端用）。
func (a *Agent) ModelRef() string { return a.getClient().ModelRef() }

// SetClient 替换 client（并发安全；完整切换见 Harness.SwitchClient）。
func (a *Agent) SetClient(c *llm.Client) {
	a.clientMu.Lock()
	a.client = c
	a.clientMu.Unlock()
}

// SetReasoningEffort 设置思考强度（透传 llm 客户端 reasoning_effort；
// 换模型重建 client 后需重设，由调用方保证）。
func (a *Agent) SetReasoningEffort(e string) { a.getClient().SetReasoningEffort(e) }

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

// SetToolArgsHook 注册工具参数生成增量回调（与 SetStreamHooks 同级，根
// agent 生效；子代理不回流）。
func (a *Agent) SetToolArgsHook(fn llm.ToolArgsHook) { a.onToolArgs = fn }

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
func (a *Agent) LLMStats() llm.StatsSnapshot { return a.getClient().Stats() }

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

// oneLine 压平多行为单行并截断（事件摘要用；n 为 rune 上限，超出加
// 省略号）。
func oneLine(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// shortIDSeq shortID 的原子自增计数。
var shortIDSeq atomic.Int64

// shortID 展示用短 id（hex 计数）。ID 唯一性由 agent_%d_/goal_%d_ 的
// seq 前缀保证，这里只是装饰，纯自增计数即可（原时间戳混合运算无额外
// 价值）。
func shortID() string {
	return fmt.Sprintf("%x", shortIDSeq.Add(1))
}
