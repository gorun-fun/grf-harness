// router.go 任务路由层（指挥官侧）：subagent 派发经 pick() 命中远端节点
// 时走 ORS 客户端（后台轮询回收、暂停应答直达本地 Host），否则调用方
// 回退本地 Manager。远端句柄与本地子代理共用 send_message /
// interrupt_agent / list_agents 三个工具入口（handler 先查本表）。
package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/term"
	"git.gorun.fun/share/grf-harness/tools"
)

// ErrNoRoute description 未命中任何节点（调用方回退本地子代理）。
var ErrNoRoute = errors.New("cluster: no matching node for this description")

// pollInterval 默认轮询间隔（后台任务终态回收）。
const pollInterval = 2 * time.Second

// maxErrStreak 连续轮询失败上限（网络抖动容忍窗口 ≈ 15×间隔）。
const maxErrStreak = 15

// Router 任务路由层。进程级共享（一个 gh 实例一个），远端句柄表跨会话
// 可见——send_message 只会来自创建它的会话（模型上下文内），共享无碍。
type Router struct {
	cfg   *config.Cluster
	table *Table
	share *ShareStore // 共享对象存储（M2 文件传输；进程级单实例）

	selfOnce sync.Once
	self     Identity

	poll time.Duration // 轮询间隔（测试缩短）

	mu      sync.Mutex
	seq     int
	handles map[string]*remoteAgent
}

// NewRouter 构建路由层（成员表以静态节点表播种；共享存储随路由层持有）。
func NewRouter(cfg *config.Cluster) *Router {
	return &Router{
		cfg:     cfg,
		table:   NewTable(cfg.Nodes),
		share:   NewShareStore(),
		poll:    pollInterval,
		handles: map[string]*remoteAgent{},
	}
}

// Share 共享对象存储（server 端点接收侧 / 派发链解包侧共用）。
func (r *Router) Share() *ShareStore { return r.share }

// SyncEnabled M2 文件传输门禁（settings cluster.sync == "seed"）。
func (r *Router) SyncEnabled() bool {
	return r.cfg != nil && strings.EqualFold(strings.TrimSpace(r.cfg.Sync), "seed")
}

// Enabled 路由层是否可介入（存在可路由节点表）。
func (r *Router) Enabled() bool { return r.cfg != nil && len(r.cfg.Nodes) > 0 }

// AddNode 追加静态节点（运行期注入：测试 / goder 设置页通道；仅本进程
// 生效不落盘，落盘走 settings 的读写链路）。
func (r *Router) AddNode(n config.ClusterNode) {
	if r.cfg == nil {
		return
	}
	r.cfg.Nodes = append(r.cfg.Nodes, n)
}

// Table 成员表（指挥官权威；注册/心跳端点驱动）。
func (r *Router) Table() *Table { return r.table }

// Self 本节点身份（懒加载，cluster.json）。
func (r *Router) Self() Identity {
	r.selfOnce.Do(func() { r.self, _ = LoadIdentity() })
	return r.self
}

// SetPollInterval 覆盖轮询间隔（测试用）。
func (r *Router) SetPollInterval(d time.Duration) { r.poll = d }

// DefaultPolicy 派发任务的默认策略（settings cluster.policy）。
func (r *Router) DefaultPolicy() string {
	if r.cfg == nil {
		return ""
	}
	return r.cfg.Policy
}

