// Package agent 实现 harness 的核心 agent 循环：用户输入 → 模型（含工具
// 调用）→ 执行工具 → 结果回传 → 循环。支持全部 25 个内置工具（定义
// 嵌入于 tools 包 spec）、异步通知（后台子代理落定注入）、图像工具结果
// 的多模态回传、目标（goal）自动续跑与计划模式。
package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"git.gorun.fun/share/grf-harness/llm"
	"git.gorun.fun/share/grf-harness/store"
	"git.gorun.fun/share/grf-harness/tools"
)

// DefaultMaxSteps 工具调用循环默认轮次上限。
const DefaultMaxSteps = 100

// OnEvent 过程事件回调（UI 展示）。kind：
//
//	tool    工具调用开始（detail: "name args摘要"）
//	result  工具结果首行
//	done    一次 Run 结束
//	notice  系统通知（子代理落定等）
//	todos   todo 列表变化（detail 为 JSON）
//	goal    goal 状态变化（detail 为摘要）
//	plan    计划模式变化
//	phase   workflow phase/log
type OnEvent func(kind, detail string)

// 工具事件状态（ToolEvent.Status）。
const (
	ToolStatusRunning = "running" // 开始执行（含审批前；等待用户审批时也处于此状态）
	ToolStatusDone    = "done"    // 执行成功
	ToolStatusError   = "error"   // 执行失败（Err 携带错误信息）
	ToolStatusDenied  = "denied"  // 审批未通过（Output 携带拒绝与反馈文本）
)

// ToolEvent 结构化工具事件：一次工具调用的完整生命周期，供嵌入方重建
// 工具时间线（进行中 → 完成/失败卡片、展开看全量输入输出）。同一
// CallID 依状态推进：running → done | error | denied。Input/Output 均为
// 全量原文不截断，展示裁剪由宿主自理。子代理的工具事件也会回流到根
// agent 的回调（用 AgentID/Label 区分来源）。
type ToolEvent struct {
	CallID  string // 工具调用 ID（LLM 返回的 tool_call id；正文回退解析生成；均为进程内唯一）
	Name    string
	Status  string // ToolStatusRunning / Done / Error / Denied
	Input   string // 完整参数 JSON
	Output  string // 完整结果文本（running 时为空）
	Err     string // Status=error 时的错误信息
	AgentID string // 发起调用的 agent（根为 agent_root）
	Label   string // agent 标签（根为 main，子代理为其 description）
}

// OnToolEvent 结构化工具事件回调。
type OnToolEvent func(ToolEvent)

// toolEventSeq 兜底 ID 计数：个别服务端不回 tool_call id 时进程内唯一。
var toolEventSeq atomic.Int64

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

func (a *Agent) emit(kind, detail string) {
	if a.onEvent != nil {
		a.onEvent(kind, detail)
	}
	if a.host != nil {
		a.host.NotifyEvent(kind, detail)
	}
}

// emitToolEvent 结构化工具事件：本 agent 未注册回调时，子代理的事件
// 回流到根 agent 的回调（时间线需要覆盖整棵 agent 树）。
func (a *Agent) emitToolEvent(e ToolEvent) {
	e.AgentID = a.ID
	e.Label = a.Label
	fn := a.onToolEvent
	if fn == nil && a.mgr != nil {
		if root := a.mgr.Root(); root != nil && root != a {
			fn = root.onToolEvent
		}
	}
	if fn != nil {
		fn(e)
	}
}

// SystemText 当前 system prompt。
func (a *Agent) SystemText() string { return a.system }

// History 历史快照（只读副本）。
func (a *Agent) History() []llm.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]llm.Message(nil), a.history...)
}

// Restore 原样回放上一场会话的消息（system 用当前构建期提示词）。
func (a *Agent) Restore(msgs []llm.Message) { a.RestoreFull(msgs, "") }

// RestoreFull 原样回放上一场会话的消息。systemText 非空时优先回放它
// （会话文件存的构建期提示词原文）——即使当前版本的提示词代码或运行时
// 事实已变化，请求前缀仍与上一场逐字一致，提示词缓存可命中；为空时
// 回退当前构建期提示词。
func (a *Agent) RestoreFull(msgs []llm.Message, systemText string) {
	if len(msgs) == 0 {
		return
	}
	if systemText == "" {
		systemText = a.system
	}
	h := make([]llm.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role != "system" {
			h = append(h, m)
		}
	}
	a.mu.Lock()
	a.system = systemText
	a.history = append([]llm.Message{{Role: "system", Content: systemText}}, h...)
	a.mu.Unlock()
}

// Reset 清空历史与 todo，回到全新会话（保留 system 提示词与配置）。
func (a *Agent) Reset() {
	a.mu.Lock()
	a.history = []llm.Message{{Role: "system", Content: a.system}}
	a.mu.Unlock()
	if a.Todos != nil {
		a.Todos.Items = nil
		if a.Todos.OnChange != nil {
			a.Todos.OnChange(nil)
		}
	}
}

