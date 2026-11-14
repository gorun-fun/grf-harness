// client.go 远端节点客户端：任务面完全复用既有 ORS 端点（POST
// /v1/responses、GET /v1/responses/{id}、cancel），控制面走 /cluster/*。
// 本文件只声明路由层消费的最小 wire 子集（ResponseResource 二十余字段的
// 并集裁剪），不引 server 包（避免循环依赖：server→agent→cluster）。
package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// 响应状态机（与 server 包同值；ORS/OpenAI 六值枚举的子集）。
const (
	statusQueued     = "queued"
	statusInProgress = "in_progress"
)

// NodeClient 一个远端节点的 HTTP 客户端。
type NodeClient struct {
	Base  string // 基地址（含 scheme，不带尾斜杠）
	Token string // 节点的 server 访问令牌（call-home 场景为集群共享密钥）
	HTTP  *http.Client
}

// NewNodeClient 构建客户端；addr 无 scheme 时补 http://。
func NewNodeClient(addr, token string) *NodeClient {
	if addr == "" {
		addr = "http://127.0.0.1"
	}
	if !hasScheme(addr) {
		addr = "http://" + addr
	}
	for len(addr) > 0 && addr[len(addr)-1] == '/' {
		addr = addr[:len(addr)-1]
	}
	return &NodeClient{Base: addr, Token: token, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

func hasScheme(addr string) bool {
	return len(addr) > 7 && (addr[:7] == "http://" || (len(addr) > 8 && addr[:8] == "https://"))
}

// Response ORS ResponseResource 的消费子集。
type Response struct {
	ID                string             `json:"id"`
	Status            string             `json:"status"`
	IncompleteDetails *IncompleteDetails `json:"incomplete_details,omitempty"`
	Error             *RespError         `json:"error,omitempty"`
	Output            []OutputItem       `json:"output,omitempty"`
}

// IncompleteDetails 未完成原因（grf:input_required 时带交互请求索引）。
type IncompleteDetails struct {
	Reason   string              `json:"reason"`
	Requests []IncompleteRequest `json:"grf_requests,omitempty"`
}

// IncompleteRequest 一个待应答的交互请求。
type IncompleteRequest struct {
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"` // function | tool_approval | question
	Arguments string `json:"arguments"`
}

// RespError 失败终态的错误对象。
type RespError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// OutputItem 输出 item（仅 message 家族；最终文本从中提取）。
type OutputItem struct {
	Type    string        `json:"type"`
	Role    string        `json:"role,omitempty"`
	Content []ContentPart `json:"content,omitempty"`
}

// ContentPart 内容部件。
type ContentPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// FinalText 提取 assistant 消息的最终文本（output_text 拼接）。
func (r *Response) FinalText() string {
	var texts []string
	for _, it := range r.Output {
		if it.Type != "message" || it.Role != "assistant" {
			continue
		}
		for _, p := range it.Content {
			if p.Type == "output_text" && p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
	}
	if len(texts) == 0 {
		return ""
	}
	out := texts[0]
	for _, t := range texts[1:] {
		out += "\n" + t
	}
	return out
}

// Incomplete 暂停应答回投的输入 item（grf:input_result）。
type InputResultItem struct {
	Type     string         `json:"type"` // "grf:input_result"
	CallID   string         `json:"call_id"`
	Answers  map[string]any `json:"answers,omitempty"`
	Approved bool           `json:"approved,omitempty"`
	Feedback string         `json:"feedback,omitempty"`
	Dismiss  bool           `json:"dismiss,omitempty"`
}

// RegisterRequest /cluster/register 请求体（call-home 节点侧发）。
type RegisterRequest struct {
	NodeID       string        `json:"node_id"`
	Name         string        `json:"name"`
	Addr         string        `json:"addr,omitempty"`
	Capabilities *Capabilities `json:"capabilities,omitempty"`
}

// HeartbeatRequest /cluster/heartbeat 请求体。
type HeartbeatRequest struct {
	NodeID  string `json:"node_id"`
	Name    string `json:"name"`
	Running int    `json:"running_turns"`
}

// call 单次 JSON POST/GET 封装：Bearer 头、状态码检查、错误体解包。
func (c *NodeClient) call(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rd)
	if err != nil {
		return err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		var eb struct {
			Error RespError `json:"error"`
		}
		if json.Unmarshal(data, &eb) == nil && eb.Error.Message != "" {
			return fmt.Errorf("%d %s: %s", resp.StatusCode, eb.Error.Type, eb.Error.Message)
		}
		return fmt.Errorf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Capabilities 拉取节点能力清单。
func (c *NodeClient) Capabilities(ctx context.Context) (*Capabilities, error) {
	var caps Capabilities
	if err := c.call(ctx, http.MethodGet, "/cluster/capabilities", nil, &caps); err != nil {
		return nil, err
	}
	return &caps, nil
}

// CreateTask 派发任务（新回合或续聊）：POST /v1/responses，background
// 模式立即返回 queued。prevID 非空即链式续聊（send_message 语义）。
func (c *NodeClient) CreateTask(ctx context.Context, input, prevID, policy, hostCtx string) (*Response, error) {
	body := map[string]any{
		"input":      input,
		"background": true, // 轮询回收（GET retrieve），不占长连接
	}
	if prevID != "" {
		body["previous_response_id"] = prevID
	}
	sess := map[string]string{}
	if policy != "" {
		sess["policy"] = policy
	}
	if hostCtx != "" {
		sess["host_context"] = hostCtx
	}
	if len(sess) > 0 {
		body["grf:session"] = sess
	}
	var out Response
	if err := c.call(ctx, http.MethodPost, "/v1/responses", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetResponse 读取回合状态：终态快照 / 进行中占位 / 404 蒸发（重启杀回合）。
func (c *NodeClient) GetResponse(ctx context.Context, id string) (*Response, error) {
	var out Response
	if err := c.call(ctx, http.MethodGet, "/v1/responses/"+id, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Resume 提交暂停应答（grf:input_result items，链式新回合）。
func (c *NodeClient) Resume(ctx context.Context, prevID string, items []InputResultItem) (*Response, error) {
	body := map[string]any{
		"input":                items,
		"background":           true,
		"previous_response_id": prevID,
	}
	var out Response
	if err := c.call(ctx, http.MethodPost, "/v1/responses", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Cancel 取消在跑回合（幂等；终态回合同样 200）。
func (c *NodeClient) Cancel(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodPost, "/v1/responses/"+id+"/cancel", nil, nil)
}

// Register call-home 注册/复活。
func (c *NodeClient) Register(ctx context.Context, req RegisterRequest) error {
	return c.call(ctx, http.MethodPost, "/cluster/register", req, nil)
}

// Heartbeat call-home 心跳。
func (c *NodeClient) Heartbeat(ctx context.Context, req HeartbeatRequest) error {
	return c.call(ctx, http.MethodPost, "/cluster/heartbeat", req, nil)
}

// Members 拉取指挥官成员表快照。
func (c *NodeClient) Members(ctx context.Context) ([]Member, error) {
	var out []Member
	if err := c.call(ctx, http.MethodGet, "/cluster/members", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}
