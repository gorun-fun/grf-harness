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
	"reflect"
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
	DefaultSkill  string    `json:"default_skill,omitempty"`
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
	// 以下字段由 srv.mu 或 sess.mu 保护：meta/harness 构建后只读
	// （applyTools 写 ClientTools、归档写 Archived，由单 turn 串行保护）；
	// busy/cancel/curSeq/turnDone 为回合簿记。
	meta *sessionMeta
	h    *agent.Harness
	root *agent.Agent
	// busy 回合槽占用位 + claimGen 占用代号：handleCreate 的 IsRunning
	// 预检与 beginTurn 登记之间隔着请求级配置、input 解析与斜杠指令
	// （/compact 可达秒级），tryClaim 原子完成"检查+占用"消除 TOCTOU——
	// 漏网时 409 退化为 200+failed 且推进链尾。释放按代号匹配，防止
	// 误清下一个请求刚拿到的占用（本请求 endTurn 之后、兜底 defer 之前）。
	busy     bool
	claimGen uint64
	cancel   context.CancelFunc // 当前回合取消（nil = 空闲）
	curSeq   int                // 最新已分配的 response 序号
	seq      atomic.Int64       // 会话级 sequence_number 发号器（ORS 事件排序契约）
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
	// loadLocks sid → 懒加载锁：同一会话的并发首触达在此串行（防
	// double-load），跨会话互不阻塞。常驻不清理——与会话数同量级、且
	// 删除后并发重建会导致双重加载。s.mu 只护 sessions/loadLocks 命中
	// 查询，不做磁盘 IO。
	loadLocks map[string]*sync.Mutex

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
		loadLocks:     map[string]*sync.Mutex{},
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
	return llm.NewClient(llm.Config{Provider: eff.Provider, BaseURL: eff.BaseURL, APIKey: eff.APIKey, Model: eff.Model}), nil
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
	if err := os.MkdirAll(filepath.Join(dir, "responses"), 0o755); err != nil {
		return nil, err
	}
	ws := filepath.Join(dir, "workspace")
	if cfg != nil && cfg.Workspace != "" {
		if abs, err := filepath.Abs(cfg.Workspace); err == nil {
			if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
				ws = abs
			}
		}
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
		meta.DefaultSkill = cfg.DefaultSkill
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
	// 存档根目录按会话隔离（conversations/<sid>/.grf/sessions）：新会话
	// 不再继承 workspace 里最新一次存档（跨会话串历史的根源），重启后
	// 也只恢复本会话自己的历史与暂停态。
	h, err := agent.Build(agent.Options{
		Workspace:     meta.Workspace,
		Client:        client,
		Settings:      settings,
		Host:          host,
		SystemPrompt:  meta.SystemPrompt,
		DefaultSkill:  meta.DefaultSkill,
		MaxSteps:      meta.MaxSteps,
		DisabledTools: meta.DisabledTools,
		StoreRoot:     s.convDir(meta.ID),
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
	// 恢复本会话最新存档（磁盘为准：重启后自动接上历史与暂停态）。
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
// 加载串行化按会话粒度（loadLocks）——buildSession 含磁盘 IO 与插件建连，
// 若在 s.mu 内做，一个大历史会话的首次触达会阻塞所有其他会话的内存
// 命中查询。
func (s *Server) getSession(sid string) (*session, error) {
	s.mu.Lock()
	if sess, ok := s.sessions[sid]; ok {
		s.mu.Unlock()
		if sess.meta.Archived {
			return nil, apiErr(errNotFound, "session is archived")
		}
		return sess, nil
	}
	ll := s.loadLocks[sid]
	if ll == nil {
		ll = &sync.Mutex{}
		s.loadLocks[sid] = ll
	}
	s.mu.Unlock()
	// 磁盘为准：目录存在即会话存在
	if _, err := os.Stat(s.convDir(sid)); err != nil {
		return nil, apiErr(errNotFound, "unknown session "+sid)
	}
	ll.Lock()
	defer ll.Unlock()
	// 拿到加载锁后复查：同会话的首个加载者可能刚刚完成
	s.mu.Lock()
	sess, ok := s.sessions[sid]
	s.mu.Unlock()
	if ok {
		if sess.meta.Archived {
			return nil, apiErr(errNotFound, "session is archived")
		}
		return sess, nil
	}
	meta, err := readMeta(s.convDir(sid))
	if err != nil {
		return nil, apiErr(errInvalid, "corrupt session meta: "+err.Error())
	}
	if meta.Archived {
		return nil, apiErr(errNotFound, "session is archived")
	}
	sess, err = s.buildSession(meta)
	if err != nil {
		return nil, apiErr(errServer, "load session: "+err.Error())
	}
	s.mu.Lock()
	s.sessions[sid] = sess
	s.mu.Unlock()
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

// WaitSettled 等待所有已加载会话的在跑回合落定：gh server 优雅退出时在
// 停止接收新请求后调用（调用方接线在根目录 server_cmd.go），避免进程
// 退出截断在跑回合的终态快照/存档写。d 为单个会话的等待期限（逐会话
// 串行等待，总耗时上界 n*d）；无在跑回合的会话立即跳过。先在 s.mu 内
// 取会话快照、锁外逐个等待——持锁等待会阻塞 getSession 等内存命中查询，
// 而等待中的回合可能正需要它们（如 cancel）推进。
func (s *Server) WaitSettled(d time.Duration) {
	s.mu.Lock()
	sessions := make([]*session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.mu.Unlock()
	for _, sess := range sessions {
		sess.waitTurn(d)
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

// maxSnapSeq 扫描快照目录求最大序号（懒加载时恢复链尾）；与列表端点的
// scanMaxSeq 同一实现，只是以会话自身目录为参。
func (sess *session) maxSnapSeq() int {
	return scanMaxSeq(sess.srv.convDir(sess.meta.ID))
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

// overrideHistory 用客户端回传的历史前缀替换会话历史（编辑重发的截断）。
// 逐条校验 raw 与当前历史语义一致（必须是前缀，防止伪造/漂移），替换后
// 立即落盘。content 兼容纯文本与多模态数组两种线格式。
func (sess *session) overrideHistory(raw []json.RawMessage) error {
	hist := sess.root.History()
	if len(raw) > len(hist) {
		return fmt.Errorf("长度 %d 超过当前历史 %d 条", len(raw), len(hist))
	}
	cut := make([]llm.Message, 0, len(raw))
	for i, r := range raw {
		cur, err := json.Marshal(hist[i])
		if err != nil {
			return fmt.Errorf("序列化当前历史第 %d 条: %w", i+1, err)
		}
		if !jsonSemanticallyEqual(cur, r) {
			return fmt.Errorf("第 %d 条与当前历史不一致（必须是当前历史的前缀）", i+1)
		}
		m, err := wireToMessage(r)
		if err != nil {
			return fmt.Errorf("解析第 %d 条: %w", i+1, err)
		}
		cut = append(cut, m)
	}
	sess.root.RestoreFull(cut, sess.root.SystemText())
	if _, err := sess.h.Store.SaveSession(cut); err != nil {
		return fmt.Errorf("落盘截断历史: %w", err)
	}
	return nil
}

// jsonSemanticallyEqual 比较两段 JSON 的语义等价（忽略转义/空白/键序）。
func jsonSemanticallyEqual(a, b []byte) bool {
	var va, vb any
	if err := json.Unmarshal(a, &va); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &vb); err != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}

// wireToMessage 把线格式消息（content 可为字符串或多模态数组）转为
// llm.Message——标准 Unmarshal 不接受 content 数组，这里手动分流。
func wireToMessage(raw json.RawMessage) (llm.Message, error) {
	var w struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		ToolCalls  []llm.ToolCall  `json:"tool_calls"`
		ToolCallID string          `json:"tool_call_id"`
		Name       string          `json:"name"`
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		return llm.Message{}, err
	}
	m := llm.Message{Role: w.Role, ToolCalls: w.ToolCalls, ToolCallID: w.ToolCallID, Name: w.Name}
	if len(w.Content) > 0 && w.Content[0] == '[' {
		var parts []llm.Part
		if err := json.Unmarshal(w.Content, &parts); err != nil {
			return llm.Message{}, fmt.Errorf("content 数组: %w", err)
		}
		m.ContentParts = parts
	} else if len(w.Content) > 0 {
		if err := json.Unmarshal(w.Content, &m.Content); err != nil {
			return llm.Message{}, fmt.Errorf("content 字符串: %w", err)
		}
	}
	return m, nil
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

// tryClaim 原子占用回合槽（每会话单 turn 的真保证），返回占用代号。
func (sess *session) tryClaim() (uint64, bool) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.busy {
		return 0, false
	}
	sess.busy = true
	sess.claimGen++
	return sess.claimGen, true
}

// releaseClaim 按代号释放回合槽：代号不匹配（槽已被后续回合重新占用）
// 则不动作。execute 的各出口与 handleCreate 的兜底 defer 都调用它。
func (sess *session) releaseClaim(gen uint64) {
	sess.mu.Lock()
	if sess.busy && sess.claimGen == gen {
		sess.busy = false
	}
	sess.mu.Unlock()
}

// beginTurn 登记回合取消句柄与结束信号（调用前须已 tryClaim 占用）。
func (sess *session) beginTurn(cancel context.CancelFunc) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	sess.cancel = cancel
	sess.turnDone = make(chan struct{})
}

// endTurn 回合收尾：清取消句柄、释放回合槽并广播结束。
func (sess *session) endTurn(gen uint64) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	sess.cancel = nil
	if sess.busy && sess.claimGen == gen {
		sess.busy = false
	}
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
