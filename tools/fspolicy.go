// fspolicy.go 文件策略引擎：观察策略（fs_policy=observe，默认——覆写已
// 存在文件前要求本会话 read 过）、工作区写边界、以及"一次性升级重试"
// 协议（sandbox_permissions + justification → Host.ApproveEscalation）。
package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// FsPolicy 文件策略：观察策略（写前须读）、工作区写边界与升级审批。
type FsPolicy struct {
	mu        sync.Mutex
	Observe   bool // 写/改已有文件前要求本会话内 read 过
	Workspace string
	Host      Host

	read   map[string]string // abs 路径 -> 内容摘要（写前校验读过）
	denied map[string]string // 升级键(tool:abs) -> 被拒模式，供一次性重试校验
}

func NewFsPolicy(workspace string, observe bool, host Host) *FsPolicy {
	return &FsPolicy{
		Observe:   observe,
		Workspace: absOr(workspace),
		Host:      host,
		read:      map[string]string{},
		denied:    map[string]string{},
	}
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

// InWorkspace 路径是否在工作区内。
func (f *FsPolicy) InWorkspace(p string) bool {
	abs := absOr(p)
	return abs == f.Workspace || strings.HasPrefix(abs, f.Workspace+string(filepath.Separator))
}

// CheckWrite 写前校验：工作区边界 + 观察策略（覆写已存在文件前要求本会话
// read 过；新建文件不受限）。返回 (abs, error)。
func (f *FsPolicy) CheckWrite(path string, readBeforeEdit bool) (string, error) {
	abs := f.resolve(path)
	f.mu.Lock()
	_, hasRead := f.read[abs]
	f.mu.Unlock()

	if !f.InWorkspace(abs) {
		f.mu.Lock()
		f.denied["write:"+abs] = "workspace-write"
		f.mu.Unlock()
		return abs, fmt.Errorf("[sandbox: file access denied under workspace-write mode] path %q is outside the workspace %q; to write it, retry the exact same call once with sandbox_permissions plus a one-sentence justification", path, f.Workspace)
	}
	if f.Observe && readBeforeEdit && !hasRead {
		if _, err := os.Stat(abs); err == nil {
			return abs, fmt.Errorf("refusing to modify %q: read it first with the read tool (the default fs-observation-policy requires it). If you just created or edited it in this session, that already counts", path)
		}
	}
	return abs, nil
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
	f.mu.Unlock()
	if !ok {
		return false, fmt.Errorf("sandbox_permissions is only valid as a one-shot retry of an operation the sandbox just denied; there is no recorded denial for this exact %s call — run it normally first and read the denial marker", tool)
	}
	if f.Host == nil {
		return false, fmt.Errorf("no host available to approve the escalation")
	}
	if !f.Host.ApproveEscalation(context.Background(), tool, key, mode, justification) {
		return false, fmt.Errorf("the user rejected the escalation for this %s call — stop and explain, never work around it (attempting or escalating other calls later is still allowed)", tool)
	}
	// 一次性：消费掉拒绝记录
	f.mu.Lock()
	delete(f.denied, tool+":"+key)
	f.mu.Unlock()
	_ = deniedMode
	return true, nil
}
