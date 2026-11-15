// inbox.go 指令收件箱（M3b）：指挥官→节点的 NAT 穿透下行通道。节点经
// SSE 长连（GET /cluster/inbox，cluster token 鉴权）挂在指挥官上，指挥官
// 把无法直连（节点无 addr/在 NAT 后）的任务指令经收件箱投递；指令执行
// 结果由节点经 /cluster/events 主动回投（上行恒可达——节点本就配了
// commander_url）。指令模型与 ORS 任务面同构：task（建任务/续聊）/
// resume（暂停应答）/cancel（取消）。指令在收件箱滞留超过 TTL 即被节点
// 丢弃（过期的任务不该再跑），连接断开重连后未消费指令继续投递。
package cluster

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 指令类型。
const (
	InboxTask   = "task"
	InboxResume = "resume"
	InboxCancel = "cancel"
)

// inboxTTL 指令有效期：投递后节点这么久没消费/没跑完即视为过期（防指挥官
// 排队的老任务在几小时后突然执行）。与共享区 TTL 同族量级。
const inboxTTL = 30 * time.Minute

// InboxCommand 指挥官→节点的一条指令。
type InboxCommand struct {
	ID     string `json:"id"` // 指令 id（去重/日志）
	Type   string `json:"type"`
	TaskID string `json:"task_id,omitempty"` // 指挥官侧远端句柄 id（task 首发与事件回投的关联键）
	Input  string `json:"input,omitempty"`   // task：本回合输入（自包含文本）
	// ContinueOf 上一回合在节点侧的 response id（续聊：send_message 语义；
	// 节点按 previous_response_id 链式续跑同一会话）。空 = 新会话。
	ContinueOf string   `json:"continue_of,omitempty"`
	Policy     string   `json:"policy,omitempty"`
	HostCtx    string   `json:"host_ctx,omitempty"`
	SeedB64    string   `json:"seed_b64,omitempty"` // 种子 tar.gz 的 base64（内联；中继无推送通道）
	// SeedFiles 种子归档清单（回执/诊断用，节点不依赖它解包）。
	SeedFiles   []string          `json:"seed_files,omitempty"`
	ReturnFiles []string          `json:"return_files,omitempty"`
	Items       []InputResultItem `json:"items,omitempty"` // resume：暂停应答 items
	TS          int64             `json:"ts"`             // unix 毫秒（过期判定）
}

// InboxEvent 节点→指挥官的回投事件（POST /cluster/events）。直连节点的
// 落定回投与中继节点的唯一结果通道共用一份形状：直连场景 response_id 匹配
// 唤醒（轮询照旧，事件只是提前量）；中继场景 task_id 匹配即为权威终态。
type InboxEvent struct {
	Type       string            `json:"type"` // "turn.settled"
	Node       string            `json:"node"`
	NodeID     string            `json:"node_id,omitempty"`
	TaskID     string            `json:"task_id,omitempty"`
	ResponseID string            `json:"response_id,omitempty"`
	Status     string            `json:"status"` // completed | failed | cancelled | incomplete | in_progress
	Reason     string            `json:"reason,omitempty"` // incomplete 原因（grf:input_required / max_tool_calls）
	Requests   []IncompleteRequest `json:"grf_requests,omitempty"` // 暂停待答请求
	FinalText  string            `json:"final_text,omitempty"`
	Error      string            `json:"error,omitempty"`
	Artifacts  []ArtifactMeta    `json:"grf_artifacts,omitempty"`
	TS         int64             `json:"ts"`
}

// inboxHub 指挥官侧收件箱：按节点名各一条队列 + 在线订阅者。并发安全。
type inboxHub struct {
	mu    sync.Mutex
	queue map[string][]InboxCommand
	subs  map[string]chan InboxCommand
}

func newInboxHub() *inboxHub {
	return &inboxHub{queue: map[string][]InboxCommand{}, subs: map[string]chan InboxCommand{}}
}

// 每节点待投递指令上限（防离线节点积压无界；满即拒——调用方报错比静默丢好）。
const inboxQueueCap = 64

