// router.go 任务路由层（指挥官侧）：subagent 派发经 pick() 命中远端节点
// 时走 ORS 客户端（后台轮询回收、暂停应答直达本地 Host），否则调用方
// 回退本地 Manager。远端句柄与本地子代理共用 send_message /
// interrupt_agent / list_agents 三个工具入口（handler 先查本表）。
package cluster

import (
	"bytes"
	"context"
	"encoding/base64"
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
	inbox *inboxHub   // 指令收件箱（M3b：NAT 节点的中继下行通道）

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
		inbox:   newInboxHub(),
		poll:    pollInterval,
		handles: map[string]*remoteAgent{},
	}
}

// Share 共享对象存储（server 端点接收侧 / 派发链解包侧共用）。
func (r *Router) Share() *ShareStore { return r.share }

// SyncMode 生效的同步档位（settings cluster.sync 归一化）：
// ""/seed/git/full。非空即文件传输通道开启。
func (r *Router) SyncMode() string {
	if r.cfg == nil {
		return ""
	}
	return SyncMode(r.cfg.Sync)
}

// SyncEnabled M2 文件传输门禁（任一同步档位开启）。
func (r *Router) SyncEnabled() bool {
	return r.SyncMode() != ""
}

// Enabled 路由层是否可介入（静态节点表或成员表任一非空——call-home/
// mDNS 发现的节点也可路由，经中继）。
func (r *Router) Enabled() bool {
	if r.cfg == nil {
		return false
	}
	return len(r.cfg.Nodes) > 0 || len(r.table.Snapshot()) > 0
}

// InboxConnected 节点是否挂在中继指令通道上（状态面板/派发前置判定）。
func (r *Router) InboxConnected(node string) bool { return r.inbox.connected(node) }

// InboxConnectedNodes 中继在线节点名列表。
func (r *Router) InboxConnectedNodes() []string { return r.inbox.connectedNodes() }

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

// SetSyncModeForTest 覆盖同步档位（测试用；生产档位经 settings 只读）。
func (r *Router) SetSyncModeForTest(mode string) {
	if r.cfg != nil {
		r.cfg.Sync = mode
	}
}

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

// routeTarget 一次路由决策的结果：直连（static，持有该节点 server token）
// 或中继（call-home/mDNS 成员，无 static 条目或无 addr——指令经收件箱
// 下行、结果经 /cluster/events 回投）。
type routeTarget struct {
	static *config.ClusterNode
	member *Member
}

// name 目标节点路由名。
func (t *routeTarget) name() string {
	if t.static != nil {
		return NodeName(*t.static)
	}
	return t.member.Name
}

// Dispatch 派发远端子代理任务。未命中节点返回 ErrNoRoute（调用方回退
// 本地）；命中但节点满载/创建失败返回错误。前台 ctx 为父回合取消链
//（取消即取消远端并返回 context.Canceled）。SeedFiles 非空、或同步档位
// 为 git/full（未显式点名）时打包种子下发——直连节点走对象推送，中继
// 节点内联 base64（上限低于对象通道，超限报错引导走直连）。
func (r *Router) Dispatch(ctx context.Context, req DispatchReq) (id, out string, err error) {
	target := r.pickTarget(req.Description)
	if target == nil {
		return "", "", ErrNoRoute
	}
	name := target.name()
	if n := r.inflight(name); n >= r.cfg.RouteMaxInflight() {
		return "", "", fmt.Errorf("cluster: node %s is at max inflight tasks (%d); route to another node or retry later", name, r.cfg.RouteMaxInflight())
	}
	policy := req.Policy
	if policy == "" {
		policy = r.cfg.Policy
	}
	hostCtx := r.hostContext()
	// 种子打包（显式点名或 git/full 自动档）
	seedFiles, seedErr := r.resolveSeed(req)
	if seedErr != nil {
		return "", "", seedErr
	}
	if target.member != nil || (target.static != nil && target.static.Addr == "") {
		return r.dispatchRelay(ctx, req, target, policy, hostCtx, seedFiles)
	}
	client := NewNodeClient(target.static.Addr, target.static.Token)
	if target.static.Pin != "" {
		if err := client.SetPinnedTLS(target.static.Pin); err != nil {
			return "", "", err
		}
	}
	seedRef := ""
	if len(seedFiles) > 0 {
		if req.Fs == nil {
			return "", "", fmt.Errorf("cluster seed: filesystem policy is required for seed_files")
		}
		var buf bytes.Buffer
		files, err := PackPaths(req.Fs, req.Fs.Workspace, seedFiles, &buf)
		if err != nil {
			return "", "", fmt.Errorf("cluster seed pack: %w", err)
		}
		_ = files
		ack, err := client.PushObject(ctx, &buf)
		if err != nil {
			return "", "", fmt.Errorf("cluster seed push to %s: %w", name, err)
		}
		seedRef = ack.Ref
	}
	h := newRemoteAgent(name, client, policy, hostCtx, oneLine(req.Description, 60), req.Host, req.Notify, req.Artifacts)
	resp, err := h.client.CreateTask(ctx, TaskOpts{
		Input: req.Prompt, Policy: policy, HostCtx: h.hostCtx,
		SeedRef: seedRef, ReturnFiles: req.ReturnFiles,
	})
	if err != nil {
		return "", "", fmt.Errorf("cluster dispatch to %s: %w", name, err)
	}
	h.setRespID(resp.ID)
	r.register(h)
	return r.await(ctx, h, req.Background)
}

