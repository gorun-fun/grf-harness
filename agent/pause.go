// pause.go 回合暂停/恢复协议：OpenAI Responses 式接入（gh server）所需。
// 暂停 = 工具循环中途返回，历史尾部保留未闭合的 tool_call（OpenAI 线格式
// 的合法中间态）；恢复 = 注入对应结果后续跑循环。触发点三类：
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
	"time"

	"git.gorun.fun/share/grf-harness/llm"
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

// RunTurn 发起一个可暂停的回合（文本输入）。中途暂停时返回
// (nil, *PausedTurn, nil)；正常结束返回最终回复。
func (a *Agent) RunTurn(ctx context.Context, userInput string, human bool) (string, *PausedTurn, error) {
	return a.runTurn(ctx, &llm.Message{Role: "user", Content: userInput}, human, nil)
}

// RunTurnParts 多模态输入版本（文本 + 图像 parts）。
func (a *Agent) RunTurnParts(ctx context.Context, parts []llm.Part, human bool) (string, *PausedTurn, error) {
	return a.runTurn(ctx, &llm.Message{Role: "user", ContentParts: parts}, human, nil)
}

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

// runTurn 核心循环（runMessage 的完整实现，支持两种进入方式）：userMsg
// 非空 = 新回合；resume 非空 = 恢复（先把处置结果注入历史再续跑循环）。
func (a *Agent) runTurn(ctx context.Context, userMsg *llm.Message, human bool, resume []ResumeInput) (string, *PausedTurn, error) {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return "", nil, fmt.Errorf("agent is already running")
	}
	a.running = true
	if userMsg != nil {
		a.humanTurn = human
	}
	step0 := a.pausedSteps
	a.pausedSteps = 0
	turnCtx, cancel := context.WithCancel(ctx)
	a.turnCancel = cancel
	if userMsg != nil {
		a.history = append(a.history, *userMsg)
	}
	a.mu.Unlock()

	defer func() {
		cancel()
		a.mu.Lock()
		a.running = false
		a.turnCancel = nil
		a.mu.Unlock()
		a.emit("done", "")
	}()

	if resume != nil {
		if err := a.applyResume(turnCtx, resume); err != nil {
			return "", nil, err
		}
	}

	hooks := llm.StreamHooks{Content: a.onContent, Reasoning: a.onReason}

	for step := step0; step < a.MaxSteps(); step++ {
		// 通知并入（在 LLM 请求前，此时 tool 结果均已闭合）
		for _, n := range a.drainNotices() {
			a.mu.Lock()
			a.history = append(a.history, llm.Message{Role: "user", Content: "[system notice] " + n})
			a.mu.Unlock()
		}

		a.mu.Lock()
		snapshot := append([]llm.Message(nil), a.history...)
		a.mu.Unlock()
		reply, err := a.client.ChatStream(turnCtx, snapshot, a.reg.Defs(), hooks)
		if err != nil {
			if errors.Is(err, context.Canceled) || turnCtx.Err() != nil {
				if userMsg != nil {
					a.rollbackOrphanUser(*userMsg)
				}
				return "", nil, context.Canceled
			}
			if userMsg != nil {
				a.rollbackOrphanUser(*userMsg)
			}
			return "", nil, fmt.Errorf("llm: %w", err)
		}
		a.mu.Lock()
		a.history = append(a.history, reply)
		a.mu.Unlock()

		if len(reply.ToolCalls) == 0 {
			// 输出被 token 上限截断（常见于推理模型思考烧完预算、
			// 工具调用还没输出就断了）：不当作最终回复结束回合，提醒
			// 模型精简思考重发。MaxSteps 兜底防无限循环。
			if reply.FinishReason == "length" {
				if reply.Content == "" {
					a.EnqueueNotice("your previous reply was truncated by the output token limit before any tool call or final answer was produced. Re-issue the intended tool call now, with much shorter reasoning")
				} else {
					a.EnqueueNotice("your previous reply was truncated by the output token limit mid-sentence. Produce a concise, complete final answer now")
				}
				continue
			}
			// 存在已结束但从未读取结果的后台作业：提醒收割。每个会话
			// 只提醒一次（反复注入会干扰模型处理用户任务）；仍在运行
			// 的作业（如 dev server）与子代理不打扰——子代理落定有
			// 自己的完成通知。
			if !a.bgReminded && a.Jobs != nil && a.Jobs.NeedsAttention() {
				a.mu.Lock()
				a.bgReminded = true
				a.mu.Unlock()
				a.EnqueueNotice("some background jobs finished but their output was never read; collect any still-relevant results with job_output now, or ignore them if they no longer matter")
				continue
			}
			return reply.Content, nil, nil
		}

		var pending []PauseCall
		for _, call := range reply.ToolCalls {
			a.emit("tool", call.Name()+" "+llm.CompactArgs(call.Arguments(), 100))
			callID := call.ID
			if callID == "" {
				callID = fmt.Sprintf("call_gen_%d", toolEventSeq.Add(1))
			}
			a.emitToolEvent(ToolEvent{CallID: callID, Name: call.Name(), Status: ToolStatusRunning, Input: call.Arguments()})
			// 暂停协议：client 工具与 deferred 交互点不在本地执行，
			// 收集到轮末统一暂停（同轮普通工具照常执行）。
			if pc, deferIt := a.deferCall(call, callID); deferIt {
				pending = append(pending, pc)
				continue
			}
			// 审批模式：执行前征求用户同意（全量参数，不截断）；拒绝连同
			// 用户反馈作为工具结果回给模型
			if a.ApproveEach() && a.host != nil {
				if d := a.host.ApproveTool(turnCtx, call.Name(), call.Arguments()); !d.Approved {
					denied := "[denied] 用户在审批模式下拒绝了本次工具调用"
					if d.Feedback != "" {
						denied += "，反馈: " + d.Feedback
					}
					denied += "；请换一种方式继续，或向用户说明并询问"
					a.emit("result", firstLine(denied))
					a.emitToolEvent(ToolEvent{CallID: callID, Name: call.Name(), Status: ToolStatusDenied,
						Input: call.Arguments(), Output: denied})
					a.appendToolResult(call, tools.Result{Text: denied})
					continue
				}
			}
			a.execToolCall(turnCtx, call, callID)
		}
		if len(pending) > 0 {
			a.mu.Lock()
			a.pausedSteps = step + 1
			a.mu.Unlock()
			return "", &PausedTurn{Calls: pending, Steps: step + 1}, nil
		}
	}
	return "", nil, fmt.Errorf("reached max tool steps (%d), aborting", a.MaxSteps())
}

