// mux.go 按用户分发消息与回合执行：userMux 每用户一个 goroutine（同一
// 用户串行、不同用户并发），静默窗口内合并连续消息（微信"图片+配文"
// 会被拆成两条先后到达）；runTurn 把一批消息并成一轮交给 agent 并回复。
package weixin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"git.gorun.fun/share/grf-harness/tools"
)

// mergeWindow 同一用户连续多条消息的合并窗口。微信里"图片+配文"会被
// ClawBot 拆成两条消息先后到达,这里等一小段静默期把它们并成同一轮输入。
const mergeWindow = 2 * time.Second

// inboundPiece 一条入站消息提取出的内容(文本/图片/文件)。
type inboundPiece struct {
	fromID       string
	contextToken string
	text         string
	images       []tools.Image
	hasMedia     bool
}

// userMux 按用户分发消息:每用户一个 goroutine,先攒一个静默窗口内的
// 所有 piece,再合并成一轮交给 agent。同一用户串行、不同用户并发。
// asks 为 ask_user 的应答拦截通道:登记期间该用户的消息直送提问方,
// 不进入普通批次。
type userMux struct {
	ctx  context.Context
	bot  *Client
	st   *State
	pool *agentPool
	mu   sync.Mutex
	chs  map[string]chan *inboundPiece
	asks map[string]chan *inboundPiece
}

func newUserMux(ctx context.Context, bot *Client, st *State, pool *agentPool) *userMux {
	return &userMux{
		ctx:  ctx,
		bot:  bot,
		st:   st,
		pool: pool,
		chs:  map[string]chan *inboundPiece{},
		asks: map[string]chan *inboundPiece{},
	}
}

// push 投递一条消息(无缓冲,用户 goroutine 忙于跑 agent 时由主循环兜着)。
func (m *userMux) push(p *inboundPiece) {
	m.mu.Lock()
	if ask, ok := m.asks[p.fromID]; ok {
		// 提问等待中:该消息是对 ask_user 的回复
		m.mu.Unlock()
		select {
		case ask <- p:
		case <-m.ctx.Done():
		}
		return
	}
	ch, ok := m.chs[p.fromID]
	if !ok {
		ch = make(chan *inboundPiece, 64)
		m.chs[p.fromID] = ch
		go m.userLoop(ch)
	}
	m.mu.Unlock()
	select {
	case ch <- p:
	case <-m.ctx.Done():
	}
}

// beginAsk 登记应答拦截通道(发问题之前登记,防止秒回被并入普通批次)。
func (m *userMux) beginAsk(fromID string) chan *inboundPiece {
	ch := make(chan *inboundPiece, 1)
	m.mu.Lock()
	m.asks[fromID] = ch
	m.mu.Unlock()
	return ch
}

// endAsk 注销应答通道。
func (m *userMux) endAsk(fromID string) {
	m.mu.Lock()
	delete(m.asks, fromID)
	m.mu.Unlock()
}

// userLoop 单用户主循环:攒批 → 处理 → 再等下一条。
func (m *userMux) userLoop(ch chan *inboundPiece) {
	for {
		var first *inboundPiece
		select {
		case p := <-ch:
			first = p
		case <-m.ctx.Done():
			return
		}
		// 静默合并:窗口内每来一条就续期,直到安静满 mergeWindow
		batch := []*inboundPiece{first}
		timer := time.NewTimer(mergeWindow)
		defer timer.Stop()
	collect:
		for {
			select {
			case p := <-ch:
				batch = append(batch, p)
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(mergeWindow)
			case <-timer.C:
				break collect
			case <-m.ctx.Done():
				return
			}
		}
		runTurn(m.ctx, m.bot, m.st, m.pool, batch)
	}
}

// extractPiece 从原始消息提取文本/图片/文件;非用户消息或空载荷返回 nil。
func extractPiece(ctx context.Context, pool *agentPool, msg map[string]any) *inboundPiece {
	// message_type=1 为用户发来的消息;2 为 bot 发出
	if mt := i64p(msg["message_type"]); mt == nil || *mt != 1 {
		return nil
	}
	fromID := str(msg["from_user_id"])
	contextToken := str(msg["context_token"])
	if fromID == "" || contextToken == "" {
		return nil
	}
	text := extractMessageText(msg)
	media := extractMedia(ctx, msg)
	p := &inboundPiece{fromID: fromID, contextToken: contextToken, text: text}
	if media != nil {
		p.hasMedia = true
		for _, img := range media.images {
			p.images = append(p.images, tools.Image{Mime: img.mime, B64: img.b64})
		}
		// 文件落盘到工作区下的 weixin_files/，把路径告诉模型，
		// 模型可用 read/bash 等工具按需处理
		if media.fileData != nil {
			path, err := saveInboundFile(pool.opts.Workspace, media.fileName, media.fileData)
			if err != nil {
				fmt.Printf("⚠️ 入站文件落盘失败(忽略): %v\n", err)
			} else {
				note := "用户发送了文件 " + path + "，已保存到工作区，可用文件工具读取"
				if p.text == "" {
					p.text = note
				} else {
					p.text += "\n" + note
				}
			}
		}
	}
	// 既无文本也无媒体:可能是载荷结构变化导致漏识别,dump 原始 JSON 便于排查
	if p.text == "" && media == nil {
		if bs, jerr := json.Marshal(msg); jerr == nil {
			preview := bs
			if len(preview) > 800 {
				preview = preview[:800]
			}
			fmt.Printf("⚠️ 未识别的消息载荷: %s\n", preview)
		}
		return nil
	}
	return p
}

