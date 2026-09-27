// image_degrade_test.go 模型端点拒绝图片输入（400 image_url）时的自愈
// 回归：历史中的图片 parts 应被降级为占位文本并重试成功，而不是反复失败。
package agent

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/llm"
	"git.gorun.fun/share/grf-harness/tools"
)

func TestImageRejectDegradesAndRecovers(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// ContentParts 不参与服务端解码，直接在原始请求体里找 image_url
		hasImage := strings.Contains(string(body), "image_url")
		if atomic.AddInt32(&calls, 1) == 1 && hasImage {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"InvalidParameter","message":"Model do not support image input","param":"image_url"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": llm.Message{Role: "assistant", Content: "recovered"}}},
		})
	}))
	defer srv.Close()

	h, err := Build(Options{
		Workspace: t.TempDir(),
		Client:    llm.NewClient(llm.Config{BaseURL: srv.URL, APIKey: "t", Model: "m"}),
		Settings:  &config.Settings{Policy: "write-workspace"},
		Host:      &tools.NoopHost{},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 预置带图片的历史（模拟截图工具结果已并入）
	h.Root.RestoreFull([]llm.Message{
		{Role: "user", Content: "看图"},
		{Role: "user", ContentParts: []llm.Part{llm.TextPart("[tool result: read_image] ok"), llm.ImagePart("image/png", "aGk=")}},
	}, "")

	out, err := h.Root.Run(t.Context(), "继续")
	if err != nil {
		t.Fatalf("应自愈成功: %v", err)
	}
	if out != "recovered" {
		t.Fatalf("回复 = %q", out)
	}
	if calls != 2 {
		t.Fatalf("期望恰好两次请求（失败+降级重试），实际 %d", calls)
	}
	for _, m := range h.Root.History() {
		for _, p := range m.ContentParts {
			if p.Type == "image_url" {
				t.Fatal("历史中的图片未被降级")
			}
		}
	}
}
