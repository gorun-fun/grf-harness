// turn.go 核心回合循环：并入 user 输入（或恢复处置）→ LLM 请求 → 执行
// 工具调用 → 结果回传，直至产生最终回复或命中暂停点（转 pause.go 的
// 暂停/恢复协议）。RunTurn/RunTurnParts 是可暂停入口；run* 系列
// （run.go）经 runMessage 也走这里。
package agent

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"git.gorun.fun/share/grf-harness/llm"
	"git.gorun.fun/share/grf-harness/tools"
)

// imageRejectRe 匹配模型端点拒绝图片输入的错误文本（不同服务商措辞
// 不同，按关键词粗匹配；命中才触发历史图片降级重试）。
var imageRejectRe = regexp.MustCompile(`(?i)image|multimodal|vision|多模态|图片`)

// RunTurn 发起一个可暂停的回合（文本输入）。中途暂停时返回
// (nil, *PausedTurn, nil)；正常结束返回最终回复。
func (a *Agent) RunTurn(ctx context.Context, userInput string, human bool) (string, *PausedTurn, error) {
	return a.runTurn(ctx, &llm.Message{Role: "user", Content: userInput}, human, nil)
}

// RunTurnParts 多模态输入版本（文本 + 图像 parts）。
func (a *Agent) RunTurnParts(ctx context.Context, parts []llm.Part, human bool) (string, *PausedTurn, error) {
	return a.runTurn(ctx, &llm.Message{Role: "user", ContentParts: parts}, human, nil)
}

// runTurn 核心循环，支持两种进入方式：userMsg 非空 = 新回合；resume
// 非空 = 恢复（先把处置结果注入历史再续跑循环）。
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

	hooks := llm.StreamHooks{Content: a.onContent, Reasoning: a.onReason, ToolArgs: a.onToolArgs}

	for step := step0; step < a.MaxSteps(); step++ {
		// 通知并入（在 LLM 请求前，此时 tool 结果均已闭合）
		for _, n := range a.drainNotices() {
			a.mu.Lock()
			a.history = append(a.history, llm.Message{Role: "user", Content: "[system notice] " + n})
			a.mu.Unlock()
		}

		a.mu.Lock()
		snapshot := canonicalizeToolResults(sanitizeSnapshot(a.history))
		a.mu.Unlock()
		client := a.getClient() // 副本：流式调用不持锁，运行中切换模型不阻塞
		reply, err := client.ChatStream(turnCtx, snapshot, a.reg.Defs(), hooks)
		if err != nil {
			if errors.Is(err, context.Canceled) || turnCtx.Err() != nil {
				if userMsg != nil {
					a.rollbackOrphanUser(*userMsg)
				}
				return "", nil, context.Canceled
			}
			// 请求失败且错误指向图片输入、历史又带图片：模型端点不支持
			// image_url，且图片持久化在历史里会让本会话每次请求都重复
			// 同样的 400。降级图片为占位文本后原地重试一次；若仍失败按
			// 原错误返回（degradeImages 只在有图片可清时返回 true，不会
			// 无限循环；MaxSteps 兜底）。
			if imageRejectRe.MatchString(err.Error()) && a.degradeImages() {
				continue
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
					denied := deniedResultText(d.Feedback)
					a.emit("result", firstLine(denied))
					a.emitToolEvent(ToolEvent{CallID: callID, Name: call.Name(), Status: ToolStatusDenied,
						Input: call.Arguments(), Output: denied})
					a.appendToolResult(call, tools.Result{Text: denied})
					continue
				}
			}
			// 计划模式：改动类工具硬拦截（提示词软约束之外的兜底；子代理
			// 经 planGuard 跟随根），拒绝结果引导模型走调研→呈交计划路径
			if a.inPlan() && a.planBlocked(call.Name()) {
				denied := "[plan mode] 计划模式下禁止执行改动类工具（" + call.Name() +
					"）。请继续用只读工具（read/grep/glob 等）调研，并用 exit_plan_mode 呈交完整计划供用户审批"
				a.emit("result", firstLine(denied))
				a.emitToolEvent(ToolEvent{CallID: callID, Name: call.Name(), Status: ToolStatusDenied,
					Input: call.Arguments(), Output: denied})
				a.appendToolResult(call, tools.Result{Text: denied})
				continue
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

// deniedResultText 审批拒绝的工具结果文本（主循环与 applyResume 共用）。
func deniedResultText(feedback string) string {
	denied := "[denied] 用户在审批模式下拒绝了本次工具调用"
	if feedback != "" {
		denied += "，反馈: " + feedback
	}
	return denied + "；请换一种方式继续，或向用户说明并询问"
}

// planBlocked 计划模式下是否拦截该工具：内置改动类（planBlockTools）+
// 未标只读的插件工具（外部副作用无法静态判断，一律按改动类处理）。
func (a *Agent) planBlocked(name string) bool {
	if planBlockTools[name] {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.planExtra[name]
}

// planBlockTools 计划模式下硬拦截的改动/执行类工具。只读工具（read/
// grep/glob 等）与呈报类（exit_plan_mode/ask_user_question/todo_write）
// 放行；memory 同样放行——记忆是 harness 元数据而非工作区交付物，计划
// 阶段记下的用户偏好本就应立即沉淀；subagent 系放行（委派调研），其内部
// 调用同样受拦截。
var planBlockTools = map[string]bool{
	"write":    true,
	"edit":     true,
	"bash":     true,
	"ralph":    true,
	"workflow": true,
}