// runTurn 把一批 piece 合并成一轮:文本拼接、图片汇总,跑一次 agent,
// 用最后一条消息的 context_token 回复。
func runTurn(ctx context.Context, bot *Client, st *State, pool *agentPool, batch []*inboundPiece) {
	var texts []string
	var images []tools.Image
	fromID := batch[0].fromID
	contextToken := batch[len(batch)-1].contextToken
	for _, p := range batch {
		if p.text != "" {
			texts = append(texts, p.text)
		}
		images = append(images, p.images...)
	}
	text := strings.Join(texts, "\n")
	if text == "" && len(images) > 0 {
		text = "用户发送了图片，请查看"
	}
	if len(batch) > 1 {
		fmt.Printf("🧩 已合并 %d 条消息(含 %d 张图片)\n", len(batch), len(images))
	}
	fmt.Printf("📩 收到消息: %s\n", displayText(text, len(images)))

	// /new 重开会话:清空该用户的 agent 历史
	if cmd := strings.TrimSpace(text); cmd == "/new" || cmd == "新会话" {
		pool.drop(fromID)
		_ = bot.SendMessage(ctx, fromID, contextToken, "🆕 已开启新会话")
		return
	}

	st.LastFromID = fromID
	st.LastContextTok = contextToken
	_ = SaveState(st)

	a, host, err := pool.get(fromID)
	if err != nil {
		fmt.Printf("组装 agent 失败: %v\n", err)
		_ = bot.SendMessage(ctx, fromID, contextToken, replyOnLlmError)
		return
	}
	// ask_user 的回复要用本轮的 context_token 关联微信对话
	host.setTurnToken(contextToken)
	// agent 一轮可能跑很久(多步工具调用),给足超时
	qctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	reply, err := a.RunWithImages(qctx, text, images)
	cancel()
	if err != nil {
		fmt.Printf("agent 执行失败: %v\n(已重置该用户会话,下条消息从新会话开始)\n", err)
		pool.drop(fromID)
		_ = bot.SendMessage(ctx, fromID, contextToken, replyOnLlmError)
		return
	}
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return
	}
	if err := bot.SendMessage(ctx, fromID, contextToken, reply); err != nil {
		fmt.Printf("回复发送失败: %v\n", err)
		return
	}
	preview := []rune(reply)
	if len(preview) > 50 {
		preview = preview[:50]
	}
	fmt.Printf("✉️  已回复: %s…\n", string(preview))
}

// displayText 控制台展示用的消息摘要。
func displayText(text string, imageCount int) string {
	if text == "" && imageCount > 0 {
		return fmt.Sprintf("(图片×%d)", imageCount)
	}
	if imageCount > 0 {
		return fmt.Sprintf("%s [图片×%d]", text, imageCount)
	}
	return text
}

// saveInboundFile 把入站文件保存到 workspace/weixin_files/ 下（名字
// 冲突时加时间戳前缀），返回绝对路径。
func saveInboundFile(workspace, name string, data []byte) (string, error) {
	dir := filepath.Join(workspace, "weixin_files")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." || strings.ContainsAny(name, "/\\") {
		name = fmt.Sprintf("file_%d.bin", time.Now().UnixMilli())
	} else {
		name = fmt.Sprintf("%d_%s", time.Now().UnixMilli(), name)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// extractMessageText 遍历 item_list 提取文本与语音转写。
func extractMessageText(msg map[string]any) string {
	var parts []string
	if items, ok := msg["item_list"].([]any); ok {
		for _, it := range items {
			item, ok := it.(map[string]any)
			if !ok {
				continue
			}
			if ti, ok := item["text_item"].(map[string]any); ok && str(ti["text"]) != "" {
				parts = append(parts, str(ti["text"]))
				continue
			}
			if vi, ok := item["voice_item"].(map[string]any); ok && str(vi["text"]) != "" {
				parts = append(parts, str(vi["text"]))
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}
