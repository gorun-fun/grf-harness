// fspolicy.go 文件策略引擎：写侧保护（统一规则串 policy 的 write-workspace，默认——覆写已
// 存在文件前要求本会话 read 过）、工作区写边界、以及"一次性升级重试"
// 协议（sandbox_permissions + justification → Host.ApproveEscalation）。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// FsPolicy 文件策略：观察策略（写/改已有文件前要求本会话内 read 过）、
// 工作区写边界、读边界（统一规则串 policy 的 read-workspace）与升级审批。
type FsPolicy struct {
	mu        sync.Mutex
	Observe   bool // 写/改已有文件前要求本会话内 read 过
	ReadLimit bool // 读边界：read/glob/grep/read_image 仅可访问工作区内路径
	Workspace string
	// wsReal 工作区展开符号链接后的真实路径（macOS 的 /var → /private/var
	// 等；解析失败时与 Workspace 相同）。边界判断对真实路径生效，防工作区
	// 内符号链接指向外部造成穿越。
	wsReal string
	Host   Host

	read   map[string]string // abs 路径 -> 内容摘要（写前校验读过）
	denied map[string]string // 升级键(tool:abs) -> 被拒模式，供一次性重试校验
	grants map[string]bool   // 升级审批授权的目录（工作区 .grf/grants.json，仅本工作区）
}

func NewFsPolicy(workspace string, observe, readLimit bool, host Host) *FsPolicy {
	ws := absOr(workspace)
	real := ws
	if r, err := filepath.EvalSymlinks(ws); err == nil {
		real = r
	}
	f := &FsPolicy{
		Observe:   observe,
		ReadLimit: readLimit,
		Workspace: ws,
		wsReal:    real,
		Host:      host,
		read:      map[string]string{},
		denied:    map[string]string{},
		grants:    map[string]bool{},
	}
	f.loadGrants()
	return f
}

// grantsPath 授权目录持久化文件：<工作区>/.grf/grants.json。
func (f *FsPolicy) grantsPath() string {
	return filepath.Join(f.wsReal, ".grf", "grants.json")
}

// loadGrants 加载本工作区的授权目录（不存在视为空）。
func (f *FsPolicy) loadGrants() {
	data, err := os.ReadFile(f.grantsPath())
	if err != nil {
		return
	}
	var dirs []string
	if json.Unmarshal(data, &dirs) != nil {
		return
	}
	for _, d := range dirs {
		f.grants[filepath.Clean(absOr(d))] = true
	}
}

// saveGrants 落盘授权目录。
func (f *FsPolicy) saveGrants() {
	dirs := make([]string, 0, len(f.grants))
	for d := range f.grants {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	data, err := json.MarshalIndent(dirs, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(f.grantsPath()), 0o755)
	_ = os.WriteFile(f.grantsPath(), data, 0o644)
}

// grantDir 授权一个目录（升级审批"批准并授权目录"）。
func (f *FsPolicy) grantDir(dir string) {
	f.mu.Lock()
	f.grants[filepath.Clean(absOr(dir))] = true
	f.mu.Unlock()
	f.saveGrants()
}

// dirGranted 路径所在目录是否已被授权（授权目录内的写操作越过写边界，
// 不再询问；读边界不受影响）。
func (f *FsPolicy) dirGranted(abs string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.grants[filepath.Dir(abs)]
}

func absOr(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return a
}

// resolve 把路径解析为绝对路径：相对路径相对工作区（filesystem backend
// 语义），绝对路径原样（符号链接不展开，交给边界判断）。
func (f *FsPolicy) resolve(p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(f.Workspace, p)
	}
	return filepath.Clean(p)
}

// MarkRead 记录一次成功的文件读取。
func (f *FsPolicy) MarkRead(abs string) {
	f.mu.Lock()
	f.read[abs] = "ok"
	f.mu.Unlock()
}

// MarkWritten 记录一次写入（新写入后再次 edit 不需要重新 read）。
func (f *FsPolicy) MarkWritten(abs string) {
	f.mu.Lock()
	f.read[abs] = "ok"
	f.mu.Unlock()
}

