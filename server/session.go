// session.go 会话池与磁盘模型：会话 = <data>/conversations/<sid>/ 目录
// （workspace + meta.json + responses/ 快照），内存只是缓存——任何请求遇到
// 目录存在的未加载会话先懒加载（每会话一把加载锁）；server 重启后凭
// previous_response_id 即可续接（含暂停态：未闭合 tool_call 就在持久化
// 历史里）。
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"git.gorun.fun/share/grf-harness/agent"
	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/llm"
	"git.gorun.fun/share/grf-harness/store"
	"git.gorun.fun/share/grf-harness/tools"
)

// sessionMeta meta.json：会话的持久配置（懒加载沿用，配置不随重启漂移）。
type sessionMeta struct {
	ID            string    `json:"id"`
	CreatedAt     time.Time `json:"created_at"`
	Archived      bool      `json:"archived,omitempty"`
	Workspace     string    `json:"workspace"`
	SystemPrompt  string    `json:"system_prompt,omitempty"`
	Policy        string    `json:"policy,omitempty"`
	AutoApprove   bool      `json:"auto_approve,omitempty"`
	MaxSteps      int       `json:"max_steps,omitempty"`
	DisabledTools []string  `json:"disabled_tools,omitempty"`
	// ClientTools 接入方注册的客户端函数工具定义（暂停类别判定依赖它，
	// 必须随会话持久化——跨重启恢复暂停回合时仍要正确分类）。
	ClientTools []toolDef `json:"client_tools,omitempty"`
}

// toolDef 持久化的工具定义。
type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// session 一个已加载的会话（harness + 回合簿记）。
type session struct {
	srv *Server
	mu  sync.Mutex
	// 以下字段由 srv.mu 或 sess.mu 保护：meta/harness 构建后只读；
	// cancel/curSeq/turnDone 为回合簿记。
	meta   *sessionMeta
	h      *agent.Harness
	root   *agent.Agent
	cancel context.CancelFunc // 当前回合取消（nil = 空闲）
	curSeq int                // 最新已分配的 response 序号
	seq    atomic.Int64       // 会话级 sequence_number 发号器（ORS 事件排序契约）
	// clientTools 本会话已注册的客户端函数工具（每请求重置前先清理）。
	clientTools []string
	turnDone    chan struct{} // 当前回合结束信号（close 于回合收尾）
}

// nextSeq 会话级 sequence_number（跨回合单调）。
func (sess *session) nextSeq() int64 { return sess.seq.Add(1) }

// Server HTTP 接入服务。
type Server struct {
	dataDir string
	token   string

	mu       sync.Mutex
	sessions map[string]*session // sid → 已加载会话

	// clientFactory 构建每会话的 LLM 客户端（默认读 ~/.grf 配置；测试注入
	// fakeLLM）。
	clientFactory func() (*llm.Client, error)
}

// New 构建 Server。token 为空时自动生成并写入 <data>/token（0600）。
func New(dataDir, token string) (*Server, error) {
	if err := os.MkdirAll(filepath.Join(dataDir, "conversations"), 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录: %w", err)
	}
	if token == "" {
		tok, err := loadOrCreateToken(filepath.Join(dataDir, "token"))
		if err != nil {
			return nil, err
		}
		token = tok
	}
	return &Server{
		dataDir:       dataDir,
		token:         token,
		sessions:      map[string]*session{},
		clientFactory: defaultClient,
	}, nil
}

// Token 访问令牌（main 打印用）。
func (s *Server) Token() string { return s.token }

// DataDir 数据目录。
func (s *Server) DataDir() string { return s.dataDir }

// defaultClient 从全局配置构建客户端。
func defaultClient() (*llm.Client, error) {
	eff, err := config.Load()
	if err != nil {
		return nil, err
	}
	return llm.NewClient(llm.Config{BaseURL: eff.BaseURL, APIKey: eff.APIKey, Model: eff.Model}), nil
}

// loadOrCreateToken 读取或生成持久令牌。
func loadOrCreateToken(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		if t := strings.TrimSpace(string(b)); t != "" {
			return t, nil
		}
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成令牌: %w", err)
	}
	tok := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(tok), 0o600); err != nil {
		return "", fmt.Errorf("写入令牌文件: %w", err)
	}
	return tok, nil
}

func (s *Server) convDir(sid string) string {
	return filepath.Join(s.dataDir, "conversations", sid)
}