// resolveSeed 本次派发的种子清单：显式 SeedFiles 优先；否则按同步档位
// 自动取（git = 跟踪文件，full = 全工作区）。文件传输关闭时显式点名报错
// 引导、自动档静默跳过（档位关闭 = 任务自包含契约）。
func (r *Router) resolveSeed(req DispatchReq) ([]string, error) {
	if len(req.SeedFiles) > 0 {
		if !r.SyncEnabled() {
			return nil, fmt.Errorf(`cluster: file transfer is disabled on this commander (set settings cluster.sync = "seed"/"git"/"full" to enable); send a self-contained prompt instead`)
		}
		return req.SeedFiles, nil
	}
	switch r.SyncMode() {
	case "git", "full":
		if req.Fs == nil || req.Fs.Workspace == "" {
			return nil, nil
		}
		ws := req.Fs.Workspace
		var (
			files []string
			err   error
		)
		if r.SyncMode() == "git" {
			files, err = GitTrackedFiles(ws)
		} else {
			files, err = AllWorkspaceFiles(ws)
		}
		if err != nil {
			return nil, fmt.Errorf("cluster sync (%s): %w", r.SyncMode(), err)
		}
		return files, nil
	}
	return nil, nil
}

// inlineSeedMax 中继内联种子上限（压缩后 base64 前的字节数）：指令 JSON
// 单帧不宜过大；更大的种子需要直连通道（对象推送）。
const inlineSeedMax = 4 << 20

// dispatchRelay 中继派发（NAT 节点）：指令入收件箱，事件回投驱动状态机。
// 无直连 client——runRelaySegment 等待 /cluster/events 的权威终态。
func (r *Router) dispatchRelay(ctx context.Context, req DispatchReq, target *routeTarget, policy, hostCtx string, seedFiles []string) (id, out string, err error) {
	name := target.name()
	if !r.inbox.connected(name) {
		return "", "", fmt.Errorf("cluster: node %s is not connected to the command inbox (relay dispatch needs the node's call-home inbox stream); route to a directly reachable node or wait for it to reconnect", name)
	}
	h := newRemoteAgent(name, nil, policy, hostCtx, oneLine(req.Description, 60), req.Host, req.Notify, req.Artifacts)
	h.relay = true
	h.taskID = h.id
	seedB64 := ""
	if len(seedFiles) > 0 {
		if req.Fs == nil {
			return "", "", fmt.Errorf("cluster seed: filesystem policy is required for seed_files")
		}
		var buf bytes.Buffer
		if _, err := PackPaths(req.Fs, req.Fs.Workspace, seedFiles, &buf); err != nil {
			return "", "", fmt.Errorf("cluster seed pack: %w", err)
		}
		if buf.Len() > inlineSeedMax {
			return "", "", fmt.Errorf("cluster seed: relay payload %d bytes exceeds the %d bytes inline limit; dispatch to a directly reachable node for large seeds", buf.Len(), inlineSeedMax)
		}
		seedB64 = base64.StdEncoding.EncodeToString(buf.Bytes())
	}
	if _, err := r.inbox.enqueue(name, InboxCommand{
		ID: newHandleID(), Type: InboxTask, TaskID: h.taskID,
		Input: req.Prompt, Policy: policy, HostCtx: hostCtx,
		SeedB64: seedB64, SeedFiles: seedFiles, ReturnFiles: req.ReturnFiles,
	}); err != nil {
		return "", "", fmt.Errorf("cluster relay dispatch to %s: %w", name, err)
	}
	r.register(h)
	return r.await(ctx, h, req.Background)
}

