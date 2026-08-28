// compact.go POST /v1/responses/compact：上下文压缩端点（ORS 2026-04-24）。
// 用当前模型对会话历史做一次总结，返回 CompactResource——注意返回的是
// 压缩后的输入窗口（不是 response id）：客户端须以它为 base input 开新
// 链（previous_response_id 省略），再追加新输入。compaction item 的
// encrypted_content 携带压缩窗口的原始 JSON（opaque，回传给 gh 即可无损
// 再水化）。
package server

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"git.gorun.fun/share/grf-harness/llm"
)

// compactRequest compact 端点请求体（model 必填，缺失即 400——ORS 合规）。
type compactRequest struct {
	Model              string          `json:"model"`
	Input              json.RawMessage `json:"input"`
	PreviousResponseID string          `json:"previous_response_id"`
	Instructions       *string         `json:"instructions"`
	PromptCacheKey     string          `json:"prompt_cache_key"`
}

func (s *Server) handleCompact(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeError(w, 400, errInvalid, "read body: "+err.Error(), "")
		return
	}
	var req compactRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, 400, errInvalid, "invalid JSON body: "+err.Error(), "")
		return
	}
	if req.Model == "" {
		writeError(w, 400, errInvalid, "model is required for compaction", "model")
		return
	}

	// 素材：previous_response_id 定位会话取历史；或直接压缩请求 input。
	var transcript string
	if req.PreviousResponseID != "" {
		sid, _, err := parseRespID(req.PreviousResponseID)
		if err != nil {
			writeError(w, 400, errInvalid, "malformed previous_response_id", "previous_response_id")
			return
		}
		sess, err := s.getSession(sid)
		if err != nil {
			writeJSONError(w, err)
			return
		}
		for _, m := range sess.root.History() {
			if m.Role == "system" {
				continue
			}
			transcript += m.Role + ": " + m.String() + "\n"
		}
	} else if len(req.Input) > 0 {
		transcript = string(req.Input)
	} else {
		writeError(w, 400, errInvalid, "either previous_response_id or input is required for compaction", "")
		return
	}

	client, err := s.clientFactory()
	if err != nil {
		writeError(w, 500, errServer, "build client: "+err.Error(), "")
		return
	}
	pre := client.Stats()
	summary, err := client.Chat(r.Context(), []llm.Message{
		{Role: "system", Content: "You compact conversation transcripts. Produce a faithful, information-dense summary in the original language, preserving: task state, decisions made, file paths, commands run and their outcomes, open questions, and the next step. Output only the summary."},
		{Role: "user", Content: transcript},
	}, nil)
	post := client.Stats()
	if err != nil {
		writeError(w, 500, errModel, "compaction model call failed: "+err.Error(), "")
		return
	}

	window, _ := json.Marshal(map[string]any{
		"summary":   summary.Content,
		"compactAt": time.Now().UTC().Format(time.RFC3339),
	})
	res := &CompactResource{
		ID:        "cmp_" + newSID(),
		Object:    "response.compaction",
		CreatedAt: time.Now().Unix(),
		Output: []Item{
			{
				Type: itemMessage, ID: "it_1", Status: "completed", Role: "user",
				Content: []contentPart{{Type: "input_text", Text: summary.Content}},
			},
			{
				Type: itemCompaction, ID: "it_2", Status: "completed",
				EncryptedContent: base64.StdEncoding.EncodeToString(window),
			},
		},
		Usage: usageFrom(post, pre),
	}
	writeJSON(w, 200, res)
}
