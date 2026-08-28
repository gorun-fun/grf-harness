// manager.go 子代理树：subagent（自包含任务）/ subagent_fork（继承已
// 完成轮次的分叉）默认后台执行并立即返回持久 id，send_message 续聊，
// interrupt_agent 取消当前回合，list_agents 列 direct children 或整棵
// 后代树。子代理落定时向父会话注入通知（父空闲则触发自动续跑）。
package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"git.gorun.fun/share/grf-harness/llm"
	"git.gorun.fun/share/grf-harness/tools"
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
	h, err := m.register(parent, label, nil)
	if err != nil {
		return "", "", err
	}
	if bg {
		go m.runTurn(h, prompt)
		return h.ID, "", nil
	}
	out, rerr := m.runOne(h.Ag, prompt)
	return h.ID, out, rerr
}

// Fork 启动一个继承父会话已完成轮次的子代理（不含父当前进行中的回合）。
func (m *Manager) Fork(parent *Agent, label, prompt string, bg bool) (string, string, error) {
	seed := m.completedTurns(parent)
	h, err := m.register(parent, label, seed)
	if err != nil {
		return "", "", err
	}
	if bg {
		go m.runTurn(h, prompt)
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

// runTurn 子代理回合：结束（含错误/取消）后落定并通知父会话。
func (m *Manager) runTurn(h *Handle, prompt string) {
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
	return err != nil && (err == context.Canceled || strings.Contains(err.Error(), "context canceled"))
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

// Interrupt 取消某 agent 当前回合（排队消息保留）。
func (m *Manager) Interrupt(caller *Agent, targetID string) error {
	m.mu.Lock()
	h, ok := m.agents[targetID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("unknown agent id %q", targetID)
	}
	// 必须是 caller 的后代
	depth := 0
	cur := h
	for cur != nil && cur.Parent != caller.ID && cur.Parent != "" {
		nh := m.agents[cur.Parent]
		if nh == nil {
			break
		}
		cur = nh
		depth++
		if depth > 16 {
			break
		}
	}
	isDesc := ok && cur != nil && cur.Parent == caller.ID
	rootSelf := targetID == caller.ID
	m.mu.Unlock()
	if !isDesc && !rootSelf {
		return fmt.Errorf("agent %q is not your descendant; you may only interrupt your direct child or deeper agents under you", targetID)
	}
	ag := m.Find(targetID)
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
	m.mu.Unlock()
	if !isChild {
		return fmt.Errorf("subagent %q is not your direct child (send_message works on depth-1 children only; use list_agents to recall them)", targetID)
	}
	ag := m.Find(targetID)
	if ag == nil {
		return fmt.Errorf("subagent %q not found", targetID)
	}
	if ag.IsRunning() {
		ag.EnqueueNotice("deferred user instruction (deliver after the current turn finishes): " + message)
		return nil
	}
	go m.runTurn(h, message)
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
		// 向上找根是否经过 caller
		cur := h
		for cur != nil {
			if cur.Parent == caller.ID {
				return true
			}
			nh := m.agents[cur.Parent]
			if nh == nil {
				break
			}
			cur = nh
		}
	}
	return false
}

// WaitForSettled 等待 caller 的全部子代理落定（最多 wait）。
func (m *Manager) WaitForSettled(caller *Agent, wait time.Duration) {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if !m.hasRunningDescendants(caller) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// RegisterAgentTools 注册 subagent / subagent_fork / send_message /
// interrupt_agent / list_agents 五个工具。
func RegisterAgentTools(r *tools.Registry, a *Agent, m *Manager) {
	sub := func(fork bool) tools.Handler {
		return func(ctx context.Context, args string) (tools.Result, error) {
			var p struct {
				Description     string `json:"description"`
				Prompt          string `json:"prompt"`
				RunInBackground *bool  `json:"run_in_background"`
			}
			if err := tools.DecodeJSON(args, &p); err != nil {
				return tools.Result{}, err
			}
			if strings.TrimSpace(p.Prompt) == "" {
				return tools.Result{}, fmt.Errorf("prompt is required (complete and self-contained — the subagent does not see this conversation)")
			}
			if strings.TrimSpace(p.Description) == "" {
				return tools.Result{}, fmt.Errorf("description is required (3-5 words, for display)")
			}
			bg := true
			if p.RunInBackground != nil {
				bg = *p.RunInBackground
			}
			var (
				id, out string
				err     error
			)
			if fork {
				id, out, err = m.Fork(a, oneLine(p.Description, 60), p.Prompt, bg)
			} else {
				id, out, err = m.Subagent(a, oneLine(p.Description, 60), p.Prompt, bg)
			}
			if err != nil {
				return tools.Result{}, err
			}
			if bg {
				return tools.TextResult(fmt.Sprintf("subagent started in the background: id=%s label=%q\nYou will be notified when it settles; recall continuable subagents with list_agents and give follow-up work via send_message.", id, p.Description)), nil
			}
			return tools.TextResult(fmt.Sprintf("subagent %s (%s) result:\n\n%s", id, p.Description, out)), nil
		}
	}
	r.Register("subagent", sub(false))
	r.Register("subagent_fork", sub(true))

	r.Register("send_message", func(ctx context.Context, args string) (tools.Result, error) {
		var p struct {
			SubagentID string `json:"subagent_id"`
			Message    string `json:"message"`
		}
		if err := tools.DecodeJSON(args, &p); err != nil {
			return tools.Result{}, err
		}
		if strings.TrimSpace(p.Message) == "" {
			return tools.Result{}, fmt.Errorf("message is required")
		}
		if err := m.SendMessage(a, p.SubagentID, p.Message); err != nil {
			return tools.Result{}, fmt.Errorf("message NOT delivered: %v", err)
		}
		return tools.TextResult(fmt.Sprintf("message delivered to %s; it becomes the subagent's next turn (queued if it is still working). No answer comes back through this call — the settle notice will carry its outcome.", p.SubagentID)), nil
	})

	r.Register("interrupt_agent", func(ctx context.Context, args string) (tools.Result, error) {
		var p struct {
			AgentID string `json:"agent_id"`
		}
		if err := tools.DecodeJSON(args, &p); err != nil {
			return tools.Result{}, err
		}
		if err := m.Interrupt(a, p.AgentID); err != nil {
			return tools.Result{}, err
		}
		return tools.TextResult(fmt.Sprintf("stop request accepted for %s; only its current turn stops — queued messages stay parked and the agent stays available", p.AgentID)), nil
	})

	r.Register("list_agents", func(ctx context.Context, args string) (tools.Result, error) {
		var p struct {
			Scope string `json:"scope"`
		}
		if err := tools.DecodeJSON(args, &p); err != nil {
			return tools.Result{}, err
		}
		if p.Scope == "" {
			p.Scope = "children"
		}
		if p.Scope != "children" && p.Scope != "descendants" {
			return tools.Result{}, fmt.Errorf("scope must be children or descendants")
		}
		return tools.TextResult(m.ListAgents(a, p.Scope)), nil
	})
}
