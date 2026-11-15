// table.go 成员表（指挥官权威）：静态节点（settings cluster.nodes）与
// call-home 注册节点合并持有，register/heartbeat 驱动状态。M1 状态仅由
// 注册/心跳驱动——静态节点未经探测前 status=unknown；心跳过期转
// offline（成员保留，复活自动恢复）。
package cluster

import (
	"sort"
	"sync"
	"time"

	"git.gorun.fun/share/grf-harness/config"
)

// 成员状态。
const (
	StatusOnline  = "online"
	StatusOffline = "offline"
	StatusUnknown = "unknown"
)

// 成员来源。
const (
	SourceStatic     = "static"
	SourceRegistered = "registered"
	SourceMdns       = "mdns"
)

// heartbeatTTL 心跳过期阈值（15s 间隔 × 6 个周期）；超时成员转 offline。
const heartbeatTTL = 90 * time.Second

// Member 成员表条目。
type Member struct {
	NodeID   string        `json:"node_id,omitempty"` // call-home 节点有；静态节点空
	Name     string        `json:"name"`
	Addr     string        `json:"addr,omitempty"`
	Labels   []string      `json:"labels,omitempty"`
	Source   string        `json:"source"` // static | registered
	Status   string        `json:"status"` // online | offline | unknown
	Running  int           `json:"running_turns,omitempty"`
	LastSeen int64         `json:"last_seen,omitempty"` // unix 秒
	Caps     *Capabilities `json:"capabilities,omitempty"`
}

// Table 成员表。并发安全。成员数量与节点数同量级（个位到几十），
// 查找一律线性扫描。
type Table struct {
	mu      sync.Mutex
	members map[string]*Member // 键 = 路由名（NodeName，节点内唯一）
}

// NewTable 构建成员表并用静态节点表播种（status=unknown，addr/labels
// 来自配置）。
func NewTable(nodes []config.ClusterNode) *Table {
	t := &Table{members: map[string]*Member{}}
	for _, n := range nodes {
		name := NodeName(n)
		t.members[name] = &Member{
			Name:   name,
			Addr:   n.Addr,
			Labels: append([]string(nil), n.Labels...),
			Source: SourceStatic,
			Status: StatusUnknown,
		}
	}
	return t
}

// Register 注册/复活节点（call-home）：upsert by 路由名；静态同名条目
// 保留静态属性（addr/labels），仅刷新状态与能力。
func (t *Table) Register(m Member) {
	t.mu.Lock()
	defer t.mu.Unlock()
	name := m.Name
	if name == "" {
		return
	}
	cur, ok := t.members[name]
	if !ok {
		c := m
		c.Name = name
		c.Source = SourceRegistered
		c.Status = StatusOnline
		c.LastSeen = time.Now().Unix()
		t.members[name] = &c
		return
	}
	// 已存在：静态条目保留 addr/labels，其余跟随注册上报
	cur.NodeID = m.NodeID
	if cur.Source != SourceStatic && m.Addr != "" {
		cur.Addr = m.Addr
	}
	if cur.Source != SourceStatic && len(m.Labels) > 0 {
		cur.Labels = append([]string(nil), m.Labels...)
	}
	if m.Caps != nil {
		caps := *m.Caps
		cur.Caps = &caps
	}
	cur.Status = StatusOnline
	cur.LastSeen = time.Now().Unix()
}

// Heartbeat 心跳刷新（running 为节点当前在跑回合数）。未注册节点自动
// 以最小条目入表（registered）。返回是否首次出现。
func (t *Table) Heartbeat(nodeID, name string, running int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if name == "" {
		name = nodeID
	}
	if cur, ok := t.members[name]; ok {
		cur.NodeID = nodeID
		cur.Status = StatusOnline
		cur.Running = running
		cur.LastSeen = time.Now().Unix()
		return false
	}
	t.members[name] = &Member{
		NodeID: nodeID, Name: name, Source: SourceRegistered,
		Status: StatusOnline, Running: running, LastSeen: time.Now().Unix(),
	}
	return true
}

// Get 按路由名取成员快照（不存在返回 nil）。
func (t *Table) Get(name string) *Member {
	t.mu.Lock()
	defer t.mu.Unlock()
	if m, ok := t.members[name]; ok {
		c := *m
		if c.Labels != nil {
			c.Labels = append([]string(nil), m.Labels...)
		}
		return &c
	}
	return nil
}

// SetCaps 补录成员能力清单（mDNS 发现后拉取 /cluster/capabilities 的
// 回填；静态/注册成员的能力由注册上报，一般不走这里）。未知成员忽略。
func (t *Table) SetCaps(name string, caps *Capabilities) {
	if caps == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if m, ok := t.members[name]; ok {
		c := *caps
		m.Caps = &c
	}
}

// Known 节点是否已在表中（邀请码门用：未知节点的首次心跳等价注册，
// 须过门；已知节点心跳免验——掉线重连不必反复出示邀请码）。
func (t *Table) Known(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.members[name]
	return ok
}

// Announce 发现通道（mDNS）存活上报：按路由名 upsert（保留静态条目属性），
// 刷新在线状态与 LastSeen。与 heartbeat 分开命名——语义来源不同，快照
// 渲染时可区分 trust 级别（call-home 是节点主动投靠，mDNS 是局域网广播）。
func (t *Table) Announce(nodeID, name, addr string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if name == "" {
		return
	}
	cur, ok := t.members[name]
	if !ok {
		t.members[name] = &Member{
			NodeID: nodeID, Name: name, Addr: addr, Source: SourceMdns,
			Status: StatusOnline, LastSeen: time.Now().Unix(),
		}
		return
	}
	cur.NodeID = nodeID
	if cur.Source == SourceMdns && addr != "" {
		cur.Addr = addr
	}
	cur.Status = StatusOnline
	cur.LastSeen = time.Now().Unix()
}

// Snapshot 成员快照（路由名字典序）。心跳/发现过期的 registered 与
// mdns 成员转 offline（不影响存储的 LastSeen）；static 成员无心跳概念，
// 保持 unknown/最近注册状态。
func (t *Table) Snapshot() []Member {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Member, 0, len(t.members))
	now := time.Now()
	for _, m := range t.members {
		c := *m
		if c.Labels != nil {
			c.Labels = append([]string(nil), m.Labels...)
		}
		if (c.Source == SourceRegistered || c.Source == SourceMdns) && c.LastSeen > 0 &&
			now.Sub(time.Unix(c.LastSeen, 0)) > heartbeatTTL {
			c.Status = StatusOffline
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
