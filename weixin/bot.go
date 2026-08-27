package weixin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"grf/agent"
	"grf/config"
	"grf/llm"
	"grf/tools"
)

// DefaultSystemPrompt 微信侧默认系统提示词。
const DefaultSystemPrompt = "你是运行在用户微信里的 AI 助手,回答请简洁准确。"

const replyOnLlmError = "⚠️ AI 服务暂时不可用,请稍后再试"

// Run 执行 connect weixin:扫码登录(必要时)→ 长轮询收消息 → 交本地 agent
// (全部 25 个工具)处理并回复。阻塞直到 ctx 取消;退出前发送 notifystop。
// workspace 为 agent 工作区;空串用用户 home 目录。
func Run(ctx context.Context, systemPrompt, workspace string) error {
	// agent 使用 gh 当前生效的模型配置(~/.grf/settings.json)
	eff, err := config.Load()
	if err != nil {
		return fmt.Errorf("加载模型配置失败: %w", err)
	}
	client := llm.NewClient(llm.Config{BaseURL: eff.BaseURL, APIKey: eff.APIKey, Model: eff.Model})
	fmt.Printf("🤖 模型: %s (%s)\n", eff.Model, eff.BaseURL)

	if workspace == "" {
		home, herr := os.UserHomeDir()
		if herr != nil {
			workspace, _ = os.Getwd()
		} else {
			workspace = home
		}
	}
	fmt.Printf("🛠  agent 工作区: %s\n", workspace)

	bot := NewClient()
	st := LoadState()
	if st.BotToken != "" && st.BaseURL != "" {
		bot.Token = st.BotToken
		bot.BaseURL = st.BaseURL
		fmt.Printf("检测到已保存的登录态(bot=%s),尝试复用…\n", maskToken(st.BotToken))
	}

	// 先试一次 getupdates:token 失效(-14)或无 token 则走扫码登录。
	if bot.Token == "" || !tryGetUpdates(ctx, bot, st) {
		if err := LoginInteractive(ctx); err != nil {
			return err
		}
		nst := LoadState()
		bot.Token = nst.BotToken
		if nst.BaseURL != "" {
			bot.BaseURL = nst.BaseURL
		}
		st = nst
	}

	if err := bot.NotifyLifecycle(ctx, "ilink/bot/msg/notifystart"); err != nil {
		fmt.Printf("notifystart 发送失败(忽略): %v\n", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = bot.NotifyLifecycle(stopCtx, "ilink/bot/msg/notifystop")
	}()

	fmt.Println("🤖 Bot 已上线,开始监听微信消息(Ctrl+C 退出)…")
	autoApprove := false
	if settings, serr := config.LoadFileForUpdate(); serr == nil {
		autoApprove = settings.AutoApprove
	}
	return messageLoop(ctx, bot, st, client, systemPrompt, workspace, autoApprove)
}

// agentPool 每个微信用户一个独立 agent 会话(独立历史与工具工作区上下文)。
// 消息经 userMux 分发到各用户的 goroutine 并发处理,pool 需加锁。
type agentPool struct {
	mu          sync.Mutex
	opts        agent.Options
	agents      map[string]*agent.Agent
	hosts       map[string]*weixinHost
	mux         *userMux // messageLoop 创建后回填(get 时必已就位)
	autoApprove bool
}

func newAgentPool(client *llm.Client, systemPrompt, workspace string, autoApprove bool) *agentPool {
	return &agentPool{
		opts: agent.Options{
			Workspace:    workspace,
			Client:       client,
			SystemPrompt: systemPrompt,
		},
		agents:      map[string]*agent.Agent{},
		hosts:       map[string]*weixinHost{},
		autoApprove: autoApprove,
	}
}

// drop 丢弃该用户的 agent 会话(下次消息重建,历史清零)。用于 /new 指令
// 与失败自愈:被污染的历史(如模型不支持的历史图片)会导致后续每轮都失败。
func (p *agentPool) drop(fromID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.agents, fromID)
}

// get 取该用户的 agent 与宿主;首次触达时组装 harness(ask_user 经
// weixinHost 转发到微信)并打印工具事件。
func (p *agentPool) get(fromID string) (*agent.Agent, *weixinHost, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a, ok := p.agents[fromID]; ok {
		return a, p.hosts[fromID], nil
	}
	host := &weixinHost{
		NoopHost: tools.NoopHost{AutoApprove: p.autoApprove},
		bot:      p.mux.bot,
		mux:      p.mux,
		pool:     p,
		fromID:   fromID,
	}
	opts := p.opts
	opts.Host = host
	h, err := agent.Build(opts)
	if err != nil {
		return nil, nil, err
	}
	root := h.Root
	root.SetOnEvent(func(kind, detail string) {
		switch kind {
		case "tool":
			fmt.Printf("   🔧 %s\n", detail)
		case "result":
			fmt.Printf("   ↳ %s\n", detail)
		case "done":
			fmt.Printf("   ✔ 一轮完成\n")
		}
	})
	p.agents[fromID] = root
	p.hosts[fromID] = host
	return root, host, nil
}

