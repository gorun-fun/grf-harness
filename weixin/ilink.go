// Package weixin 微信 ClawBot(腾讯官方 iLink Bot API)客户端:
// 扫码登录 + 长轮询收消息,配合 grf 的 LLM 配置回复微信消息。
package weixin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// 协议参考: https://ilinkai.weixin.qq.com 下的 iLink Bot API(微信 ClawBot 插件)。

const (
	defaultBaseURL        = "https://ilinkai.weixin.qq.com"
	channelVersion        = "2.4.6"
	ilinkAppID            = "bot"
	ilinkAppClientVersion = "132102"
	botAgent              = "grf-harness/1.0.0 (go)"
	staleTokenCode  int64 = -14 // ret/errcode=-14 表示 token 失效,需重新扫码
)

// APIError iLink 业务错误(HTTP 200 也可能携带业务错误码)。
type APIError struct {
	Method  string
	Path    string
	HTTP    int
	Ret     *int64
	Errcode *int64
	Msg     string
	Raw     string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s %s HTTP %d ret=%v errcode=%v: %s", e.Method, e.Path, e.HTTP, e.Ret, e.Errcode, e.Msg)
}

// IsStaleToken token 失效,调用方应停止紧密轮询并重新扫码登录。
func (e *APIError) IsStaleToken() bool {
	return (e.Ret != nil && *e.Ret == staleTokenCode) || (e.Errcode != nil && *e.Errcode == staleTokenCode)
}

// Client iLink Bot API 客户端。Token/BaseURL 登录成功后由调用方更新。
type Client struct {
	HTTP    *http.Client
	Token   string
	BaseURL string
}

func NewClient() *Client {
	return &Client{
		HTTP:    &http.Client{Timeout: 60 * time.Second},
		BaseURL: defaultBaseURL,
	}
}

// randomUin 每次请求生成随机 uint32 十进制字符串的 base64(防重放)。
func randomUin() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%d", binary.BigEndian.Uint32(b[:]))))
}

func baseInfo() map[string]any {
	return map[string]any{
		"channel_version": channelVersion,
		"bot_agent":       botAgent,
	}
}

// GenerateClientID 生成与官方客户端格式一致、用于消息幂等的唯一 ID。
func GenerateClientID() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(0xffff))
	return fmt.Sprintf("openclaw-weixin:%d-%04x", time.Now().UnixMilli(), n.Int64())
}

func (c *Client) url(path string) string {
	return strings.TrimRight(c.BaseURL, "/") + "/" + strings.TrimLeft(path, "/")
}

// do 执行请求并统一解析:HTTP 非 2xx、JSON 解析失败、业务码非 0 均返回 *APIError。
func (c *Client) do(ctx context.Context, method, path string, body any, withToken bool, timeout time.Duration) (map[string]any, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		bs, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(bs)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, c.url(path), reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("AuthorizationType", "ilink_bot_token")
	req.Header.Set("X-WECHAT-UIN", randomUin())
	req.Header.Set("iLink-App-Id", ilinkAppID)
	req.Header.Set("iLink-App-ClientVersion", ilinkAppClientVersion)
	if withToken && c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))

	apiErr := &APIError{Method: method, Path: path, HTTP: resp.StatusCode, Raw: string(raw)}

	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		apiErr.Msg = "响应不是合法 JSON"
		return nil, apiErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr.Msg = str(data["errmsg"])
		apiErr.Ret = i64p(data["ret"])
		apiErr.Errcode = i64p(data["errcode"])
		return nil, apiErr
	}
	// HTTP 200 ≠ 成功,必须校验业务返回码(ret / errcode)。
	for _, key := range []string{"ret", "errcode"} {
		if v := i64p(data[key]); v != nil {
			if *v != 0 {
				apiErr.Ret = i64p(data["ret"])
				apiErr.Errcode = i64p(data["errcode"])
				apiErr.Msg = str(data["errmsg"])
				return nil, apiErr
			}
			break
		}
	}
	return data, nil
}

func (c *Client) Get(ctx context.Context, path string, timeout time.Duration) (map[string]any, error) {
	return c.do(ctx, http.MethodGet, path, nil, false, timeout)
}

func (c *Client) Post(ctx context.Context, path string, body any) (map[string]any, error) {
	return c.do(ctx, http.MethodPost, path, body, true, 60*time.Second)
}

func (c *Client) PostNoToken(ctx context.Context, path string, body any) (map[string]any, error) {
	return c.do(ctx, http.MethodPost, path, body, false, 60*time.Second)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func i64p(v any) *int64 {
	f, ok := v.(float64)
	if !ok {
		return nil
	}
	n := int64(f)
	return &n
}
