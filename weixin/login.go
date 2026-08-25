package weixin

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	qrScanTimeout   = 480 * time.Second // 扫码整体等待上限(与官方客户端一致)
	maxQRRefresh    = 3                 // 最多展示 3 个二维码链接
	qrStatusTimeout = 35 * time.Second  // 状态长轮询超时(超时视为 wait)
	loginPollSleep  = 1 * time.Second
)

// FetchLoginQRCode 申请登录二维码。localTokens 为本地已有 token(最多 10 个,用于已连接复用)。
// 返回的 qrcode 为轮询凭据,qrcode_img_content 为扫码链接(浏览器打开后用微信扫页面上的码)。
func (c *Client) FetchLoginQRCode(ctx context.Context, localTokens []string) (qrcode, link string, err error) {
	seen := map[string]bool{}
	tokens := make([]string, 0, len(localTokens))
	for _, t := range localTokens {
		t = strings.TrimSpace(t)
		if t != "" && !seen[t] {
			seen[t] = true
			tokens = append(tokens, t)
		}
	}
	if len(tokens) > 10 {
		tokens = tokens[:10]
	}
	data, err := c.PostNoToken(ctx, "ilink/bot/get_bot_qrcode?bot_type=3", map[string]any{
		"local_token_list": tokens,
	})
	if err != nil {
		return "", "", err
	}
	qrcode = str(data["qrcode"])
	if qrcode == "" {
		return "", "", fmt.Errorf("二维码响应缺少 qrcode 字段")
	}
	return qrcode, str(data["qrcode_img_content"]), nil
}

// loginResult 轮询结果的语义化解析。
type loginResult struct {
	BotToken    string
	BaseURL     string
	IlinkBotID  string
	IlinkUserID string

	AlreadyConnected bool // binded_redirect:服务端提示已连接,可复用本地 token
	Scanned          bool
	NeedVerifyCode   bool
	VerifyBlocked    bool
	Expired          bool
	RedirectBase     string // scaned_but_redirect:切换轮询节点
}

func parseLoginStatus(data map[string]any) loginResult {
	r := loginResult{}
	state := str(data["status"])
	switch {
	case state == "confirmed" || str(data["bot_token"]) != "":
		r.BotToken = str(data["bot_token"])
		r.BaseURL = str(data["baseurl"])
		if r.BaseURL == "" {
			r.BaseURL = str(data["base_url"])
		}
		r.IlinkBotID = str(data["ilink_bot_id"])
		r.IlinkUserID = str(data["ilink_user_id"])
	case state == "binded_redirect" || data["binded_redirect"] == true:
		r.AlreadyConnected = true
	case state == "expired":
		r.Expired = true
	case state == "scaned_but_redirect":
		if h := str(data["redirect_host"]); h != "" {
			r.RedirectBase = strings.TrimRight(h, "/")
			if !strings.HasPrefix(r.RedirectBase, "http") {
				r.RedirectBase = "https://" + r.RedirectBase
			}
		}
	case state == "scaned":
		r.Scanned = true
	case state == "need_verifycode" || state == "verify_code_blocked" || data["need_verifycode"] == true:
		if state == "verify_code_blocked" {
			r.VerifyBlocked = true
		} else {
			r.NeedVerifyCode = true
		}
	}
	return r
}

// pollLoginStatus 轮询一次二维码状态。GET 不带 Bearer;超时视为 wait。
func (c *Client) pollLoginStatus(ctx context.Context, qrcode, verifyCode string) (map[string]any, error) {
	path := "ilink/bot/get_qrcode_status?qrcode=" + url.QueryEscape(qrcode)
	if verifyCode != "" {
		path += "&verify_code=" + url.QueryEscape(verifyCode)
	}
	data, err := c.Get(ctx, path, qrStatusTimeout+5*time.Second)
	if err != nil {
		if ctx.Err() == nil && strings.Contains(err.Error(), "context deadline exceeded") {
			return map[string]any{"status": "wait"}, nil
		}
		return nil, err
	}
	return data, nil
}

