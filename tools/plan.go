// plan.go 计划模式工具：exit_plan_mode 呈交计划、enter_plan_mode 模型
// 主动申请进入计划模式（PlanController 由 agent 包注册时注入，审批分别
// 经 Host.ApprovePlan 与 Host.AskUser）。
package tools

import (
	"context"
	"fmt"
	"strings"
)

// PlanController 计划模式控制器（agent 包实现）。
type PlanController interface {
	// InPlanMode 当前是否处于计划模式。
	InPlanMode() bool
	// SubmitPlan 呈交计划：返回 (已批准, 用户反馈)。反馈仅在未批准时有值。
	SubmitPlan(ctx context.Context, plan string) (bool, string)
}

// PlanNoop 默认控制器：不在计划模式。
type PlanNoop struct{}

func (PlanNoop) InPlanMode() bool                                           { return false }
func (PlanNoop) SubmitPlan(ctx context.Context, plan string) (bool, string) { return false, "" }

// PlanModeEnterer 可选控制器扩展：支持模型主动请求进入计划模式
// （enter_plan_mode）。未实现时该工具按"不支持"优雅拒绝，既有
// PlanController 实现不受影响。
type PlanModeEnterer interface {
	// RequestEnterPlanMode 征求用户同意后切换到计划模式。
	// 返回 (已切换, 拒绝反馈)；反馈仅在未切换时有值。
	RequestEnterPlanMode(ctx context.Context, reason string) (bool, string)
}

// RegisterExitPlanMode 注册 exit_plan_mode。host 懒取（注册后 SetHost
// 替换的宿主同样生效，与 RegisterBash/RegisterAskUser 的注入模式一致）；
// 交互决策本体在 PlanController（SubmitPlan）。
func RegisterExitPlanMode(r *Registry, pc PlanController, host func() Host) {
	r.Register("exit_plan_mode", func(ctx context.Context, args string) (Result, error) {
		var a struct {
			Plan string `json:"plan"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return Result{}, err
		}
		if strings.TrimSpace(a.Plan) == "" {
			return Result{}, fmt.Errorf("plan is required (the COMPLETE plan as markdown, starting with a # heading)")
		}
		if !strings.HasPrefix(strings.TrimSpace(a.Plan), "#") {
			return Result{}, fmt.Errorf("the plan must be markdown starting with a # heading that names it")
		}
		if !pc.InPlanMode() {
			return Result{}, fmt.Errorf("not in plan mode; exit_plan_mode is only usable in plan mode")
		}
		if host == nil || host() == nil {
			return Result{}, fmt.Errorf("no host available to review the plan")
		}
		approved, feedback := pc.SubmitPlan(ctx, a.Plan)
		if approved {
			return TextResult("The user approved the plan. You are now out of plan mode — carry out the plan from your next step."), nil
		}
		if strings.TrimSpace(feedback) == "" {
			feedback = "The user kept planning without written feedback. Ask what to adjust or revise the plan."
		}
		return TextResult("The plan was NOT approved. The user's feedback:\n\n" + feedback + "\n\nRevise the plan and present it again with exit_plan_mode."), nil
	})
}

// RegisterEnterPlanMode 注册 enter_plan_mode（仅根会话注册）。已在计划
// 模式时幂等提示；否则经控制器征求用户同意，批准即切换、拒绝连同反馈
// 引导模型直接继续执行。
func RegisterEnterPlanMode(r *Registry, pc PlanController) {
	r.Register("enter_plan_mode", func(ctx context.Context, args string) (Result, error) {
		var a struct {
			Reason string `json:"reason"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return Result{}, err
		}
		if strings.TrimSpace(a.Reason) == "" {
			return Result{}, fmt.Errorf("reason is required: one or two sentences on why this task warrants a plan first")
		}
		if pc.InPlanMode() {
			return TextResult("Already in plan mode. Investigate with read-only tools and present your plan with exit_plan_mode."), nil
		}
		pe, ok := pc.(PlanModeEnterer)
		if !ok {
			return TextResult("This host does not support switching into plan mode mid-session; continue executing directly."), nil
		}
		entered, feedback := pe.RequestEnterPlanMode(ctx, a.Reason)
		if entered {
			return TextResult("The user approved entering plan mode. From now on investigate freely but change nothing — mutating tools (write/edit/bash/ralph/workflow) are rejected outright. Present your implementation plan with exit_plan_mode and wait for approval before executing it."), nil
		}
		if strings.TrimSpace(feedback) == "" {
			feedback = "no reason given"
		}
		return TextResult("The user declined plan mode. Feedback: " + feedback + "\n\nContinue executing the task directly."), nil
	})
}
