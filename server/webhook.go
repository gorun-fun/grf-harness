// webhook.go — 回合终态事件外发（可选，嵌入方无头编排用）。
//
// GRF_TURN_WEBHOOK_URL 配置后，后台（background:true）回合落定即 POST
// JSON 给宿主：{type:"turn.settled", session_id, response_id, status,
// model, usage:{...}, ts}。宿主据此把"轮询 GET /v1/responses/{id}"降级为
// 事件驱动的收尾（轮询仍可保留作兜底）。同步/SSE 回合不外发——调用方
// 本就在请求生命周期内拿终态。
//
// GRF_TURN_WEBHOOK_SECRET 配置时附 HMAC-SHA256 签名头
// X-GRF-Signature: sha256=<hex(body)>，接收方应以常量时间比较校验后再
// 信任事件体。投递尽力而为：5s 超时、失败仅记日志、不重试（幂等键是
// response_id，丢了事件还有轮询兜底）。
package server

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"
)

// turnWebhook 回合终态事件外发配置。
type turnWebhook struct {
	url    string
	secret string
}

// turnWebhookFromEnv 从环境变量读取（gh server 启动期一次性；宿主网关
// 类集成经子进程 env 注入，见 docs/server.md §8.5）。
func turnWebhookFromEnv() turnWebhook {
	return turnWebhook{
		url:    os.Getenv("GRF_TURN_WEBHOOK_URL"),
		secret: os.Getenv("GRF_TURN_WEBHOOK_SECRET"),
	}
}

func (w turnWebhook) enabled() bool { return w.url != "" }

// postTurn 异步投递回合终态事件（goroutine，绝不阻塞回合收尾）。
func (w turnWebhook) postTurn(base *ResponseResource) {
	if !w.enabled() || base == nil {
		return
	}
	usage := map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
	if base.Usage != nil {
		usage = map[string]any{
			"input_tokens":  base.Usage.InputTokens,
			"output_tokens": base.Usage.OutputTokens,
			"total_tokens":  base.Usage.TotalTokens,
		}
	}
	payload := map[string]any{
		"type":        "turn.settled",
		"session_id":  base.SessionID,
		"response_id": base.ID,
		"status":      base.Status,
		"model":       base.Model,
		"usage":       usage,
		"ts":          time.Now().UTC().Format(time.RFC3339),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	go func() {
		req, err := http.NewRequest(http.MethodPost, w.url, bytes.NewReader(raw))
		if err != nil {
			log.Printf("turn webhook 构建请求失败: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if w.secret != "" {
			mac := hmac.New(sha256.New, []byte(w.secret))
			_, _ = mac.Write(raw)
			req.Header.Set("X-GRF-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		}
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("turn webhook 投递失败(%s): %v", base.ID, err)
			return
		}
		_ = resp.Body.Close()
	}()
}