// DispatchReq 一次远端派发的请求与回调。
type DispatchReq struct {
	Description string // 模型给的子代理描述（路由依据 + 显示标签）
	Prompt      string // 自包含任务提示词（远端 agent 只见它）
	Policy      string // 任务策略覆盖；空用 Router 默认
	Background  bool   // 后台（返 id 即刻，落定经 Notify）；前台同步等终态
	// M2 文件传输：种子下发（SeedFiles 为指挥官工作区相对路径，经 Fs
	// 读边界校验后打包推到命中节点；需 settings cluster.sync = "seed"）
	// 与产物回传声明（ReturnFiles 相对远端会话工作区的 glob，节点在
	// 回合落定后打包推回指挥官共享区）。两者均只在路由命中时生效。
	SeedFiles   []string
	ReturnFiles []string
	Fs          *tools.FsPolicy
	// Host 暂停应答直达：远端 grf:input_required 时经它征答（指挥官
	// 本地交互宿主）。可为 nil（全部 Dismiss，远端 agent 自行假设继续）。
	Host func() tools.Host
	// Notify 后台落定通知（agent 侧接 EnqueueNotice；仅后台派发使用）。
	Notify func(id, label, outcome, final string)
	// Artifacts 产物回传回调：runSegment 终态读到 grf_artifacts 时调用
	//（前台/后台均触发；对象已在指挥官本地共享区，回调侧负责解包）。
	Artifacts func(id string, arts []ArtifactMeta)
}

// hostContext 远端任务的宿主环境简报（grf:session.host_context）：告知
// 节点 agent 任务来自集群指挥官（而非本地用户直接会话）。
func (r *Router) hostContext() string {
	return fmt.Sprintf("cluster task dispatched by the commander session on %q; this subagent runs on this node with its local filesystem and tools", r.Self().Name)
}

// Dispatch 派发远端子代理任务。未命中节点返回 ErrNoRoute（调用方回退
// 本地）；命中但节点满载/创建失败返回错误。前台 ctx 为父回合取消链
// （取消即取消远端并返回 context.Canceled）。SeedFiles 非空时先打包推送
// 种子到命中节点（失败即派发失败——远端没有种子跑也是白跑）。
func (r *Router) Dispatch(ctx context.Context, req DispatchReq) (id, out string, err error) {
	node := r.pick(req.Description)
	if node == nil {
		return "", "", ErrNoRoute
	}
	name := NodeName(*node)
	if n := r.inflight(name); n >= r.cfg.RouteMaxInflight() {
		return "", "", fmt.Errorf("cluster: node %s is at max inflight tasks (%d); route to another node or retry later", name, r.cfg.RouteMaxInflight())
	}
	policy := req.Policy
	if policy == "" {
		policy = r.cfg.Policy
	}
	client := NewNodeClient(node.Addr, node.Token)
	seedRef := ""
	if len(req.SeedFiles) > 0 {
		if !r.SyncEnabled() {
			return "", "", fmt.Errorf(`cluster: file transfer is disabled on this commander (set settings cluster.sync = "seed" to enable); send a self-contained prompt instead`)
		}
		if req.Fs == nil {
			return "", "", fmt.Errorf("cluster seed: filesystem policy is required for seed_files")
		}
		var buf bytes.Buffer
		if _, err := PackPaths(req.Fs, req.Fs.Workspace, req.SeedFiles, &buf); err != nil {
			return "", "", fmt.Errorf("cluster seed pack: %w", err)
		}
		ack, err := client.PushObject(ctx, &buf)
		if err != nil {
			return "", "", fmt.Errorf("cluster seed push to %s: %w", name, err)
		}
		seedRef = ack.Ref
	}
	h := newRemoteAgent(name, client, policy, r.hostContext(), oneLine(req.Description, 60), req.Host, req.Notify, req.Artifacts)
	resp, err := h.client.CreateTask(ctx, TaskOpts{
		Input: req.Prompt, Policy: policy, HostCtx: h.hostCtx,
		SeedRef: seedRef, ReturnFiles: req.ReturnFiles,
	})
	if err != nil {
		return "", "", fmt.Errorf("cluster dispatch to %s: %w", name, err)
	}
	h.setRespID(resp.ID)
	r.register(h)
	if req.Background {
		h.setRunning(true)
		go r.supervise(h)
		return h.id, "", nil
	}
	h.setRunning(true)
	outcome, final := r.runSegment(ctx, h)
	h.setRunning(false)
	if ctx.Err() != nil {
		return h.id, "", context.Canceled
	}
	if outcome != "completed" {
		return h.id, "", fmt.Errorf("cluster: remote task on %s %s", name, outcome)
	}
	return h.id, final, nil
}

