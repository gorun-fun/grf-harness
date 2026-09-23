// state.go 元素状态树与 state_id 缓存：computer_get_app_state 产出 DFS
// 先序定序的元素树（每元素一个稳定索引），动作工具携带 {state_id, index}
// 定位元素；平台层按同一确定性遍历序重走路径并校验 role/name 签名，界面
// 变化即拒绝执行，逼模型重新观察——这是"观察→动作→再观察"闭环的关键。
package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// AppInfo 运行中的应用。
type AppInfo struct {
	PID       int
	Name      string // 应用名（进程名）
	Frontmost bool
}

// WinInfo 应用的一个窗口。ID 为平台窗口标识（macOS=CGWindowID，可直传
// computer_screenshot 的 window_id；Linux=X window id；Windows=HWND）。
type WinInfo struct {
	ID    int64
	PID   int
	App   string
	Title string
	X, Y  int
	W, H  int
	Main  bool // 是否该应用当前主窗口/焦点窗口
}

// Node 元素树节点（扁平存储，平台层按 DFS 先序输出）。Path 为从窗口起的
// 1 起子序号路径（如 "2.5.1"，macOS/Windows 直接还原成 UI element 链）；
// WindowID 仅窗口层节点携带（>0 即窗口）。
type Node struct {
	Depth    int
	Path     string
	Role     string // 稳定角色标识（macOS "AXButton"、Windows "Button"、Linux atspi role）
	Name     string // 可见标题/标签
	Desc     string // 辅助描述（占位符提示等）
	Value    string // 当前值（文本内容/输入框内容，已截断）
	X, Y     int    // 元素全局坐标（左上）
	W, H     int
	Enabled  bool
	WindowID int64
}

// State 一次观察的快照。
type State struct {
	ID   string
	PID  int
	App  string
	At   time.Time
	Nodes []*Node
}

// stateTTL 快照过期时间；stateMax 最多保留的快照个数。
const stateTTL = 10 * time.Minute

// stateStore 进程内 state_id → 快照缓存（最近 stateMax 个）。
type stateStore struct {
	mu    sync.Mutex
	order []string
	m     map[string]*State
	seq   int64
}

var states = &stateStore{m: map[string]*State{}}

// put 记录一次快照，生成新 state_id（递增序号 + 时间戳）。
func (s *stateStore) put(pid int, app string, nodes []*Node) *State {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	st := &State{
		ID:    fmt.Sprintf("s%d-%d", time.Now().Unix(), s.seq),
		PID:   pid, App: app, At: time.Now(), Nodes: nodes,
	}
	s.m[st.ID] = st
	s.order = append(s.order, st.ID)
	if len(s.order) > 8 {
		delete(s.m, s.order[0])
		s.order = s.order[1:]
	}
	return st
}

// get 取快照（不存在或已过期返回 ok=false）。
func (s *stateStore) get(id string) (*State, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.m[id]
	if !ok || time.Since(st.At) > stateTTL {
		return nil, false
	}
	return st, true
}

// renderState 把快照渲染成给模型读的紧凑缩进文本。
func renderState(st *State) string {
	var b strings.Builder
	fmt.Fprintf(&b, "state_id=%s app=%q pid=%d 元素数=%d\n", st.ID, st.App, st.PID, len(st.Nodes))
	b.WriteString("格式: [索引] 角色 \"名称\" =值 (x,y,w,h) ✗禁用；动作工具传 state_id+index 定位元素\n")
	for i, n := range st.Nodes {
		if i == 0 {
			b.WriteString("窗口:\n")
		}
		indent := strings.Repeat("  ", n.Depth)
		line := fmt.Sprintf("%s[%d] %s", indent, i, n.Role)
		if n.Name != "" {
			line += fmt.Sprintf(" %q", trunc(n.Name, 60))
		}
		if n.Desc != "" {
			line += fmt.Sprintf(" (%s)", trunc(n.Desc, 60))
		}
		if n.Value != "" {
			line += fmt.Sprintf(" =%q", trunc(n.Value, 80))
		}
		if n.W > 0 {
			line += fmt.Sprintf(" (%d,%d %dx%d)", n.X, n.Y, n.W, n.H)
		}
		if !n.Enabled {
			line += " ✗"
		}
		if n.WindowID > 0 {
			line += fmt.Sprintf(" window_id=%d", n.WindowID)
		}
		b.WriteString(line + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// trunc 截断字符串（按 rune）。
func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// errChanged 界面已变化（快照过期，元素签名对不上）。
type errChanged struct{ detail string }

func (e *errChanged) Error() string {
	return "界面已变化：" + e.detail + "；请重新 computer_get_app_state 观察后再操作"
}

// changedError 把平台层回报的实际签名与快照比对，不一致返回 *errChanged。
// name 允许变化（动态标题常见），role 必须一致；name 也比对但仅在快照记录了
// 非空名称时——为空名称的元素常是匿名容器。
func changedError(idx int, want, got *Node) error {
	if want.Role != got.Role {
		return &errChanged{fmt.Sprintf("元素 [%d] 角色已从 %s 变为 %s", idx, want.Role, got.Role)}
	}
	if want.Name != "" && got.Name != want.Name {
		return &errChanged{fmt.Sprintf("元素 [%d] 名称已从 %q 变为 %q", idx, want.Name, got.Name)}
	}
	return nil
}