// WaitLoginConfirmation 循环轮询直到确认/失败/超时。verifyCodeCh 用于命令行输入数字配对码。
func (c *Client) WaitLoginConfirmation(ctx context.Context, qrcode string, timeout time.Duration,
	verifyCodeCh <-chan string) loginResult {
	deadline := time.Now().Add(timeout)
	pendingVerify := ""
	scannedPrinted := false

	for {
		if time.Now().After(deadline) {
			return loginResult{}
		}
		r, err := c.pollLoginStatus(ctx, qrcode, pendingVerify)
		if err != nil {
			if ctx.Err() != nil {
				return loginResult{} // 整体已取消(Ctrl+C/超时),不再重试
			}
			fmt.Printf("轮询扫码状态失败: %v,稍后重试\n", err)
			sleepCtx(ctx, loginPollSleep)
			continue
		}
		res := parseLoginStatus(r)

		switch {
		case res.BotToken != "":
			if res.IlinkBotID == "" {
				fmt.Println("confirmed 响应缺少 ilink_bot_id,登录状态异常")
				return loginResult{}
			}
			return res
		case res.RedirectBase != "":
			c.BaseURL = res.RedirectBase
			fmt.Printf("扫码轮询切换到新节点: %s\n", res.RedirectBase)
			continue
		case res.Scanned:
			if !scannedPrinted {
				fmt.Println("✅ 已扫码,请在手机上确认…")
				scannedPrinted = true
			}
		case res.NeedVerifyCode:
			fmt.Print("请输入手机微信显示的数字配对码: ")
			select {
			case code := <-verifyCodeCh:
				pendingVerify = strings.TrimSpace(code)
			case <-ctx.Done():
				return loginResult{}
			case <-time.After(2 * time.Minute):
				fmt.Println("配对码输入超时")
				return loginResult{}
			}
			continue
		}

		select {
		case <-ctx.Done():
			return loginResult{}
		case <-time.After(loginPollSleep):
		}
	}
}

// LoginWithQRCode 执行完整二维码登录,最多申请 maxQRRefresh 次二维码。
// 不做终端二维码渲染:打印扫码链接,用户在浏览器打开后用微信扫码。
func (c *Client) LoginWithQRCode(ctx context.Context, localTokens []string, verifyCodeCh <-chan string) (loginResult, error) {
	deadline := time.Now().Add(qrScanTimeout)
	for refresh := 1; refresh <= maxQRRefresh; refresh++ {
		if err := ctx.Err(); err != nil {
			return loginResult{}, err
		}
		qrcode, link, err := c.FetchLoginQRCode(ctx, localTokens)
		if err != nil {
			return loginResult{}, fmt.Errorf("获取二维码失败: %w", err)
		}
		fmt.Println()
		fmt.Println("════════ 微信 ClawBot 扫码登录 ════════")
		if link != "" {
			fmt.Printf("  浏览器打开以下链接,用微信扫页面上的二维码:\n\n  %s\n\n", link)
		} else {
			fmt.Printf("  扫码凭据: %s\n\n", qrcode)
		}
		fmt.Println("  (微信 → 我 → 设置 → 插件 → 开启 ClawBot)")
		fmt.Printf("  第 %d/%d 个二维码,等待扫码…\n", refresh, maxQRRefresh)
		fmt.Println()

		res := c.WaitLoginConfirmation(ctx, qrcode, time.Until(deadline), verifyCodeCh)
		if res.BotToken != "" || res.AlreadyConnected {
			return res, nil
		}
		if res.VerifyBlocked {
			fmt.Println("多次输入配对码错误,正在刷新二维码…")
			continue
		}
		fmt.Println("二维码已过期或超时,正在重新生成…")
	}
	return loginResult{}, fmt.Errorf("二维码多次失效或登录超时,请稍后重试")
}

// NotifyLifecycle 发送 msg/notifystart / msg/notifystop 生命周期通知。
func (c *Client) NotifyLifecycle(ctx context.Context, endpoint string) error {
	_, err := c.Post(ctx, endpoint, map[string]any{"base_info": baseInfo()})
	return err
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
