// manager.go 子代理树：subagent（自包含任务）/ subagent_fork（继承已
// 完成轮次的分叉）默认后台执行并立即返回持久 id，send_message 续聊，
// interrupt_agent 取消当前回合，list_agents 列 direct children 或整棵
// 后代树。子代理落定时向父会话注入通知（父空闲则触发自动续跑）。
// 五个工具的注册在 manager_tools.go。
package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"git.gorun.fun/share/grf-harness/llm"
)

// Handle 子代理注册项。
type Handle struct {
	ID     string
	Label  string
	Parent string // 父 agent id
	Depth  int
	Ag     *Agent
}

// Manager agent 树（根 + 子代理）。并发安全。
type Manager struct {
	mu         sync.Mutex
	seq        int
	agents     map[string]*Handle
	root       *Agent
	DepthLimit int // 子代理树深度上限（根为 0）

	// NewChild 构建子代理（由 app 层注入：同 client、独立历史、受限工具链）。
	NewChild func(parent *Agent, label string, seed []llm.Message) *Agent
}

func NewManager(root *Agent, depthLimit int) *Manager {
	m := &Manager{agents: map[string]*Handle{}, root: root, DepthLimit: depthLimit}
	return m
}

// Root 根 agent。
func (m *Manager) Root() *Agent { return m.root }

func (m *Manager) register(parent *Agent, label string, seed []llm.Message) (*Handle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ph, ok := m.agents[parent.ID]
	depth := 0
	if parent != m.root {
		if !ok {
			return nil, fmt.Errorf("parent %s not registered", parent.ID)
		}
		depth = ph.Depth + 1
	}
	if depth > m.DepthLimit {
		return nil, fmt.Errorf("subagent depth limit reached (%d); delegate from a shallower agent instead", m.DepthLimit)
	}
	m.seq++
	h := &Handle{
		ID:     fmt.Sprintf("agent_%d_%s", m.seq, shortID()),
		Label:  label,
		Parent: parent.ID,
		Depth:  depth,
	}
	m.agents[h.ID] = h
	// Agent 构建延迟到锁外会破坏原子性——NewChild 只做内存构建，锁内可接受
	child := m.NewChild(parent, label, seed)
	child.ID = h.ID
	child.Label = label
	child.mgr = m
	h.Ag = child
	return h, nil
}

// Subagent 启动一个子代理。bg=true 后台（返回 id 即刻），否则前台等结果。
func (m *Manager) Subagent(parent *Agent, label, prompt string, bg bool) (string, string, error) {
	return m.start(parent, label, prompt, bg, nil)
}

// Fork 启动一个继承父会话已完成轮次的子代理（不含父当前进行中的回合）。
func (m *Manager) Fork(parent *Agent, label, prompt string, bg bool) (string, string, error) {
	return m.start(parent, label, prompt, bg, m.completedTurns(parent))
}

// start 注册并启动子代理的公共路径（Subagent/Fork 的差异只在 seed：
// 前者空历史，后者父已完成轮次副本）。bg=true 后台执行（返回 id 即刻，
// 落定后经 settle 通知父会话），否则前台等结果。
func (m *Manager) start(parent *Agent, label, prompt string, bg bool, seed []llm.Message) (string, string, error) {
	h, err := m.register(parent, label, seed)
	if err != nil {
		return "", "", err
	}
	if bg {
		go m.runAndSettle(h, prompt)
		return h.ID, "", nil
	}
	out, rerr := m.runOne(h.Ag, prompt)
	return h.ID, out, rerr
}

// completedTurns 父历史中去掉当前进行中回合的种子副本。
// 当前回合 = 自最后一个 user 消息（父发起本轮 Run 时追加的那条）起。
func (m *Manager) completedTurns(parent *Agent) []llm.Message {
	hist := parent.History()
	// 找最后一个非 system-notice 的真实 user 输入
	last := -1
	for i := len(hist) - 1; i >= 1; i-- {
		if hist[i].Role == "user" && !strings.HasPrefix(hist[i].Content, "[system notice]") {
			last = i
			break
		}
	}
	if last <= 0 {
		return nil
	}
	seed := append([]llm.Message(nil), hist[:last]...)
	return seed
}

// runAndSettle 子代理回合：结束（含错误/取消）后落定并通知父会话。
// （与 Agent.runTurn 刻意不同名——那是回合循环本体，这里是"跑完并
// 落定"的后台包装，仅包内使用。）
func (m *Manager) runAndSettle(h *Handle, prompt string) {
	out, err := m.runOne(h.Ag, prompt)
	m.settle(h, out, err)
}

func (m *Manager) runOne(ag *Agent, prompt string) (string, error) {
	ag.mu.Lock()
	ag.humanTurn = false
	ag.mu.Unlock()
	return ag.Run(context.Background(), prompt)
}

// settle 落定通知：后台子代理回合结束 → 父会话通知。
func (m *Manager) settle(h *Handle, out string, err error) {
	outcome := "completed"
	switch {
	case err == nil:
	case isCanceled(err):
		outcome = "interrupted"
	default:
		outcome = "failed: " + err.Error()
	}
	final := oneLine(out, 400)
	if final == "" {
		final = "(no final message)"
	}
	parent := m.Find(h.Parent)
	if parent != nil {
		parent.EnqueueNotice(fmt.Sprintf("your background subagent settled — id: %s, label: %s, outcome: %s\nfinal assistant message:\n%s\nUse send_message(subagent_id=%s, ...) to give it more work on the same conversation.", h.ID, h.Label, outcome, final, h.ID))
	}
}