// await 注册后的公共收尾：后台起监管 goroutine，前台同步等终态。
func (r *Router) await(ctx context.Context, h *remoteAgent, background bool) (id, out string, err error) {
	if background {
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
		return h.id, "", fmt.Errorf("cluster: remote task on %s %s", h.node, outcome)
	}
	return h.id, final, nil
}

// supervise 后台监管循环：跑一个远端回合 → 落定 → 排队的 send_message
// 合并为新回合续跑 → 全部落定发通知（与本地 runAndSettle/settle 语义
// 对齐：运行中投递排队、落定后通知）。中继句柄的续跑走收件箱指令。
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
		if err := r.startFollowUp(h, msg); err != nil {
			h.setRunning(false)
			if h.notify != nil {
				h.notify(h.id, h.label, "failed: deliver follow-up message: "+err.Error(), "")
			}
			return
		}
	}
}

// startFollowUp 续跑一个新回合（send_message 排队消息 / 空闲投递）：
// 直连走 ORS 续聊，中继走收件箱 continue_of 指令。
func (r *Router) startFollowUp(h *remoteAgent, msg string) error {
	if h.relay {
		_, err := r.inbox.enqueue(h.node, InboxCommand{
			ID: newHandleID(), Type: InboxTask, TaskID: h.taskID,
			Input: msg, ContinueOf: h.respID(), Policy: h.policy, HostCtx: h.hostCtx,
		})
		return err
	}
	resp, err := h.client.CreateTask(context.Background(), TaskOpts{
		Input: msg, PrevID: h.respID(), Policy: h.policy, HostCtx: h.hostCtx,
	})
	if err != nil {
		return err
	}
	h.setRespID(resp.ID)
	return nil
}

// runSegment 跑一个远端回合（CreateTask/Resume 已完成）：轮询到终态。
// 暂停（grf:input_required）经 buildResumeItems 直达本地 Host 应答后续
// 跑；ctx 取消即取消远端并返回 interrupted。网络错误容忍 maxErrStreak
// 个周期；404 蒸发（节点重启杀回合）立即失败。节点侧落定回投
//（/cluster/events）到达即跳过等待立即复查（轮询降级为兜底）。中继
// 句柄无轮询通道，事件即权威终态（runRelaySegment）。
func (r *Router) runSegment(ctx context.Context, h *remoteAgent) (outcome, final string) {
	if h.relay {
		return r.runRelaySegment(ctx, h)
	}
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
					if err := r.submitResume(ctx, h, d.Requests); err != nil {
						return "failed: submit interaction answers: " + err.Error(), ""
					}
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
		case <-h.settle:
			// 节点回投到达：立即复查（下一轮循环的 GET 即终态快照）
		case <-time.After(r.poll):
		}
	}
}

// submitResume 暂停应答提交：直连走 ORS Resume，中继走收件箱 resume 指令
//（节点按 continue_of 链在暂停会话上续跑）。
func (r *Router) submitResume(ctx context.Context, h *remoteAgent, reqs []IncompleteRequest) error {
	items := buildResumeItems(reqs, h.host)
	if h.relay {
		_, err := r.inbox.enqueue(h.node, InboxCommand{
			ID: newHandleID(), Type: InboxResume, TaskID: h.taskID,
			ContinueOf: h.respID(), Items: items,
		})
		return err
	}
	rres, rerr := h.client.Resume(ctx, h.respID(), items)
	if rerr != nil {
		return rerr
	}
	h.setRespID(rres.ID)
	return nil
}

