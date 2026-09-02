// policy.go set_policy 工具：运行时变更统一规则串 policy（仅根会话注册）。
// 模型只能拉起弹窗并附理由——新规则不可由模型传值，用户从预制的两个
// 选项（write-workspace / off）里亲自选择；选择经 Host.AskUser 呈现
// （TUI ask 表单 / 行模式编号选项 / deferred 宿主转暂停点），生效仅本
// 会话、不落盘。
package tools

import (
	"context"
	"fmt"
	"strings"
)

// SetPolicyOption 预制策略选项（用户弹窗可选值，也是最终落地的 policy 串）。
type SetPolicyOption struct {
	Policy      string
	Label       string
	Description string
}

// SetPolicyOptions 预制的全部策略选项：工作区访问与完全访问，仅此两个。
var SetPolicyOptions = []SetPolicyOption{
	{Policy: "write-workspace", Label: "工作区访问（write-workspace）",
		Description: "文件工具与 bash 沙箱限制在工作区内（写前须先 read；读边界沿用当前）"},
	{Policy: "off", Label: "完全访问（off）",
		Description: "本会话内解除沙箱与读写限制"},
}

// setPolicyKeepOpt "保持现状"选项文案（默认拒绝）。
const setPolicyKeepOpt = "保持当前 policy"

// RegisterSetPolicy 注册 set_policy。host 为宿主 getter（懒取，SetHost 后
// 生效）；apply 收到用户选定的 policy（预制值之一）由调用方落地。
func RegisterSetPolicy(r *Registry, host func() Host, apply func(policy string)) {
	r.Register("set_policy", func(ctx context.Context, args string) (Result, error) {
		var a struct {
			Reason string `json:"reason"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return Result{}, err
		}
		if strings.TrimSpace(a.Reason) == "" {
			return Result{}, fmt.Errorf("reason is required: one sentence for the user explaining why this session needs a policy change")
		}
		h := host()
		if h == nil || apply == nil {
			return Result{}, fmt.Errorf("no interactive user is available in this session; the policy cannot be changed")
		}
		opts := make([]Option, 0, len(SetPolicyOptions)+1)
		opts = append(opts, Option{Label: setPolicyKeepOpt, Description: "维持当前 policy 不变（默认）"})
		for _, o := range SetPolicyOptions {
			opts = append(opts, Option{Label: o.Label, Description: o.Description})
		}
		answers, err := h.AskUser(ctx, []Question{{
			ID: "set_policy",
			Header: "策略变更",
			Question: "模型请求变更本会话的 policy 规则，请选择要采用的规则（仅本会话生效，重启后恢复原配置）。\n模型理由：" + a.Reason,
			Options: opts,
		}})
		if err != nil {
			return Result{}, fmt.Errorf("the user could not answer: %v", err)
		}
		ans := ""
		switch v := answers["set_policy"].(type) {
		case string:
			ans = v
		case []string:
			if len(v) > 0 {
				ans = v[0]
			}
		}
		ans = strings.TrimSpace(ans)
		if ans == "" || ans == setPolicyKeepOpt {
			return TextResult("The user kept the current policy — the request was declined. Continue working under the existing policy; do not try other ways around it."), nil
		}
		for _, o := range SetPolicyOptions {
			if ans == o.Label {
				apply(o.Policy)
				return TextResult(fmt.Sprintf("The user selected `%s` for this session (effective immediately, this session only).", o.Policy)), nil
			}
		}
		// 选项外的自定义输入：视为用户给出的拒绝反馈
		return TextResult("The user did not pick a policy — the request was declined. User feedback: " + ans), nil
	})
}
