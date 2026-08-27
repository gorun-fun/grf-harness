package store

import (
	"os"
	"path/filepath"
	"testing"

	"git.gorun.fun/share/grf-harness/llm"
)

func TestSessionRoundTrip(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	msgs := []llm.Message{
		{Role: "system", Content: "system prompt（保存时应剔除）"},
		{Role: "user", Content: "帮我修个 bug"},
		{Role: "assistant", Content: "", Reasoning: "想一想", ToolCalls: []llm.ToolCall{
			{ID: "t1", Type: "function", Function: llm.FunctionCall{Name: "bash", Arguments: `{"cmd":"ls"}`}},
		}},
		{Role: "tool", ToolCallID: "t1", Name: "bash", Content: "file_a\nfile_b"},
		{Role: "assistant", Content: "目录里有两个文件"},
	}
	if _, err := s.SaveSession(msgs); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestSession()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("恢复消息数 = %d, 期望 4（system 被剔除）", len(got))
	}
	for i := range got {
		if got[i].Role != msgs[i+1].Role || got[i].Content != msgs[i+1].Content ||
			got[i].Reasoning != msgs[i+1].Reasoning || got[i].ToolCallID != msgs[i+1].ToolCallID ||
			got[i].Name != msgs[i+1].Name {
			t.Fatalf("消息 %d 回环不一致: %+v vs %+v", i, got[i], msgs[i+1])
		}
		if len(got[i].ToolCalls) != len(msgs[i+1].ToolCalls) {
			t.Fatalf("消息 %d ToolCalls 数不一致", i)
		}
	}
	// 列表标题取最后一条用户消息
	infos := s.ListSessions(0)
	if len(infos) != 1 || infos[0].Title != "帮我修个 bug" {
		t.Fatalf("ListSessions = %+v", infos)
	}
}

func TestLoadSessionMissing(t *testing.T) {
	msgs, err := LoadSession(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || msgs != nil {
		t.Fatalf("期望 (nil, nil), got (%v, %v)", msgs, err)
	}
}

// TestSessionMetaRoundTrip system 原文与缓存键随会话文件往返。
func TestSessionMetaRoundTrip(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	s.SetCacheKey("grf-key1")
	msgs := []llm.Message{
		{Role: "system", Content: "sys v1"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}
	path, err := s.SaveSession(msgs)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := LoadSessionFull(path)
	if err != nil {
		t.Fatal(err)
	}
	if sess.System != "sys v1" || sess.CacheKey != "grf-key1" {
		t.Fatalf("meta = (%q, %q), 期望 (sys v1, grf-key1)", sess.System, sess.CacheKey)
	}
	if len(sess.Messages) != 2 || sess.Messages[0].Role != "user" {
		t.Fatalf("messages = %+v（不应含 system）", sess.Messages)
	}
	// 旧版文件（无 system/cache_key 字段）回退为零值
	old := filepath.Join(root, ".grf", "sessions", "session-old.json")
	if err := os.WriteFile(old, []byte(`{"version":1,"messages":[{"role":"user","content":"x"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	sess2, err := LoadSessionFull(old)
	if err != nil || sess2.System != "" || sess2.CacheKey != "" || len(sess2.Messages) != 1 {
		t.Fatalf("旧版文件回退失败: %+v, %v", sess2, err)
	}
}