// supervise 后台监管循环：跑一个远端回合 → 落定 → 排队的 send_message
// 合并为新回合续跑 → 全部落定发通知（与本地 runAndSettle/settle 语义
// 对齐：运行中投递排队、落定后通知）。
func (r *Router) supervise(h *remoteAgent) {
	for {
		outcome, final := r.runSegment(context.Background(), h)
		msg := h.takePending()
		if msg == "" {
			h.setRunning(false)
			if h.notify != nil {
				h.notify(h.id, h.label, outcome, oneLine(final, 400))
			}
			return
		}
		resp, err := h.client.CreateTask(context.Background(), TaskOpts{
			Input: msg, PrevID: h.respID(), Policy: h.policy, HostCtx: h.hostCtx,
		})
		if err != nil {
			h.setRunning(false)
			if h.notify != nil {
				h.notify(h.id, h.label, "failed: deliver follow-up message: "+err.Error(), "")
			}
			return
		}
		h.setRespID(resp.ID)
	}
}

// runSegment 跑一个远端回合（CreateTask/Resume 已完成）：轮询到终态。
// 暂停（grf:input_required）经 buildResumeItems 直达本地 Host 应答后续
// 跑；ctx 取消即取消远端并返回 interrupted。网络错误容忍 maxErrStreak
// 个周期；404 蒸发（节点重启杀回合）立即失败。
func (r *Router) runSegment(ctx context.Context, h *remoteAgent) (outcome, final string) {
	errStreak := 0
	for {
		resp, err := h.client.GetResponse(ctx, h.respID())
		if err != nil {
			if strings.Contains(err.Error(), "404") {
				return "failed: response vanished (the node restarted while the task was running)", ""
			}
			errStreak++
			if errStreak >= maxErrStreak {
				return "failed: node unreachable: " + err.Error(), ""
			}
		} else {
			errStreak = 0
			switch resp.Status {
			case "completed":
				// 产物回传（M2）：清单随终态快照到达，对象已在本地共享区
				if len(resp.GrfArtifacts) > 0 && h.artifacts != nil {
					h.artifacts(h.id, resp.GrfArtifacts)
				}
				return "completed", resp.FinalText()
			case "failed":
				msg := "model failure"
				if resp.Error != nil && resp.Error.Message != "" {
					msg = resp.Error.Message
				}
				return "failed: " + msg, ""
			case "cancelled":
				return "interrupted", ""
			case "incomplete":
				if d := resp.IncompleteDetails; d != nil && d.Reason == "grf:input_required" && len(d.Requests) > 0 {
					items := buildResumeItems(d.Requests, h.host)
					rres, rerr := h.client.Resume(ctx, h.respID(), items)
					if rerr != nil {
						return "failed: submit interaction answers: " + rerr.Error(), ""
					}
					h.setRespID(rres.ID)
					continue
				}
				return "incomplete (max tool steps on the node)", ""
			}
			// queued / in_progress：继续轮询
		}
		select {
		case <-ctx.Done():
			cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = h.client.Cancel(cctx, h.respID())
			cancel()
			return "interrupted", ""
		case <-time.After(r.poll):
		}
	}
}