// newSID 生成 8 字节 hex 会话 ID。
func newSID() string {
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// newSession 新建会话（写 meta.json，构建 harness 并缓存）。
func (s *Server) newSession(cfg *grfSessionConfig) (*session, error) {
	sid := newSID()
	dir := s.convDir(sid)
	ws := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(filepath.Join(dir, "responses"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(ws, 0o755); err != nil {
		return nil, err
	}
	meta := &sessionMeta{
		ID:        sid,
		CreatedAt: time.Now(),
		Workspace: ws,
	}
	if cfg != nil {
		meta.SystemPrompt = cfg.SystemPrompt
		meta.Policy = cfg.Policy
		meta.MaxSteps = cfg.MaxSteps
		meta.DisabledTools = cfg.DisabledTools
		if cfg.AutoApprove != nil {
			meta.AutoApprove = *cfg.AutoApprove
		}
	}
	if err := writeMeta(dir, meta); err != nil {
		return nil, err
	}
	sess, err := s.buildSession(meta)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.sessions[sid] = sess
	s.mu.Unlock()
	return sess, nil
}

// buildSession 按 meta 构建 harness（加载存档恢复历史，含暂停态）。
func (s *Server) buildSession(meta *sessionMeta) (*session, error) {
	client, err := s.clientFactory()
	if err != nil {
		return nil, fmt.Errorf("构建模型客户端: %w", err)
	}
	// Policy 空值走 ParsePolicy 的默认（off 全关）
	settings := &config.Settings{}
	if meta.Policy != "" {
		settings.Policy = config.NormalizePolicy(meta.Policy)
	}
	settings.AutoApprove = meta.AutoApprove
	host := &deferredHost{NoopHost: tools.NoopHost{AutoApprove: meta.AutoApprove}}
	h, err := agent.Build(agent.Options{
		Workspace:     meta.Workspace,
		Client:        client,
		Settings:      settings,
		Host:          host,
		SystemPrompt:  meta.SystemPrompt,
		MaxSteps:      meta.MaxSteps,
		DisabledTools: meta.DisabledTools,
	})
	if err != nil {
		return nil, err
	}
	sess := &session{srv: s, meta: meta, h: h, root: h.Root}
	// 恢复客户端函数工具注册（暂停类别判定依赖）。
	for _, td := range meta.ClientTools {
		h.Root.Reg().RegisterClient(llm.Tool{Name: td.Name, Description: td.Description, Parameters: td.Parameters})
		sess.clientTools = append(sess.clientTools, td.Name)
	}
	// 恢复该目录下最新存档（磁盘为准：重启后自动接上历史与暂停态）。
	if infos := h.Store.ListSessions(1); len(infos) > 0 {
		if full, err := store.LoadSessionFull(infos[0].Path); err == nil && len(full.Messages) > 0 {
			h.Root.RestoreFull(full.Messages, full.System)
			if full.CacheKey != "" {
				h.SetCacheKey(full.CacheKey)
			}
		}
	}
	sess.curSeq = sess.maxSnapSeq()
	return sess, nil
}

// getSession 取会话：内存命中或从磁盘懒加载；不存在/已归档返回错误。
func (s *Server) getSession(sid string) (*session, error) {
	s.mu.Lock()
	if sess, ok := s.sessions[sid]; ok {
		s.mu.Unlock()
		if sess.meta.Archived {
			return nil, apiErr(errNotFound, "session is archived")
		}
		return sess, nil
	}
	s.mu.Unlock()
	// 磁盘为准：目录存在即会话存在（并发首触达由会话级锁兜底）
	if _, err := os.Stat(s.convDir(sid)); err != nil {
		return nil, apiErr(errNotFound, "unknown session "+sid)
	}
	s.mu.Lock()
	defer s.mu.Unlock() // 持锁完成加载，防 double-load
	if sess, ok := s.sessions[sid]; ok {
		return sess, nil
	}
	meta, err := readMeta(s.convDir(sid))
	if err != nil {
		return nil, apiErr(errInvalid, "corrupt session meta: "+err.Error())
	}
	if meta.Archived {
		return nil, apiErr(errNotFound, "session is archived")
	}
	sess, err := s.buildSession(meta)
	if err != nil {
		return nil, apiErr(errServer, "load session: "+err.Error())
	}
	s.sessions[sid] = sess
	return sess, nil
}

// dropSession 关闭并移除内存缓存（磁盘保留）。
func (s *Server) dropSession(sid string) {
	s.mu.Lock()
	sess := s.sessions[sid]
	delete(s.sessions, sid)
	s.mu.Unlock()
	if sess != nil {
		sess.h.Close()
	}
}

// writeMeta / readMeta meta.json 落盘与读取。
func writeMeta(dir string, meta *sessionMeta) error {
	data, err := json.MarshalIndent(meta, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "meta.json"), data, 0o644)
}

func readMeta(dir string) (*sessionMeta, error) {
	data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, err
	}
	var meta sessionMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

// ---- response 快照（responses/NNNN.json）----

// respID response id 形如 resp_<sid>_<seq>（sid 可从 id 直接解析）。
func respID(sid string, seq int) string {
	return fmt.Sprintf("resp_%s_%04d", sid, seq)
}

// parseRespID 拆出 (sid, seq)。
func parseRespID(id string) (string, int, error) {
	parts := strings.Split(id, "_")
	if len(parts) != 3 || parts[0] != "resp" {
		return "", 0, fmt.Errorf("malformed response id")
	}
	seq, err := strconv.Atoi(parts[2])
	if err != nil || seq < 1 {
		return "", 0, fmt.Errorf("malformed response id")
	}
	return parts[1], seq, nil
}

// nextResp 分配下一个 response id（会话内单调）。
func (sess *session) nextResp() string {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	sess.curSeq++
	return respID(sess.meta.ID, sess.curSeq)
}

// rollbackResp 撤销一次未产生资源的 respID 分配（输入校验失败路径——
// 校验在回合执行内才发生，此时 id 已分配；不回滚会推进链尾却没有对应
// 资源，后续 previous_response_id 校验全部错位）。
func (sess *session) rollbackResp(id string) {
	if _, seq, err := parseRespID(id); err == nil {
		sess.mu.Lock()
		if sess.curSeq == seq {
			sess.curSeq--
		}
		sess.mu.Unlock()
	}
}

// maxSnapSeq 扫描快照目录求最大序号（懒加载时恢复链尾）。
func (sess *session) maxSnapSeq() int {
	entries, err := os.ReadDir(filepath.Join(sess.srv.convDir(sess.meta.ID), "responses"))
	if err != nil {
		return 0
	}
	max := 0
	for _, e := range entries {
		if n, err := strconv.Atoi(strings.TrimSuffix(e.Name(), ".json")); err == nil && n > max {
			max = n
		}
	}
	return max
}

// snapPath 快照文件路径。
func (sess *session) snapPath(seq int) string {
	return filepath.Join(sess.srv.convDir(sess.meta.ID), "responses", fmt.Sprintf("%04d.json", seq))
}

// saveSnap 写终态快照（store=false 时调用方跳过）。
func (sess *session) saveSnap(r *ResponseResource) {
	_, seq, err := parseRespID(r.ID)
	if err != nil {
		return
	}
	data, err := json.MarshalIndent(r, "", " ")
	if err != nil {
		return
	}
	_ = os.WriteFile(sess.snapPath(seq), data, 0o644)
}

// loadSnap 读快照。
func (sess *session) loadSnap(seq int) (*ResponseResource, error) {
	data, err := os.ReadFile(sess.snapPath(seq))
	if err != nil {
		return nil, err
	}
	var r ResponseResource
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// curRespID 当前链尾 response id。
func (sess *session) curRespID() string {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.curSeq == 0 {
		return ""
	}
	return respID(sess.meta.ID, sess.curSeq)
}

// listConversations 枚举磁盘会话（分页参数由 handler 处理）。
func (s *Server) listConversations() []*sessionMeta {
	entries, err := os.ReadDir(filepath.Join(s.dataDir, "conversations"))
	if err != nil {
		return nil
	}
	var metas []*sessionMeta
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if meta, err := readMeta(filepath.Join(s.dataDir, "conversations", e.Name())); err == nil {
			metas = append(metas, meta)
		}
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].CreatedAt.After(metas[j].CreatedAt) })
	return metas
}

// autoApproveSetting 会话的审批默认值。
func (sess *session) autoApproveSetting() bool { return sess.meta.AutoApprove }

// ---- 回合簿记（cancel 端点与 busy 检查用）----

// beginTurn 登记回合取消句柄与结束信号。
func (sess *session) beginTurn(cancel context.CancelFunc) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	sess.cancel = cancel
	sess.turnDone = make(chan struct{})
}

// endTurn 回合收尾：清取消句柄并广播结束。
func (sess *session) endTurn() {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	sess.cancel = nil
	if sess.turnDone != nil {
		close(sess.turnDone)
		sess.turnDone = nil
	}
}

// cancelTurn 取消当前回合（无在跑回合返回 false）。
func (sess *session) cancelTurn() bool {
	sess.mu.Lock()
	c := sess.cancel
	sess.mu.Unlock()
	if c == nil {
		return false
	}
	c()
	return true
}

// waitTurn 等待回合结束（无在跑回合立即 true）。
func (sess *session) waitTurn(d time.Duration) bool {
	sess.mu.Lock()
	ch := sess.turnDone
	sess.mu.Unlock()
	if ch == nil {
		return true
	}
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}