// realPath 展开符号链接后的真实路径。文件（或其祖先）尚不存在时（write
// 新文件的常见情形），展开最近的已存在祖先再拼回余下部分；全部不存在则
// 原样返回。
func (f *FsPolicy) realPath(abs string) string {
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	dir, rest := filepath.Dir(abs), filepath.Base(abs)
	for {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return abs // 上溯到根仍不可解析
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
}

// InWorkspace 路径是否在工作区内：按展开符号链接后的真实路径判定，
// 工作区内的符号链接若指向外部即视为越界（防穿越）。
func (f *FsPolicy) InWorkspace(p string) bool {
	real := f.realPath(absOr(p))
	for _, root := range []string{f.Workspace, f.wsReal} {
		if real == root || strings.HasPrefix(real, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// CheckWrite 写前校验：工作区边界 + 观察策略（覆写已存在文件前要求本会话
// read 过；新建文件不受限）。返回 (abs, error)。
func (f *FsPolicy) CheckWrite(path string, readBeforeEdit bool) (string, error) {
	abs := f.resolve(path)
	f.mu.Lock()
	_, hasRead := f.read[abs]
	f.mu.Unlock()

	if !f.InWorkspace(abs) && !f.dirGranted(abs) {
		f.mu.Lock()
		f.denied["write:"+abs] = "write-workspace"
		f.mu.Unlock()
		return abs, fmt.Errorf("[sandbox: file access denied under write-workspace mode] path %q is outside the workspace %q; to write it, user approval is required and was not granted (or no host is available) — this is final for this file", path, f.Workspace)
	}
	if f.Observe && readBeforeEdit && !hasRead {
		if _, err := os.Stat(abs); err == nil {
			return abs, fmt.Errorf("refusing to modify %q: read it first with the read tool (the default fs-observation-policy requires it). If you just created or edited it in this session, that already counts", path)
		}
	}
	return abs, nil
}

// CheckRead 读前校验：读边界（policy 的 read-workspace）开启时禁止读
// 工作区外路径。硬拒绝，不记 denied、不接升级协议（多租户场景用户不可信、
// 无审批方）。返回 (abs, error)。
func (f *FsPolicy) CheckRead(path string) (string, error) {
	abs := f.resolve(path)
	if f.ReadLimit && !f.InWorkspace(abs) {
		return abs, fmt.Errorf("[read-policy: path %q is outside the workspace %q; reading outside the workspace is not permitted in this mode]", path, f.Workspace)
	}
	return abs, nil
}

// AskAndEscalate 越权即问：工具被写边界拒绝后由 harness 直接征求宿主批准
// （阻塞至用户作答，不随工具调用取消）。批准/拒绝都消费掉拒绝记录——
// 一次性，拒绝后模型带 sandbox_permissions 重试也不会二次询问（按无记录
// 拒绝）。detail 作为展示给用户的理由。无宿主或无记录返回 false。
func (f *FsPolicy) AskAndEscalate(tool, key, mode, detail string) bool {
	if f.Host == nil {
		return false
	}
	f.mu.Lock()
	_, ok := f.denied[tool+":"+key]
	if !ok && tool == "edit" { // edit 的拒绝记录统一为 "write:" 前缀
		_, ok = f.denied["write:"+key]
	}
	f.mu.Unlock()
	if !ok {
		return false
	}
	decision := f.Host.ApproveEscalation(context.Background(), tool, key, mode, detail)
	if decision.Approved && decision.GrantDir != "" {
		f.grantDir(decision.GrantDir)
	}
	f.mu.Lock()
	delete(f.denied, tool+":"+key)
	if tool == "edit" {
		delete(f.denied, "write:"+key)
	}
	f.mu.Unlock()
	return decision.Approved
}

// Escalate 校验并执行一次性升级：tool 为 "write"/"edit"/"bash"，key 为
// 被拒资源（路径或命令）。无此前拒绝记录时按投机升级拒绝。审批属 UI
// 交互，不随工具调用取消（用独立 context，保留由宿主自行决定）。
func (f *FsPolicy) Escalate(tool, key, mode, justification string) (bool, error) {
	if strings.TrimSpace(justification) == "" {
		return false, fmt.Errorf("sandbox_permissions requires a one-sentence justification")
	}
	f.mu.Lock()
	deniedMode, ok := f.denied[tool+":"+key]
	if !ok && tool == "edit" {
		// edit 的拒绝记录由 CheckWrite 统一记为 "write:" 前缀
		deniedMode, ok = f.denied["write:"+key]
	}
	f.mu.Unlock()
	if !ok {
		return false, fmt.Errorf("sandbox_permissions is only valid as a one-shot retry of an operation the sandbox just denied; there is no recorded denial for this exact %s call — run it normally first and read the denial marker", tool)
	}
	if f.Host == nil {
		return false, fmt.Errorf("no host available to approve the escalation")
	}
	if !f.Host.ApproveEscalation(context.Background(), tool, key, mode, justification).Approved {
		return false, fmt.Errorf("the user rejected the escalation for this %s call — stop and explain, never work around it (attempting or escalating other calls later is still allowed)", tool)
	}
	// 一次性：消费掉拒绝记录
	f.mu.Lock()
	delete(f.denied, tool+":"+key)
	f.mu.Unlock()
	_ = deniedMode
	return true, nil
}