// execToolCall 执行一次工具调用并入历史（主循环与审批恢复共用）。
func (a *Agent) execToolCall(turnCtx context.Context, call llm.ToolCall, callID string) {
	t0 := time.Now()
	res := a.reg.Execute(turnCtx, call.Name(), call.Arguments())
	a.mu.Lock()
	a.toolCalls++
	a.toolTime += time.Since(t0)
	a.mu.Unlock()
	a.emit("result", firstLine(res.Text))
	status := ToolStatusDone
	if res.Err != "" {
		status = ToolStatusError
	}
	a.emitToolEvent(ToolEvent{CallID: callID, Name: call.Name(), Status: status,
		Input: call.Arguments(), Output: res.Text, Err: res.Err})
	a.appendToolResult(call, res)
}

// deferCall 判断一次工具调用是否转入暂停协议（client 工具 / deferred
// 交互点），是则返回暂停描述。
func (a *Agent) deferCall(call llm.ToolCall, callID string) (PauseCall, bool) {
	dh, ok := a.host.(tools.DeferredHost)
	deferred := ok && dh.DeferInteractions()
	pc := PauseCall{CallID: callID, Name: call.Name(), Arguments: call.Arguments()}
	if deferred && call.Name() == "ask_user_question" {
		pc.Kind = PauseKindQuestion
		return pc, true
	}
	if a.reg.IsClient(call.Name()) {
		pc.Kind = PauseKindFunction
		return pc, true
	}
	if deferred && a.ApproveEach() {
		pc.Kind = PauseKindApproval
		return pc, true
	}
	return PauseCall{}, false
}

