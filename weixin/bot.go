// Package weixin 微信 ClawBot 消息渠道：`gh connect weixin` 拉起守护进程
// （见根目录 connect.go），长轮询收消息后交本地 agent（全部 28 个工具）
// 处理并回复。按功能线条分文件：bot.go（Run 入口/扫码登录/长轮询主循环）、
// pool.go（每用户 agent 会话池）、mux.go（按用户分发与回合执行）、
// login.go（扫码流程）、media.go（媒体下载解密）、ilink.go（HTTP 客户端）、
// host.go（ask_user 转发到微信）、state.go（登录态落盘）。
package weixin

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/llm"
)

const replyOnLlmError = "⚠️ AI 服务暂时不可用,请稍后再试"

// Run 执行 connect weixin:扫码登录(必要时)→ 长轮询收消息 → 交本地 agent
// (全部 28 个工具)处理并回复。阻塞直到 ctx 取消;退出前发送 notifystop。
// workspace 为 agent 工作区;空串用用户 home 目录。
func Run(ctx context.Context, systemPrompt, workspace string) error {
	// agent 使用 gh 当前生效的模型配置(~/.grf/settings.json)
	eff, err := config.Load()
	if err != nil {
		return fmt.Errorf("加载模型配置失败: %w", err)
	}
	client := llm.NewFromEffective(eff)
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
	mux := newUserMux(ctx, bot, pool)
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
				if piece := extractPiece(msg); piece != nil {
					mux.push(piece)
				}
			}
		}
	}
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