// LoginInteractive 前台交互式扫码登录:二维码链接打终端、配对码从 stdin 读,
// 成功后登录态落盘(~/.grf/weixin_state.json)。供 connect 在守护化之前调用,
// 把新建 ClawBot 的流程走完再进后台。
func LoginInteractive(ctx context.Context) error {
	bot := NewClient()
	st := LoadState()
	verifyCh := make(chan string)
	go readStdinLines(ctx, verifyCh)
	res, err := bot.LoginWithQRCode(ctx, localTokens(st), verifyCh)
	if err != nil {
		return err
	}
	if res.BotToken == "" {
		return fmt.Errorf("服务端提示此端已连接过,但本地没有可复用的 token,请重新运行")
	}
	st.BotToken = res.BotToken
	if res.BaseURL != "" {
		st.BaseURL = res.BaseURL
	}
	st.IlinkBotID = res.IlinkBotID
	st.IlinkUserID = res.IlinkUserID
	st.GetUpdatesBuf = "" // 新登录重置游标
	if err := SaveState(st); err != nil {
		return err
	}
	fmt.Printf("🎉 微信 ClawBot 连接成功!baseurl=%s\n", st.BaseURL)
	return nil
}

// messageLoop 长轮询主循环:getupdates → 逐条处理 → 回复。token 失效时重新扫码。
func messageLoop(ctx context.Context, bot *Client, st *State, client *llm.Client,
	systemPrompt, workspace string, autoApprove bool) error {
	pool := newAgentPool(client, systemPrompt, workspace, autoApprove)
	mux := newUserMux(ctx, bot, st, pool)
	pool.mux = mux
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		data, err := bot.Post(ctx, "ilink/bot/getupdates", map[string]any{
			"get_updates_buf": st.GetUpdatesBuf,
			"base_info":       baseInfo(),
		})
		if err != nil {
			if apiErr, ok := err.(*APIError); ok && apiErr.IsStaleToken() {
				fmt.Println("⚠️ token 已失效,需要重新扫码登录…")
				res, lerr := bot.LoginWithQRCode(ctx, nil, nil)
				if lerr != nil {
					return fmt.Errorf("重新登录失败: %w", lerr)
				}
				bot.Token = res.BotToken
				if res.BaseURL != "" {
					bot.BaseURL = res.BaseURL
				}
				st.BotToken, st.BaseURL = bot.Token, bot.BaseURL
				st.GetUpdatesBuf = ""
				_ = SaveState(st)
				continue
			}
			fmt.Printf("getupdates 失败: %v,5 秒后重试\n", err)
			sleepCtx(ctx, 5*time.Second)
			continue
		}
		if buf := str(data["get_updates_buf"]); buf != "" {
			st.GetUpdatesBuf = buf
			_ = SaveState(st)
		}

		if msgs, ok := data["msgs"].([]any); ok {
			for _, m := range msgs {
				msg, ok := m.(map[string]any)
				if !ok {
					continue
				}
				if piece := extractPiece(ctx, pool, msg); piece != nil {
					mux.push(piece)
				}
			}
		}
	}
}

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

// SendMessage 发送文本消息。context_token 必须来自当前入站消息,否则无法关联对话。
func (c *Client) SendMessage(ctx context.Context, toUserID, contextToken, text string) error {
	body := map[string]any{
		"msg": map[string]any{
			"from_user_id":  "",
			"to_user_id":    toUserID,
			"client_id":     GenerateClientID(),
			"message_type":  2, // BOT 发出
			"message_state": 2, // FINISH(完整消息)
			"context_token": contextToken,
			"item_list":     []any{map[string]any{"type": 1, "text_item": map[string]any{"text": text}}},
		},
		"base_info": baseInfo(),
	}
	_, err := c.do(ctx, http.MethodPost, "ilink/bot/sendmessage", body, true, 120*time.Second)
	return err
}

// tryGetUpdates 用已保存 token 试探一次长轮询。
// 返回 false 表示 token 失效,需要重新扫码;网络错误不强制扫码(主循环自行重试)。
func tryGetUpdates(ctx context.Context, client *Client, st *State) bool {
	ctx2, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	_, err := client.Post(ctx2, "ilink/bot/getupdates", map[string]any{
		"get_updates_buf": st.GetUpdatesBuf,
		"base_info":       baseInfo(),
	})
	if err == nil {
		return true
	}
	if apiErr, ok := err.(*APIError); ok {
		return !apiErr.IsStaleToken() && apiErr.HTTP != http.StatusUnauthorized
	}
	return true
}

func localTokens(st *State) []string {
	if st.BotToken != "" {
		return []string{st.BotToken}
	}
	return nil
}

// readStdinLines 把命令行输入行送入 channel(用于数字配对码输入)。
func readStdinLines(ctx context.Context, out chan<- string) {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		select {
		case out <- line:
		case <-ctx.Done():
			return
		}
	}
}

func maskToken(t string) string {
	if len(t) <= 8 {
		return "***"
	}
	return t[:4] + "…" + t[len(t)-4:]
}
