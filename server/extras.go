// extras.go OpenAI 形状超集端点：retrieve（断线恢复/回合蒸发检测）、
// cancel（中断在跑回合）、conversations（会话列表/详情/归档/历史）。
// 这些端点超出 ORS 散文但属于"proper superset"允许的增量（OpenAI
// Responses/Conversations API 已有形状，客户端生态熟悉）。
package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"git.gorun.fun/share/grf-harness/llm"
)

// handleRetrieve GET /v1/responses/{id}：读终态快照；在跑回合返回进行中
// 资源；404 = 回合蒸发（server 重启杀掉了在跑回合，无终态）。
func (s *Server) handleRetrieve(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sid, seq, err := parseRespID(id)
	if err != nil {
		writeError(w, 400, errInvalid, "malformed response id", "")
		return
	}
	sess, err := s.getSession(sid)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	if snap, err := sess.loadSnap(seq); err == nil {
		writeJSON(w, 200, snap)
		return
	}
	if sess.root.IsRunning() && sess.curRespID() == id {
		writeJSON(w, 200, map[string]any{
			"id": id, "object": "response", "created_at": time.Now().Unix(),
			"status": statusInProgress, "output": []Item{}, "grf:session_id": sid,
		})
		return
	}
	writeError(w, 404, errNotFound, "response not found (it was never completed, or the server restarted while it was running)", "")
}

// handleCancel POST /v1/responses/{id}/cancel：取消在跑回合（幂等）。
func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sid, seq, err := parseRespID(id)
	if err != nil {
		writeError(w, 400, errInvalid, "malformed response id", "")
		return
	}
	sess, err := s.getSession(sid)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	if sess.curRespID() != id {
		writeError(w, 404, errNotFound, "response id is not the current turn of this session", "")
		return
	}
	running := sess.cancelTurn()
	if running {
		sess.waitTurn(5 * time.Second) // 等执行路径落终态快照
	}
	if snap, err := sess.loadSnap(seq); err == nil {
		writeJSON(w, 200, snap)
		return
	}
	// 快照尚未落盘（store=false 或竞态）：返回即时构造的终态。
	now := time.Now().Unix()
	writeJSON(w, 200, &ResponseResource{
		ID: id, Object: "response", CreatedAt: now, CompletedAt: &now,
		Status: statusCancelled, Output: []Item{}, SessionID: sid,
	})
}

// conversationDTO conversations 端点的会话表示。
type conversationDTO struct {
	ID             string    `json:"id"`
	Object         string    `json:"object"` // "conversation"
	CreatedAt      time.Time `json:"created_at"`
	Archived       bool      `json:"archived,omitempty"`
	Workspace      string    `json:"workspace,omitempty"`
	LastResponseID string    `json:"last_response_id,omitempty"`
}

func toConversationDTO(meta *sessionMeta, lastResp string) conversationDTO {
	return conversationDTO{
		ID: meta.ID, Object: "conversation", CreatedAt: meta.CreatedAt,
		Archived: meta.Archived, Workspace: meta.Workspace, LastResponseID: lastResp,
	}
}

// handleListConversations GET /v1/conversations。
func (s *Server) handleListConversations(w http.ResponseWriter, r *http.Request) {
	metas := s.listConversations()
	out := make([]conversationDTO, 0, len(metas))
	for _, m := range metas {
		last := ""
		if maxSeq := scanMaxSeq(s.convDir(m.ID)); maxSeq > 0 {
			last = respID(m.ID, maxSeq)
		}
		out = append(out, toConversationDTO(m, last))
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": out})
}

// scanMaxSeq 会话目录下最大快照序号（不加载会话）。
func scanMaxSeq(dir string) int {
	entries, err := os.ReadDir(filepath.Join(dir, "responses"))
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

// handleGetConversation GET /v1/conversations/{id}。
func (s *Server) handleGetConversation(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("id")
	sess, err := s.getSession(sid)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	dto := toConversationDTO(sess.meta, sess.curRespID())
	calls, dur := sess.root.ToolStats()
	writeJSON(w, 200, map[string]any{
		"id": dto.ID, "object": "conversation", "created_at": dto.CreatedAt,
		"workspace": dto.Workspace, "last_response_id": dto.LastResponseID,
		"paused":  sess.root.HasPausedCalls(),
		"running": sess.root.IsRunning(),
		"stats": map[string]any{
			"llm":   sess.root.LLMStats(),
			"tools": map[string]any{"calls": calls, "time_ms": dur.Milliseconds()},
		},
	})
}

// handleArchiveConversation DELETE /v1/conversations/{id}：归档（终止在跑
// 回合 → 存档 → 标记；目录与 workspace 保留，agent 写过的文件不删）。
func (s *Server) handleArchiveConversation(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("id")
	sess, err := s.getSession(sid)
	if err != nil {
		writeJSONError(w, err)
		return
	}
	if sess.root.IsRunning() {
		sess.cancelTurn()
		sess.waitTurn(5 * time.Second)
	}
	sess.h.Store.SetCacheKey(sess.h.CacheKey())
	_, _ = sess.h.Store.SaveSession(sess.root.History())
	sess.meta.Archived = true
	_ = writeMeta(s.convDir(sid), sess.meta)
	s.dropSession(sid)
	w.WriteHeader(204)
}

// handleConversationItems GET /v1/conversations/{id}/items：完整消息历史
// （llm.Message 线格式，含 system 首条与工具消息）。
func (s *Server) handleConversationItems(w http.ResponseWriter, r *http.Request) {
	sess, err := s.getSession(r.PathValue("id"))
	if err != nil {
		writeJSONError(w, err)
		return
	}
	hist := sess.root.History()
	out := make([]llm.Message, 0, len(hist))
	out = append(out, hist...)
	writeJSON(w, 200, map[string]any{"object": "list", "data": out})
}