// InPlanMode / 计划模式。
func (a *Agent) InPlanMode() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.planMode }

// SetPlanMode 切换计划模式（用户主动操作）。
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

// EnqueueNotice 注入一条系统通知；空闲时触发 OnIdleNotice 自动续跑。
func (a *Agent) EnqueueNotice(text string) {
	a.mu.Lock()
	a.notices = append(a.notices, text)
	idle := !a.running
	a.mu.Unlock()
	if idle && a.OnIdleNotice != nil {
		go a.OnIdleNotice(text)
	}
}

// drainNotices 取走积压通知。
func (a *Agent) drainNotices() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.notices) == 0 {
		return nil
	}
	out := a.notices
	a.notices = nil
	return out
}

// IsRunning 当前是否有 Run 进行中。
func (a *Agent) IsRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running
}

// ---- 核心循环 ----

// Run 处理一条用户输入，执行完整工具调用循环，返回最终回复。
func (a *Agent) Run(ctx context.Context, userInput string) (string, error) {
	return a.RunAs(ctx, userInput, a.humanTurn)
}

// RunWithImages 处理一条带图像的用户输入：user 消息按多模态 parts
// （文本 + data URL 图像）发送。无图像时退化为普通 Run。
func (a *Agent) RunWithImages(ctx context.Context, userInput string, images []tools.Image) (string, error) {
	if len(images) == 0 {
		return a.RunAs(ctx, userInput, a.humanTurn)
	}
	parts := []llm.Part{llm.TextPart(userInput)}
	for _, img := range images {
		parts = append(parts, llm.ImagePart(img.Mime, img.B64))
	}
	return a.runMessage(ctx, llm.Message{Role: "user", ContentParts: parts}, a.humanTurn)
}

// RunAs 以指定身份（human / 系统）执行一轮。续跑与通知触发的回合用
// 非人类身份（goal 工具对非人类 turn 有限制）。
func (a *Agent) RunAs(ctx context.Context, userInput string, human bool) (string, error) {
	return a.runMessage(ctx, llm.Message{Role: "user", Content: userInput}, human)
}

// runMessage 单轮核心循环：并入给定 user 消息后执行工具调用循环直至
// 产生最终回复。遇暂停点（client 工具 / deferred 交互，见 pause.go）返回
// *PausedError——Run* 系列不处理暂停，接入方应改用 RunTurn/Resume 驱动。
func (a *Agent) runMessage(ctx context.Context, userMsg llm.Message, human bool) (string, error) {
	s, paused, err := a.runTurn(ctx, &userMsg, human, nil)
	if err != nil {
		return "", err
	}
	if paused != nil {
		return "", &PausedError{Turn: paused}
	}
	return s, nil
}

// rollbackOrphanUser 本轮失败（LLM 出错/被取消）时移除尾部未被任何
// assistant 回复闭合的 user 输入，避免用户重发后出现连续两条重复 user
// 消息。只在本轮输入仍是最后一条消息时回滚（工具循环已推进则不动）。
func (a *Agent) rollbackOrphanUser(userMsg llm.Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := len(a.history)
	if n == 0 || a.history[n-1].Role != "user" {
		return
	}
	last := a.history[n-1]
	same := last.Content == userMsg.Content && len(last.ContentParts) == len(userMsg.ContentParts)
	if same {
		a.history = a.history[:n-1]
	}
}

// appendToolResult 把工具结果并入历史：纯文本 → tool 消息；带图像 →
// user 消息（多模态 parts，OpenAI tool 消息不支持图像内容）。
func (a *Agent) appendToolResult(call llm.ToolCall, res tools.Result) {
	if len(res.Images) == 0 {
		a.mu.Lock()
		a.history = append(a.history, llm.Message{
			Role: "tool", Content: res.Text, ToolCallID: call.ID, Name: call.Name(),
		})
		a.mu.Unlock()
		return
	}
	parts := []llm.Part{llm.TextPart(fmt.Sprintf("[tool result: %s] %s", call.Name(), res.Text))}
	for _, img := range res.Images {
		parts = append(parts, llm.ImagePart(img.Mime, img.B64))
	}
	a.mu.Lock()
	a.history = append(a.history, llm.Message{Role: "user", ContentParts: parts})
	a.mu.Unlock()
}

// hasBusyBackground 是否有仍在运行的后台作业或子代理。
func (a *Agent) hasBusyBackground() bool {
	if a.Jobs != nil && a.Jobs.ActiveCount() > 0 {
		return true
	}
	if a.mgr != nil {
		return a.mgr.hasRunningDescendants(a)
	}
	return false
}

// RunAndWaitBackground 等待全部后台子代理落定（最多 wait 上限）并返回
// 期间产生的通知（任务模式收尾用）。
func (a *Agent) RunAndWaitBackground(wait time.Duration) []string {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if !a.hasBusyBackground() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return a.drainNotices()
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			if len([]rune(t)) > 120 {
				r := []rune(t)
				t = string(r[:120]) + "…"
			}
			return t
		}
	}
	return "(empty)"
}
