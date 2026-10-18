// Package store 会话持久化：.grf/sessions/ 下的 JSON 会话记录
// （结构化消息数组 + system 提示词原文 + 缓存路由键，resume 时原样回放，
// 保证请求前缀与上一场逐字一致、提示词缓存可命中），供 -resume /
// /resume 恢复上下文。
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/llm"
	"git.gorun.fun/share/grf-harness/term"
)

type Store struct {
	root     string
	mu       sync.Mutex
	cur      string // 当前会话文件路径；自动保存复用，新会话另起
	cacheKey string // 写入会话文件的缓存路由键（由上层同步）
}

func New(root string) *Store { return &Store{root: root} }

func (s *Store) dir() string { return filepath.Join(s.root, ".grf", "sessions") }

// sessionFile 会话落盘格式。msgJSON 是 llm.Message 的持久化镜像：
// 独立于 llm.Message 的请求序列化（MarshalJSON 会改写多模态 content），
// 额外保存 ContentParts 与 Reasoning 供恢复后展示。System 是构建期
// system 提示词原文——resume 优先回放它，保证请求前缀与上一场逐字一致
// （提示词代码/运行时事实变化也不砸缓存）。CacheKey 是 prompt_cache_key，
// 续接同一会话时复用，服务端可路由到同一缓存分片。
type sessionFile struct {
	Version  int       `json:"version"`
	System   string    `json:"system,omitempty"`
	CacheKey string    `json:"cache_key,omitempty"`
	Messages []msgJSON `json:"messages"`
}

type msgJSON struct {
	Role         string         `json:"role"`
	Content      string         `json:"content,omitempty"`
	ContentParts []llm.Part     `json:"content_parts,omitempty"`
	Reasoning    string         `json:"reasoning,omitempty"`
	ToolCalls    []llm.ToolCall `json:"tool_calls,omitempty"`
	ToolCallID   string         `json:"tool_call_id,omitempty"`
	Name         string         `json:"name,omitempty"`
}

func toMsgJSON(m llm.Message) msgJSON {
	return msgJSON{
		Role: m.Role, Content: m.Content, ContentParts: m.ContentParts,
		Reasoning: m.Reasoning, ToolCalls: m.ToolCalls,
		ToolCallID: m.ToolCallID, Name: m.Name,
	}
}

func fromMsgJSON(m msgJSON) llm.Message {
	return llm.Message{
		Role: m.Role, Content: m.Content, ContentParts: m.ContentParts,
		Reasoning: m.Reasoning, ToolCalls: m.ToolCalls,
		ToolCallID: m.ToolCallID, Name: m.Name,
	}
}

// SaveSession 保存当前会话（结构化消息），返回文件路径。
// 同一会话内多次保存复用同一文件（自动保存每回合覆写），新会话另起新文件。
func (s *Store) SaveSession(msgs []llm.Message) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != "" {
		if err := writeSession(s.cur, msgs, s.cacheKey); err != nil {
			return "", err
		}
		return s.cur, nil
	}
	dir := s.dir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("session-%s.json", time.Now().Format("20060102-150405"))
	s.cur = filepath.Join(dir, name)
	if err := writeSession(s.cur, msgs, s.cacheKey); err != nil {
		s.cur = ""
		return "", err
	}
	return s.cur, nil
}

// ResetCurrent 下次保存另起新文件（新会话 / 恢复历史会话后调用）。
func (s *Store) ResetCurrent() {
	s.mu.Lock()
	s.cur = ""
	s.mu.Unlock()
}

// SetCacheKey 设置随会话文件落盘的缓存路由键。
func (s *Store) SetCacheKey(key string) {
	s.mu.Lock()
	s.cacheKey = key
	s.mu.Unlock()
}

// writeSession 序列化落盘。system 提示词原文存入文件头部字段（消息数组
// 仍不含 system——resume 时由 agent 重建头部）。
func writeSession(path string, msgs []llm.Message, cacheKey string) error {
	system := ""
	kept := make([]msgJSON, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == "system" {
			system = m.Content
			continue
		}
		kept = append(kept, toMsgJSON(m))
	}
	data, err := json.MarshalIndent(sessionFile{Version: 1, System: system,
		CacheKey: cacheKey, Messages: kept}, "", " ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(path, data, 0o644)
}

// Session 一场会话的完整恢复材料：消息（不含 system）+ system 提示词
// 原文 + 缓存路由键（旧版文件后两者可为空）。
type Session struct {
	Messages []llm.Message
	System   string
	CacheKey string
}

// LoadSessionFull 读回一场会话的完整恢复材料；文件不存在或为空返回
// 零值 Session。
func LoadSessionFull(path string) (Session, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Session{}, nil
		}
		return Session{}, err
	}
	var f sessionFile
	if err := json.Unmarshal(data, &f); err != nil {
		return Session{}, fmt.Errorf("解析会话 %s: %w", path, err)
	}
	if len(f.Messages) == 0 {
		return Session{}, nil
	}
	msgs := make([]llm.Message, 0, len(f.Messages))
	for _, m := range f.Messages {
		msgs = append(msgs, fromMsgJSON(m))
	}
	return Session{Messages: msgs, System: f.System, CacheKey: f.CacheKey}, nil
}

// LoadSession 读回一次会话的消息（仅消息，丢弃 system/cache 元数据）。
func LoadSession(path string) ([]llm.Message, error) {
	s, err := LoadSessionFull(path)
	return s.Messages, err
}

// LatestSessionFull 最近一次会话的完整恢复材料；无会话返回零值。
func (s *Store) LatestSessionFull() (Session, error) {
	path := s.latestPath()
	if path == "" {
		return Session{}, nil
	}
	return LoadSessionFull(path)
}

// latestPath 最新会话文件路径；无返回空串。
func (s *Store) latestPath() string {
	entries, err := os.ReadDir(s.dir())
	if err != nil {
		return ""
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	return filepath.Join(s.dir(), names[len(names)-1])
}

// SessionInfo 历史会话索引（/resume 列表用）。
type SessionInfo struct {
	Name  string
	Path  string
	Title string
	When  time.Time
}

// ListSessions 按时间倒序返回历史会话；n>0 时最多 n 条。
func (s *Store) ListSessions(n int) []SessionInfo {
	entries, err := os.ReadDir(s.dir())
	if err != nil {
		return nil
	}
	var infos []SessionInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(s.dir(), e.Name())
		info := SessionInfo{Name: e.Name(), Path: path, Title: e.Name()}
		if msgs, err := LoadSession(path); err == nil {
			info.Title = sessionTitle(msgs)
		}
		if info.Title == "" {
			info.Title = e.Name()
		}
		if t, err := time.ParseInLocation("20060102-150405",
			strings.TrimSuffix(strings.TrimPrefix(e.Name(), "session-"), ".json"), time.Local); err == nil {
			info.When = t
		}
		infos = append(infos, info)
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name > infos[j].Name })
	if n > 0 && len(infos) > n {
		infos = infos[:n]
	}
	return infos
}

// sessionTitle 取会话中最后一条用户消息的首个非空行作标题（60 字截断）。
func sessionTitle(msgs []llm.Message) string {
	last := ""
	for _, m := range msgs {
		if m.Role != "user" || strings.HasPrefix(m.Content, "[system notice]") {
			continue
		}
		if t := term.FirstLine(m.Content, 0); t != "" {
			last = t
		}
	}
	return term.FirstLine(last, 60)
}
