// plan.go exit_plan_mode 工具：计划模式下呈交计划（PlanController 由
// agent 包注册时注入，审批经 Host.ApprovePlan）。
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

// RegisterExitPlanMode 注册 exit_plan_mode。
func RegisterExitPlanMode(r *Registry, pc PlanController, host Host) {
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
		if host == nil {
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