// buildResumeItems 暂停应答直达：远端待答请求逐项经本地 Host 征答。
// Host 缺失或征答失败一律 Dismiss（"调用方放弃，agent 自行假设继续"——
// 远端 agent 会陈述假设收尾）；审批被拒（Approved=false + Feedback）如实
// 回传，远端模型据此调整。问人事项的 fail-closed 由 Host 自身语义承担
// （TUI 超时/取消即错误 → 此处 Dismiss 收敛，不留悬挂回合）。
func buildResumeItems(reqs []IncompleteRequest, hostFn func() tools.Host) []InputResultItem {
	var host tools.Host
	if hostFn != nil {
		host = hostFn()
	}
	items := make([]InputResultItem, 0, len(reqs))
	for _, rq := range reqs {
		it := InputResultItem{Type: "grf:input_result", CallID: rq.CallID}
		switch rq.Kind {
		case "question":
			answered := false
			if host != nil {
				var payload struct {
					Questions []tools.Question `json:"questions"`
				}
				if err := json.Unmarshal([]byte(rq.Arguments), &payload); err == nil && len(payload.Questions) > 0 {
					if ans, err := host.AskUser(context.Background(), payload.Questions); err == nil && len(ans) > 0 {
						it.Answers = ans
						answered = true
					}
				}
			}
			if !answered {
				it.Dismiss = true
			}
		case "tool_approval":
			if host != nil {
				d := host.ApproveTool(context.Background(), rq.Name, rq.Arguments)
				it.Approved = d.Approved
				it.Feedback = d.Feedback
			} else {
				it.Dismiss = true
			}
		default:
			// function（指挥官未注册客户端工具）等未知类别：放弃
			it.Dismiss = true
		}
		items = append(items, it)
	}
	return items
}

// ---- 远端句柄表 ----

// remoteAgent 一个远端子代理句柄。
type remoteAgent struct {
	id, label, node string
	client          *NodeClient
	policy, hostCtx string
	host            func() tools.Host
	notify          func(id, label, outcome, final string)
	artifacts       func(id string, arts []ArtifactMeta)

	mu      sync.Mutex
	curResp string
	running bool
	pending []string // send_message 运行中排队；落定后合并为一轮输入
}

func newRemoteAgent(node string, client *NodeClient, policy, hostCtx, label string,
	host func() tools.Host, notify func(id, label, outcome, final string),
	artifacts func(id string, arts []ArtifactMeta)) *remoteAgent {
	return &remoteAgent{
		id: newHandleID(), label: label, node: node,
		client: client, policy: policy, hostCtx: hostCtx,
		host: host, notify: notify, artifacts: artifacts,
	}
}

func (h *remoteAgent) respID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.curResp
}

func (h *remoteAgent) setRespID(id string) {
	h.mu.Lock()
	h.curResp = id
	h.mu.Unlock()
}

func (h *remoteAgent) setRunning(v bool) {
	h.mu.Lock()
	h.running = v
	h.mu.Unlock()
}

func (h *remoteAgent) isRunning() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.running
}

func (h *remoteAgent) pushPending(msg string) {
	h.mu.Lock()
	h.pending = append(h.pending, msg)
	h.mu.Unlock()
}

// takePending 取走排队消息（合并为一段；空切片返回空串）。
func (h *remoteAgent) takePending() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.pending) == 0 {
		return ""
	}
	msg := strings.Join(h.pending, "\n\n")
	h.pending = nil
	return msg
}

var handleSeq int
var handleMu sync.Mutex

// newHandleID 远端句柄 id：ragent_ 前缀与本地 agent_N_ 空间显式区分。
func newHandleID() string {
	handleMu.Lock()
	handleSeq++
	n := handleSeq
	handleMu.Unlock()
	return fmt.Sprintf("ragent_%d_%x", n, time.Now().UnixNano()&0xffff)
}

func (r *Router) register(h *remoteAgent) {
	r.mu.Lock()
	r.handles[h.id] = h
	r.mu.Unlock()
}

func (r *Router) lookup(id string) *remoteAgent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.handles[id]
}

// Owns id 是否远端句柄（send_message / interrupt_agent 的 handler 据此分流）。
func (r *Router) Owns(id string) bool { return r.lookup(id) != nil }

// inflight 某节点在跑远端任务数（满载判定）。
func (r *Router) inflight(node string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, h := range r.handles {
		if h.node == node && h.isRunning() {
			n++
		}
	}
	return n
}