func isCanceled(err error) bool {
	return errors.Is(err, context.Canceled)
}

// Find 按 id 查 agent。
func (m *Manager) Find(id string) *Agent {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id == m.root.ID {
		return m.root
	}
	if h, ok := m.agents[id]; ok {
		return h.Ag
	}
	return nil
}

// HandleOf 按 id 查注册项。
func (m *Manager) HandleOf(id string) (*Handle, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.agents[id]
	return h, ok
}

// isDescendant h 是否为 caller 的后代（含深层；调用方须持 m.mu）。
// 沿 Parent 链上溯：链条单向生长（注册只发生在已注册父上），终于根
// （Parent 为空串，根不在 agents 表里）——环不可能，无需深度兜底。
func (m *Manager) isDescendant(caller *Agent, h *Handle) bool {
	for cur := h; cur != nil; {
		if cur.Parent == caller.ID {
			return true
		}
		next, ok := m.agents[cur.Parent]
		if !ok {
			return false
		}
		cur = next
	}
	return false
}

// Interrupt 取消某 agent 当前回合（排队消息保留）。
func (m *Manager) Interrupt(caller *Agent, targetID string) error {
	m.mu.Lock()
	h, ok := m.agents[targetID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("unknown agent id %q", targetID)
	}
	// 只能打断 caller 自身或其后代
	if targetID != caller.ID && !m.isDescendant(caller, h) {
		m.mu.Unlock()
		return fmt.Errorf("agent %q is not your descendant; you may only interrupt your direct child or deeper agents under you", targetID)
	}
	ag := h.Ag // 锁内直接取 Agent 引用，解锁后不再经 Find 二次查找
	m.mu.Unlock()
	if ag == nil {
		return fmt.Errorf("agent %q not found", targetID)
	}
	ag.mu.Lock()
	cancel := ag.turnCancel
	ag.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// SendMessage 向直接子代理投递消息：运行中排队（当前回合结束后开始），
// 空闲则立即开始新回合。返回确认文本。
func (m *Manager) SendMessage(caller *Agent, targetID, message string) error {
	m.mu.Lock()
	h, ok := m.agents[targetID]
	isChild := ok && h.Parent == caller.ID
	var ag *Agent
	if isChild {
		ag = h.Ag // 锁内直接取 Agent 引用，解锁后不再经 Find 二次查找
	}
	m.mu.Unlock()
	if !isChild {
		return fmt.Errorf("subagent %q is not your direct child (send_message works on depth-1 children only; use list_agents to recall them)", targetID)
	}
	if ag == nil {
		return fmt.Errorf("subagent %q not found", targetID)
	}
	if ag.IsRunning() {
		ag.EnqueueNotice("deferred user instruction (deliver after the current turn finishes): " + message)
		return nil
	}
	go m.runAndSettle(h, message)
	return nil
}

// ListAgents 列表文本：scope=children 仅直接子代理，descendants 整棵树
// （前序遍历，带父 id 与深度标注）。
func (m *Manager) ListAgents(caller *Agent, scope string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var rows []string
	if scope == "descendants" {
		// 前序遍历 caller 的后代树
		var walk func(id string, depth int)
		walk = func(id string, depth int) {
			var children []*Handle
			for _, h := range m.agents {
				if h.Parent == id {
					children = append(children, h)
				}
			}
			sort.Slice(children, func(i, j int) bool { return children[i].ID < children[j].ID })
			for _, ch := range children {
				status := "idle"
				if ch.Ag != nil && ch.Ag.IsRunning() {
					status = "running"
				}
				rows = append(rows, fmt.Sprintf("%s  depth=%d parent=%s status=%s label=%q",
					ch.ID, ch.Depth, ch.Parent, status, ch.Label))
				walk(ch.ID, depth+1)
			}
		}
		walk(caller.ID, 0)
	} else {
		var children []*Handle
		for _, h := range m.agents {
			if h.Parent == caller.ID {
				children = append(children, h)
			}
		}
		sort.Slice(children, func(i, j int) bool { return children[i].ID < children[j].ID })
		for _, ch := range children {
			status := "idle"
			if ch.Ag != nil && ch.Ag.IsRunning() {
				status = "running"
			}
			rows = append(rows, fmt.Sprintf("%s  status=%s label=%q", ch.ID, status, ch.Label))
		}
	}
	if len(rows) == 0 {
		return "no continuable background subagents; start one with subagent or subagent_fork"
	}
	return strings.Join(rows, "\n") + "\n(send_message works on these entries; the snapshot is not a delivery promise — send_message performs the authoritative check)"
}

// hasRunningDescendants caller 树下是否有运行中的子代理。
func (m *Manager) hasRunningDescendants(caller *Agent) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, h := range m.agents {
		if h.Ag == nil || !h.Ag.IsRunning() {
			continue
		}
		if m.isDescendant(caller, h) {
			return true
		}
	}
	return false
}