// pausedCalls 定位最后一条带工具调用的 assistant 消息，返回其调用中尚未
// 被后续 tool 消息闭合的部分（即暂停待恢复的调用）。
func (a *Agent) pausedCalls() (llm.ToolCall, []llm.ToolCall) {
	a.mu.Lock()
	defer a.mu.Unlock()
	lastAsst := -1
	for i := len(a.history) - 1; i >= 0; i-- {
		if a.history[i].Role == "assistant" && len(a.history[i].ToolCalls) > 0 {
			lastAsst = i
			break
		}
	}
	if lastAsst < 0 {
		return llm.ToolCall{}, nil
	}
	closed := map[string]bool{}
	for _, m := range a.history[lastAsst+1:] {
		if m.Role == "tool" {
			closed[m.ToolCallID] = true
		}
	}
	var open []llm.ToolCall
	for _, c := range a.history[lastAsst].ToolCalls {
		if !closed[c.ID] {
			open = append(open, c)
		}
	}
	if len(open) == 0 {
		return llm.ToolCall{}, nil
	}
	return a.history[lastAsst].ToolCalls[0], open
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
		kind := PauseKindApproval
		if a.reg.IsClient(c.Name()) {
			kind = PauseKindFunction
		} else if c.Name() == "ask_user_question" {
			kind = PauseKindQuestion
		}
		switch kind {
		case PauseKindFunction:
			out := in.Output
			if in.Dismiss {
				out = "[client tool] the caller dismissed this call without executing it; proceed without it or offer an alternative"
			}
			a.emit("result", firstLine(out))
			a.emitToolEvent(ToolEvent{CallID: c.ID, Name: c.Name(), Status: ToolStatusDone,
				Input: c.Arguments(), Output: out})
			a.appendToolResult(c, tools.Result{Text: out})
		case PauseKindQuestion:
			if in.Dismiss {
				out := "error: the user could not answer: dismissed by the caller; proceed with reasonable assumptions and state them"
				a.emitToolEvent(ToolEvent{CallID: c.ID, Name: c.Name(), Status: ToolStatusError,
					Input: c.Arguments(), Output: out, Err: out})
				a.appendToolResult(c, tools.Result{Text: out, Err: out})
				break
			}
			out, err := tools.MarshalAnswers(in.Answers)
			if err != nil {
				return fmt.Errorf("marshal answers: %w", err)
			}
			a.emitToolEvent(ToolEvent{CallID: c.ID, Name: c.Name(), Status: ToolStatusDone,
				Input: c.Arguments(), Output: out})
			a.appendToolResult(c, tools.Result{Text: out})
		case PauseKindApproval:
			if in.Approved && !in.Dismiss {
				a.execToolCall(turnCtx, c, c.ID)
				continue
			}
			denied := "[denied] 用户在审批模式下拒绝了本次工具调用"
			if in.Feedback != "" {
				denied += "，反馈: " + in.Feedback
			}
			denied += "；请换一种方式继续，或向用户说明并询问"
			a.emit("result", firstLine(denied))
			a.emitToolEvent(ToolEvent{CallID: c.ID, Name: c.Name(), Status: ToolStatusDenied,
				Input: c.Arguments(), Output: denied})
			a.appendToolResult(c, tools.Result{Text: denied})
		}
	}
	return nil
}

// SetSystemText 运行期替换静态系统提示词段（Responses 请求级 instructions
// 生效用）。会改写请求前缀，提示词缓存将失效。
func (a *Agent) SetSystemText(text string) {
	if text == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.system = text
	if len(a.history) > 0 && a.history[0].Role == "system" {
		a.history[0].Content = text
	}
}
