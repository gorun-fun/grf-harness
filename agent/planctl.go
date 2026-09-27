// planctl.go 计划模式工具的交互控制器：exit_plan_mode 呈交计划审批、
// enter_plan_mode 申请进入计划模式（工具本体在 tools 包注册，交互决策
// 在此实现；自 build.go 拆出）。
package agent

import (
	"context"
	"fmt"
	"strings"

	"git.gorun.fun/share/grf-harness/tools"
)

// planCtl 计划模式工具的控制器实现（exit_plan_mode 呈交计划、
// enter_plan_mode 申请进入）。
type planCtl struct{ a *Agent }

func (p planCtl) InPlanMode() bool { return p.a.InPlanMode() }

func (p planCtl) SubmitPlan(ctx context.Context, plan string) (bool, string) {
	if p.a.host == nil {
		return false, ""
	}
	ok, feedback := p.a.host.ApprovePlan(ctx, plan)
	if ok {
		p.a.SetPlanMode(false) // 批准即退出计划模式（与工具结果文案一致）
	}
	return ok, feedback
}

// enter 计划模式选项的固定文案（AskUser 答案按 label 精确匹配）。
const (
	planEnterOpt   = "进入计划模式"
	planDeclineOpt = "直接执行"
)

// RequestEnterPlanMode enter_plan_mode 的控制器实现：经 AskUser 弹选项
// 表单征求用户同意（TUI/行模式交互；无交互宿主报错按拒绝处理），批准
// 即切换计划模式。选项外的自定义输入视为拒绝反馈。
func (p planCtl) RequestEnterPlanMode(ctx context.Context, reason string) (bool, string) {
	if p.a.host == nil {
		return false, "no host available for approval"
	}
	answers, err := p.a.host.AskUser(ctx, []tools.Question{{
		ID:       "enter_plan_mode",
		Header:   "计划模式",
		Question: "模型请求进入计划模式，先只读调研再呈交实施计划供你审批。理由：\n" + reason,
		Options: []tools.Option{
			{Label: planEnterOpt, Description: "切换到计划模式：改动类工具被硬拦截，直到计划批准"},
			{Label: planDeclineOpt, Description: "不进入计划模式，按当前方式继续执行"},
		},
	}})
	if err != nil {
		return false, fmt.Sprintf("no interactive user available (%v)", err)
	}
	ans := ""
	switch v := answers["enter_plan_mode"].(type) {
	case string:
		ans = v
	case []string:
		if len(v) > 0 {
			ans = v[0]
		}
	}
	switch strings.TrimSpace(ans) {
	case planEnterOpt:
		p.a.SetPlanMode(true)
		return true, ""
	case planDeclineOpt, "":
		return false, ""
	default:
		return false, ans // 自定义输入即拒绝反馈
	}
}