// enqueue 向节点投递指令：在线走订阅通道（非阻塞——订阅通道缓冲即背压），
// 离线入队等待重连。返回节点当前是否在线（调用方据此决定是否接受"很快会被
// 执行"的语义）。
func (h *inboxHub) enqueue(node string, cmd InboxCommand) (online bool, err error) {
	if cmd.TS == 0 {
		cmd.TS = time.Now().UnixMilli()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if ch, ok := h.subs[node]; ok {
		select {
		case ch <- cmd:
			return true, nil
		default: // 订阅通道满：并入队列（节点在连但消费慢，如长任务卡读）
		}
	}
	if len(h.queue[node]) >= inboxQueueCap {
		return false, fmt.Errorf("cluster inbox: command queue for %q is full", node)
	}
	_, online = h.subs[node]
	h.queue[node] = append(h.queue[node], cmd)
	return online, nil
}

// connected 节点是否挂在中继通道上。
func (h *inboxHub) connected(node string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.subs[node]
	return ok
}

// connectedNodes 在线节点名列表（状态面板/诊断）。
func (h *inboxHub) connectedNodes() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.subs))
	for n := range h.subs {
		out = append(out, n)
	}
	return out
}

// subscribe 节点接入：建订阅通道并把积压队列移交进去（过期指令原地丢弃）。
// 返回订阅通道与注销函数；注销后未被消费的指令留在通道里由 GC 回收，
// 队列侧不再有副本（enqueue 只在无订阅者时入队）。
func (h *inboxHub) subscribe(node string) (<-chan InboxCommand, func()) {
	h.mu.Lock()
	ch := make(chan InboxCommand, 16)
	pending := h.queue[node]
	delete(h.queue, node)
	h.subs[node] = ch
	h.mu.Unlock()
	go func() {
		cutoff := time.Now().Add(-inboxTTL).UnixMilli()
		for _, cmd := range pending {
			if cmd.TS < cutoff {
				continue
			}
			select {
			case ch <- cmd:
			default: // 缓冲耗尽：放弃（极端积压，宁可丢老指令）
			}
		}
	}()
	return ch, func() {
		h.mu.Lock()
		if cur, ok := h.subs[node]; ok && cur == ch {
			delete(h.subs, node)
		}
		h.mu.Unlock()
	}
}

// InboxLoop 节点侧指令通道：挂指挥官的 /cluster/inbox SSE，收到指令即回调
// exec（exec 在调用方 goroutine 串行执行——节点同一时刻本就只跑一个集群
// 任务槽的语义由 exec 自身决定，这里只保证投递顺序）。断线自动重连
//（3s 退避），直到 ctx 取消。exec 返回错误只记日志投递侧无重试语义
//（任务级失败经 /cluster/events 回投，指令本身不该重试）。
func InboxLoop(ctx context.Context, commanderURL, token string, self Identity, exec func(cmd InboxCommand)) {
	c := NewNodeClient(commanderURL, token)
	for {
		if err := c.Inbox(ctx, self.ID, self.Name, exec); err != nil && ctx.Err() == nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// Inbox 挂接指挥官指令流（SSE）：GET /cluster/inbox?node_id=&name=，逐条
// "data: {...}" 解析回调 onCmd。连接保持到 ctx 取消或网络断开（返回错误
// 由 InboxLoop 重连）。
func (c *NodeClient) Inbox(ctx context.Context, nodeID, name string, onCmd func(InboxCommand)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.Base+"/cluster/inbox?node_id="+nodeID+"&name="+name, nil)
	if err != nil {
		return err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Accept", "text/event-stream")
	// 不用 c.HTTP（30s 整体超时会掐断长连）；无超时 client 生命周期由
	// ctx 控制，pin 配置经 pushHTTP 复用
	resp, err := c.pushHTTP().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("cluster inbox: %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	br := bufio.NewReader(resp.Body)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(line, "data: ") {
			continue // 事件名行/注释 keepalive：载荷在 data 行
		}
		var cmd InboxCommand
		if json.Unmarshal([]byte(line[len("data: "):]), &cmd) != nil || cmd.Type == "" {
			continue
		}
		onCmd(cmd)
	}
}

// PostSettle 回投落定事件（节点→指挥官 POST /cluster/events）。尽力而为：
// 失败仅返回错误（调用方记日志；直连场景指挥官还有轮询兜底，中继场景
// 节点会在下一回合事件里再报——极端丢失时指挥官按节点掉线收敛）。
func (c *NodeClient) PostSettle(ctx context.Context, ev InboxEvent) error {
	ev.TS = time.Now().UnixMilli()
	return c.call(ctx, http.MethodPost, "/cluster/events", ev, nil)
}