// SendMessage 向远端子代理投递消息：运行中排队（落定后自动作为新回合
// 输入，通知随之回流）；空闲立即起新回合。与本地 Manager.SendMessage
// 语义对齐（返回 nil 即已投递/排队）。
func (r *Router) SendMessage(id, message string) error {
	h := r.lookup(id)
	if h == nil {
		return fmt.Errorf("unknown remote agent id %q", id)
	}
	if h.isRunning() {
		h.pushPending(message)
		return nil
	}
	resp, err := h.client.CreateTask(context.Background(), TaskOpts{
		Input: message, PrevID: h.respID(), Policy: h.policy, HostCtx: h.hostCtx,
	})
	if err != nil {
		return fmt.Errorf("cluster send to %s: %w", h.node, err)
	}
	h.setRespID(resp.ID)
	h.setRunning(true)
	go r.supervise(h)
	return nil
}

// Interrupt 取消远端子代理当前回合（幂等；排队消息保留、句柄保留）。
func (r *Router) Interrupt(id string) error {
	h := r.lookup(id)
	if h == nil {
		return fmt.Errorf("unknown remote agent id %q", id)
	}
	cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.client.Cancel(cctx, h.respID()); err != nil {
		return fmt.Errorf("cluster interrupt %s: %w", id, err)
	}
	return nil
}

// ListText 远端句柄列表文本（list_agents 的追加段；与 Manager.ListAgents
// 行格式对齐）。无句柄返回空串。
func (r *Router) ListText() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.handles) == 0 {
		return ""
	}
	var rows []string
	for _, h := range r.handles {
		status := "idle"
		if h.isRunning() {
			status = "running"
		}
		rows = append(rows, fmt.Sprintf("%s  node=%s status=%s label=%q", h.id, h.node, status, h.label))
	}
	sort.Strings(rows)
	return strings.Join(rows, "\n")
}

// PromptLines 系统提示词"集群节点"段的节点行（静态表渲染）；文件传输
// （sync=seed）开启时在段尾追加 seed_files/return_files 用法提示行。
func (r *Router) PromptLines() []string {
	if r.cfg == nil {
		return nil
	}
	lines := make([]string, 0, len(r.cfg.Nodes)+1)
	for _, n := range r.cfg.Nodes {
		line := fmt.Sprintf("- %s (addr=%s", NodeName(n), n.Addr)
		if len(n.Labels) > 0 {
			line += ", labels=" + strings.Join(n.Labels, "/")
		}
		line += ")"
		lines = append(lines, line)
	}
	if r.SyncEnabled() {
		lines = append(lines, "- file transfer is enabled on cluster dispatches: pass seed_files (workspace-relative files/dirs) to copy this machine's files onto the node before the task runs, and return_files (globs on the node's session workspace) to receive result files back — they unpack into cluster-artifacts/<subagent-id>/ in this workspace")
	}
	return lines
}

// pick 路由决策：description 中 "@name" 精确命中（第一轮，显式意图
// 优先）→ labels 整词命中（第二轮，大小写不敏感）。nil = 不路由。
func (r *Router) pick(description string) *config.ClusterNode {
	if r.cfg == nil || len(r.cfg.Nodes) == 0 {
		return nil
	}
	tokens := tokenize(description)
	for i := range r.cfg.Nodes {
		n := &r.cfg.Nodes[i]
		if tokens["@"+strings.ToLower(NodeName(*n))] {
			return n
		}
	}
	for i := range r.cfg.Nodes {
		n := &r.cfg.Nodes[i]
		for _, l := range n.Labels {
			if tokens[strings.ToLower(l)] {
				return n
			}
		}
	}
	return nil
}

// ShouldRoute description 是否命中远端节点（handler 分流判定）。
func (r *Router) ShouldRoute(description string) bool {
	return r.pick(description) != nil
}

// tokenize 分词（小写、去尾部标点；@ 前缀保留）。
func tokenize(s string) map[string]bool {
	out := map[string]bool{}
	for _, f := range strings.Fields(s) {
		f = strings.Trim(f, ".,;:!?()[]{}\"'")
		if f != "" {
			out[strings.ToLower(f)] = true
		}
	}
	return out
}

// oneLine 与 agent 包同语义：压平多行 + rune 截断（通知摘要用）。
func oneLine(s string, n int) string {
	return term.ClipRunes(strings.TrimSpace(strings.ReplaceAll(s, "\n", " ")), n)
}