// runRelaySegment 中继回合执行：等待 /cluster/events 的权威终态（task_id
// 匹配）。暂停应答同样直达本地 Host 后经收件箱续跑；成员掉线（心跳/
// 发现 TTL 过期）超过容忍窗口即判失败——中继无轮询通道，这是唯一的
// 失联收敛路径。事件里带 resp 时顺带推进链尾（后续续跑用）。
func (r *Router) runRelaySegment(ctx context.Context, h *remoteAgent) (outcome, final string) {
	// 清残留事件：上一回合/重复回投的迟到事件不属于本回合（task_id 跨
	// 回合不变，靠时序区分——回合边界清空是唯一可靠判据）
	for {
		select {
		case <-h.settle:
			continue
		default:
		}
		break
	}
	offlineStreak := 0
	for {
		select {
		case <-ctx.Done():
			r.relayCancel(h)
			return "interrupted", ""
		case ev := <-h.settle:
			if ev.ResponseID != "" {
				h.setRespID(ev.ResponseID)
			}
			switch ev.Status {
			case "completed":
				if len(ev.Artifacts) > 0 && h.artifacts != nil {
					h.artifacts(h.id, ev.Artifacts)
				}
				return "completed", ev.FinalText
			case "failed":
				msg := ev.Error
				if msg == "" {
					msg = "model failure"
				}
				return "failed: " + msg, ""
			case "cancelled":
				return "interrupted", ""
			case "incomplete":
				if ev.Reason == "grf:input_required" && len(ev.Requests) > 0 {
					if err := r.submitResume(ctx, h, ev.Requests); err != nil {
						return "failed: submit interaction answers: " + err.Error(), ""
					}
					continue
				}
				return "incomplete (max tool steps on the node)", ""
			case "in_progress":
				continue // 节点起了回合：继续等终态
			default:
				continue
			}
		case <-time.After(r.poll):
			// 失联收敛：成员表 offline/蒸发累计 maxErrStreak 个周期即败
			m := r.table.Get(h.node)
			if m != nil && m.Status == StatusOnline {
				offlineStreak = 0
				continue
			}
			offlineStreak++
			if offlineStreak >= maxErrStreak {
				return "failed: relay node " + h.node + " went offline before reporting the task outcome", ""
			}
		}
	}
}

// relayCancel 中继取消（尽力而为：节点在即收；失联时回投自然不会再来，
// 由失联收敛兜底）。
func (r *Router) relayCancel(h *remoteAgent) {
	_, _ = r.inbox.enqueue(h.node, InboxCommand{ID: newHandleID(), Type: InboxCancel, TaskID: h.taskID})
}

