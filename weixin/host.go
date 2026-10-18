// host.go 微信侧工具宿主:把 ask_user_question 的提问(问题+选项)转发到
// 微信对话框,用该用户的下一条消息作答;审批类回调沿用 Noop 语义。
package weixin

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"git.gorun.fun/share/grf-harness/tools"
)

// askTimeout 单个问题等待用户微信回复的上限;超时让 agent 自行假设继续。
const askTimeout = 15 * time.Minute

// weixinHost 每个微信用户一个。contextToken 随轮次更新(同一用户串行
// 处理,写入只发生在轮次开始,读取发生在轮次内,无竞态)。
type weixinHost struct {
	tools.NoopHost // Approve*/NotifyEvent 的非交互默认行为
	bot    *Client
	mux    *userMux
	pool   *agentPool
	fromID string

	mu    sync.Mutex
	token string // 当前轮次的 context_token,回复问题用它关联对话
}

// setTurnToken 每轮开始前更新回复凭据。
func (h *weixinHost) setTurnToken(t string) {
	h.mu.Lock()
	h.token = t
	h.mu.Unlock()
}

// AskUser 逐问发送到微信并等待回复。答案:选项题匹配编号/选项文字,
// 多选按分隔符拆分;自由题原样返回文本。
func (h *weixinHost) AskUser(ctx context.Context, qs []tools.Question) (map[string]any, error) {
	answers := map[string]any{}
	for _, q := range qs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ans, err := h.askOne(ctx, q)
		if err != nil {
			return nil, err
		}
		answers[q.ID] = ans
	}
	return answers, nil
}

// askOne 先登记应答通道(避免用户秒回时回复被并入普通批次),再发问题,
// 然后等该用户的下一条消息。无文本的回复(如纯图片)跳过继续等。
func (h *weixinHost) askOne(ctx context.Context, q tools.Question) (any, error) {
	ch := h.mux.beginAsk(h.fromID)
	defer h.mux.endAsk(h.fromID)

	h.mu.Lock()
	token := h.token
	h.mu.Unlock()
	if err := h.bot.SendMessage(ctx, h.fromID, token, formatQuestion(q)); err != nil {
		return nil, fmt.Errorf("发送问题到微信失败: %w", err)
	}
	fmt.Printf("❓ 已发起到微信的提问: %s\n", q.Question)

	qctx, cancel := context.WithTimeout(ctx, askTimeout)
	defer cancel()
	for {
		var p *inboundPiece
		select {
		case p = <-ch:
		case <-qctx.Done():
			return nil, fmt.Errorf("等待用户回复超时(%s),请自行做合理假设继续", askTimeout)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		text := strings.TrimSpace(p.text)
		if text == "" {
			continue // 纯图片等无文本回复,继续等待
		}
		if text == "/new" || text == "新会话" {
			h.pool.drop(h.fromID)
			return nil, fmt.Errorf("用户开启了新会话,当前任务取消")
		}
		fmt.Printf("💬 微信回复提问: %s\n", text)
		return matchAnswer(q, text), nil
	}
}

// formatQuestion 把问题渲染成微信文本(编号列表 + 作答提示)。
func formatQuestion(q tools.Question) string {
	var b strings.Builder
	b.WriteString("❓ ")
	b.WriteString(q.Question)
	b.WriteString("\n")
	for i, opt := range q.Options {
		if opt.Description != "" {
			fmt.Fprintf(&b, "%d. %s — %s\n", i+1, opt.Label, opt.Description)
		} else {
			fmt.Fprintf(&b, "%d. %s\n", i+1, opt.Label)
		}
	}
	switch {
	case len(q.Options) == 0:
		b.WriteString("请直接回复内容")
	case q.MultiSelect:
		b.WriteString("请回复编号(多个用逗号分隔)、选项文字或自定义内容")
	default:
		b.WriteString("请回复编号、选项文字或自定义内容")
	}
	return b.String()
}

// matchAnswer 把用户的回复匹配成工具答案（序号映射与多选拆分用 tools
// 公共件，标签模糊匹配是消息渠道的口语回复语义）。
func matchAnswer(q tools.Question, reply string) any {
	if len(q.Options) == 0 {
		return reply
	}
	if q.MultiSelect {
		var out []string
		for _, part := range tools.SplitOptionParts(reply) {
			if l := matchOption(q.Options, part); l != "" {
				out = append(out, l)
			}
		}
		if len(out) == 0 {
			return []string{reply}
		}
		return out
	}
	if l := matchOption(q.Options, reply); l != "" {
		return l
	}
	return reply
}

// matchOption 编号精确匹配 → 标签全等(忽略大小写) → 包含匹配。
func matchOption(opts []tools.Option, s string) string {
	s = strings.TrimSpace(s)
	if l := tools.OptionByIndex(opts, s); l != "" {
		return l
	}
	for _, o := range opts {
		if strings.EqualFold(o.Label, s) {
			return o.Label
		}
	}
	for _, o := range opts {
		if s != "" && (strings.Contains(o.Label, s) || strings.Contains(s, o.Label)) {
			return o.Label
		}
	}
	return ""
}
