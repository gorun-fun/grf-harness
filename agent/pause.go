// pause.go 回合暂停/恢复协议：OpenAI Responses 式接入（gh server）所需。
// 暂停 = 工具循环中途返回，历史尾部保留未闭合的 tool_call（OpenAI 线格式
// 的合法中间态）；恢复 = 注入对应结果后续跑循环（循环本体在 turn.go）。
// 触发点三类：
//
//	client 工具（Registry.RegisterClient，ORS function 工具）
//	deferred 宿主的 ask_user 提问（tools.DeferredHost + ask_user_question）
//	deferred 宿主审批模式下的工具调用（SetApproveMode + DeferredHost）
//
// 暂停状态完全落在持久化历史里（未闭合 tool_call 的参数即交互请求原文），
// 进程重启后仍可 Resume——仅步数计数重计（MaxSteps 是安全上限，重计只会
// 更宽松）。
package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"git.gorun.fun/share/grf-harness/tools"
)

// 暂停原因（PauseCall.Kind）。
const (
	PauseKindFunction = "function"      // 客户端函数工具：接入方执行后回传 Output
	PauseKindApproval = "tool_approval" // 逐工具审批：接入方批准（本地执行）或拒绝
	PauseKindQuestion = "question"      // ask_user 提问：接入方回传 Answers
)

// PauseCall 一个未闭合的工具调用（暂停点）。Arguments 即交互请求原文
// （question 的 questions 数组、审批下的工具参数都在其中）。
type PauseCall struct {
	CallID    string
	Name      string
	Arguments string
	Kind      string
}

// PausedTurn 回合暂停快照。
type PausedTurn struct {
	Calls []PauseCall
	Steps int // 暂停时已消耗的工具循环步数（Resume 续用）
}

// ResumeInput 恢复时对一个暂停点的处置。
type ResumeInput struct {
	CallID   string         // 匹配暂停调用（CallID）；单个暂停调用时可空
	Output   string         // Kind=function：接入方执行结果
	Answers  map[string]any // Kind=question：各问题答案
	Approved bool           // Kind=approval：是否放行（放行则本地执行该工具）
	Feedback string         // Kind=approval 拒绝时的反馈文本
	Dismiss  bool           // 放弃：question 视为用户取消、approval 视为拒绝
}

// PausedError Run* 系列遇到暂停点时的返回（这些入口不处理暂停；接入方
// 用 RunTurn/Resume 驱动）。
type PausedError struct{ Turn *PausedTurn }

func (e *PausedError) Error() string {
	kinds := make([]string, 0, len(e.Turn.Calls))
	for _, c := range e.Turn.Calls {
		kinds = append(kinds, c.Kind+":"+c.Name)
	}
	return "turn paused awaiting " + strings.Join(kinds, ", ")
}

// ErrResumeInput 恢复处置校验失败（未闭合调用缺结果 / 无暂停可恢复）：
// 属调用方输入错误而非执行失败，接入方（server）应映射为 400 类响应、
// 不生成终态资源。
var ErrResumeInput = errors.New("resume input error")

// Resume 以给定处置继续暂停的回合。inputs 需覆盖全部未闭合调用（按
// CallID 匹配；恰好一个未闭合调用时 CallID 可省）。
func (a *Agent) Resume(ctx context.Context, inputs []ResumeInput) (string, *PausedTurn, error) {
	if inputs == nil {
		inputs = []ResumeInput{} // nil 也走恢复分支：空处置应报"缺少结果"而非当新回合
	}
	return a.runTurn(ctx, nil, false, inputs)
}

// HasPausedCalls 历史尾部是否存在未闭合的工具调用（暂停待恢复态；跨
// 重启恢复暂停回合的判据——未闭合信息本就在持久化历史里）。执行中的
// 工具调用（assistant 已入历史、结果尚未回填）是正常中间态，不算暂停。
func (a *Agent) HasPausedCalls() bool {
	if a.IsRunning() {
		return false
	}
	_, open := a.pausedCalls()
	return len(open) > 0
}

// applyResume 注入恢复处置：按暂停类别把每个未闭合调用的结果并入历史
// （审批放行的在本地执行）。inputs 需覆盖全部未闭合调用。
func (a *Agent) applyResume(turnCtx context.Context, inputs []ResumeInput) error {
	_, open := a.pausedCalls()
	if len(open) == 0 {
		return fmt.Errorf("%w: no paused tool calls to resume", ErrResumeInput)
	}
	byID := map[string]ResumeInput{}
	for _, in := range inputs {
		byID[in.CallID] = in
	}
	if len(inputs) == 1 && len(open) == 1 && inputs[0].CallID == "" {
		byID[open[0].ID] = inputs[0]
	}
	var missing []string
	for _, c := range open {
		if _, ok := byID[c.ID]; !ok {
			missing = append(missing, c.Name())
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: missing results for paused calls: %s", ErrResumeInput, strings.Join(missing, ", "))
	}
	for _, c := range open {
		in := byID[c.ID]
		// 类别按名字重推断（与 deferCall 共用 pauseKindOf，见其注释）
		kind := pauseKindOf(a.reg, c.Name())
		switch kind {
		case PauseKindFunction:
			out := in.Output
			if in.Dismiss {
				out = "[client tool] the caller dismissed this call without executing it; proceed without it or offer an alternative"
			}
			a.finishCall(c, c.ID, tools.Result{Text: out}, ToolStatusDone, true)
		case PauseKindQuestion:
			if in.Dismiss {
				out := "error: the user could not answer: dismissed by the caller; proceed with reasonable assumptions and state them"
				a.finishCall(c, c.ID, tools.Result{Text: out, Err: out}, ToolStatusError, false)
				break
			}
			out, err := tools.MarshalAnswers(in.Answers)
			if err != nil {
				return fmt.Errorf("marshal answers: %w", err)
			}
			a.finishCall(c, c.ID, tools.Result{Text: out}, ToolStatusDone, false)
		case PauseKindApproval:
			if in.Approved && !in.Dismiss {
				a.execToolCall(turnCtx, c, c.ID)
				continue
			}
			a.finishCall(c, c.ID, tools.Result{Text: deniedResultText(in.Feedback)}, ToolStatusDenied, true)
		}
	}
	return nil
}