// NotifySettle 节点落定回投入口（server 的 /cluster/events 端点薄委托）：
// task_id 精确匹配中继句柄；response_id 匹配直连句柄（提前唤醒轮询）。
// 未知事件忽略（200 回执——重复/迟到的事件不该让节点重试）。
func (r *Router) NotifySettle(ev InboxEvent) {
	if ev.Type != "turn.settled" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, h := range r.handles {
		if h.relay {
			if ev.TaskID != "" && ev.TaskID == h.taskID {
				h.deliverSettle(ev)
				return
			}
			continue
		}
		if ev.ResponseID != "" && ev.ResponseID == h.respID() {
			h.deliverSettle(ev)
			return
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

// remoteAgent 一个远端子代理句柄。relay=true 时无直连 client（NAT 中继）：
// 指令经收件箱下行，/cluster/events 回投经 settle 通道驱动状态机。
type remoteAgent struct {
	id, label, node string
	client          *NodeClient // 中继句柄为 nil
	policy, hostCtx string
	host            func() tools.Host
	notify          func(id, label, outcome, final string)
	artifacts       func(id string, arts []ArtifactMeta)

	relay  bool
	taskID string // 中继任务号（回投事件匹配键；直连句柄空）

	settle chan InboxEvent // 落定事件（缓冲 1：新事件替换旧事件）

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
		settle: make(chan InboxEvent, 1),
	}
}

// deliverSettle 投递一条落定事件（替换式：先清旧再放新，绝不久等——
// 调用方持 Router.mu，阻塞会锁死整个句柄表）。
func (h *remoteAgent) deliverSettle(ev InboxEvent) {
	select {
	case <-h.settle:
	default:
	}
	select {
	case h.settle <- ev:
	default:
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
// 语义对齐（返回 nil 即已投递/排队）。中继句柄走收件箱续跑指令。
func (r *Router) SendMessage(id, message string) error {
	h := r.lookup(id)
	if h == nil {
		return fmt.Errorf("unknown remote agent id %q", id)
	}
	if h.isRunning() {
		h.pushPending(message)
		return nil
	}
	if err := r.startFollowUp(h, message); err != nil {
		return fmt.Errorf("cluster send to %s: %w", h.node, err)
	}
	h.setRunning(true)
	go r.supervise(h)
	return nil
}

// Interrupt 取消远端子代理当前回合（幂等；排队消息保留、句柄保留）。
// 中继句柄经收件箱 cancel 指令（尽力而为，失联由收敛路径兜底）。
func (r *Router) Interrupt(id string) error {
	h := r.lookup(id)
	if h == nil {
		return fmt.Errorf("unknown remote agent id %q", id)
	}
	if h.relay {
		r.relayCancel(h)
		return nil
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
		kind := ""
		if h.relay {
			kind = " relay=1"
		}
		rows = append(rows, fmt.Sprintf("%s  node=%s status=%s%s label=%q", h.id, h.node, status, kind, h.label))
	}
	sort.Strings(rows)
	return strings.Join(rows, "\n")
}

// InboxSubscribe 节点接入收件箱（server 的 /cluster/inbox SSE 端点薄委托；
// 返回指令流与注销函数）。
func (r *Router) InboxSubscribe(node string) (<-chan InboxCommand, func()) {
	return r.inbox.subscribe(node)
}

// HandleStatus 状态快照的远端句柄条目（/v1/agent/cluster/status 数据源）。
type HandleStatus struct {
	ID     string `json:"id"`
	Node   string `json:"node"`
	Status string `json:"status"` // running | idle
	Relay  bool   `json:"relay,omitempty"`
	Label  string `json:"label"`
}

// HandlesSnapshot 远端句柄状态快照（id 字典序）。
func (r *Router) HandlesSnapshot() []HandleStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]HandleStatus, 0, len(r.handles))
	for _, h := range r.handles {
		status := "idle"
		if h.isRunning() {
			status = "running"
		}
		out = append(out, HandleStatus{ID: h.id, Node: h.node, Status: status, Relay: h.relay, Label: h.label})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// PromptLines 系统提示词"集群节点"段的节点行：静态表与成员表合并渲染
//（实时状态/能力可见——在线状态、负载、远端模型数/插件/平台），模型据
// 此按"谁在线/谁有 GPU 或浏览器插件"路由。中继可达（收件箱在连）的
// call-home 节点无静态 token 也能派发，行内标注。文件传输档位开启时
// 段尾附 seed_files/return_files 用法（git/full 自动档注明免点名）。
func (r *Router) PromptLines() []string {
	if r.cfg == nil {
		return nil
	}
	members := r.table.Snapshot()
	byName := make(map[string]Member, len(members))
	for _, m := range members {
		byName[m.Name] = m
	}
	lines := make([]string, 0, len(r.cfg.Nodes)+len(members)+1)
	for _, n := range r.cfg.Nodes {
		name := NodeName(n)
		m, hasBeat := byName[name]
		delete(byName, name) // 已渲染，registered 副本不再重复
		line := fmt.Sprintf("- %s (addr=%s", name, n.Addr)
		if len(n.Labels) > 0 {
			line += ", labels=" + strings.Join(n.Labels, "/")
		}
		if hasBeat {
			line += ", status=" + m.Status
			if m.Running > 0 {
				line += fmt.Sprintf(", running=%d", m.Running)
			}
			line += capsLine(m.Caps)
		} else {
			line += ", status=unknown (no heartbeat yet)"
		}
		line += ")"
		lines = append(lines, line)
	}
	// 无静态条目的成员（call-home / mDNS 发现）：可见 + 中继可派发标注
	for _, m := range byName {
		line := fmt.Sprintf("- %s (discovered via %s", m.Name, m.Source)
		if m.Addr != "" {
			line += ", addr=" + m.Addr
		}
		if len(m.Labels) > 0 {
			line += ", labels=" + strings.Join(m.Labels, "/")
		}
		line += ", status=" + m.Status
		if m.Running > 0 {
			line += fmt.Sprintf(", running=%d", m.Running)
		}
		line += capsLine(m.Caps)
		if r.inbox.connected(m.Name) {
			line += ", dispatchable via the command inbox relay"
		} else {
			line += ", not dispatchable yet (no static token entry and not connected to the inbox relay)"
		}
		line += ")"
		lines = append(lines, line)
	}
	switch r.SyncMode() {
	case "seed":
		lines = append(lines, "- file transfer is enabled on cluster dispatches: pass seed_files (workspace-relative files/dirs) to copy this machine's files onto the node before the task runs, and return_files (globs on the node's session workspace) to receive result files back — they unpack into cluster-artifacts/<subagent-id>/ in this workspace")
	case "git", "full":
		lines = append(lines, fmt.Sprintf("- file transfer is on auto-pilot (%s mode): every cluster dispatch automatically copies this workspace's %s to the node's session workspace — write prompts as if the node already has the files; return_files still brings result files back into cluster-artifacts/<subagent-id>/", r.SyncMode(), map[bool]string{true: "git-tracked files", false: "entire workspace"}[r.SyncMode() == "git"]))
	}
	return lines
}

// capsLine 能力摘要（提示词一行内）：模型数/插件/平台。caps 为 nil 返回空。
func capsLine(c *Capabilities) string {
	if c == nil {
		return ""
	}
	var parts []string
	if len(c.Models) > 0 {
		parts = append(parts, fmt.Sprintf("models=%d", len(c.Models)))
	}
	for _, p := range c.Plugins {
		parts = append(parts, "plugin="+p.Name)
	}
	if c.Platform != "" {
		parts = append(parts, "platform="+c.Platform)
	}
	if len(parts) == 0 {
		return ""
	}
	return ", " + strings.Join(parts, ",")
}

// pickTarget 路由决策：description 中 "@name" 精确命中（第一轮，显式意图
// 优先）→ labels 整词命中（第二轮，大小写不敏感）。静态节点（有 token，
// 直连）优先于成员表中继。nil = 不路由。
func (r *Router) pickTarget(description string) *routeTarget {
	if r.cfg == nil {
		return nil
	}
	tokens := tokenize(description)
	// 第一轮：@name——静态节点，再成员表（中继）
	for i := range r.cfg.Nodes {
		n := &r.cfg.Nodes[i]
		if tokens["@"+strings.ToLower(NodeName(*n))] {
			if n.Addr != "" {
				return &routeTarget{static: n}
			}
			// 显式配了名但无 addr：若该名有成员则中继，否则不路由
			if m := r.table.Get(NodeName(*n)); m != nil {
				return &routeTarget{member: m}
			}
			return nil
		}
	}
	for _, m := range r.table.Snapshot() {
		if tokens["@"+strings.ToLower(m.Name)] {
			if idx := r.staticIndex(m.Name); idx >= 0 {
				return &routeTarget{static: &r.cfg.Nodes[idx]} // 同名静态条目：直连优先
			}
			return &routeTarget{member: &m}
		}
	}
	// 第二轮：labels 整词
	for i := range r.cfg.Nodes {
		n := &r.cfg.Nodes[i]
		for _, l := range n.Labels {
			if tokens[strings.ToLower(l)] {
				if n.Addr != "" {
					return &routeTarget{static: n}
				}
				if m := r.table.Get(NodeName(*n)); m != nil {
					return &routeTarget{member: m}
				}
				return nil
			}
		}
	}
	for _, m := range r.table.Snapshot() {
		for _, l := range m.Labels {
			if tokens[strings.ToLower(l)] {
				if idx := r.staticIndex(m.Name); idx >= 0 {
					return &routeTarget{static: &r.cfg.Nodes[idx]}
				}
				return &routeTarget{member: &m}
			}
		}
	}
	return nil
}

// staticIndex 静态节点表里该路由名的下标（无 = -1）。
func (r *Router) staticIndex(name string) int {
	for i := range r.cfg.Nodes {
		if NodeName(r.cfg.Nodes[i]) == name {
			return i
		}
	}
	return -1
}

// ShouldRoute description 是否命中远端节点（handler 分流判定）。
func (r *Router) ShouldRoute(description string) bool {
	return r.pickTarget(description) != nil
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
