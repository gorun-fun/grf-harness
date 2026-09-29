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
	"strings"
	"sync"
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

	// delivered 累计本次流式请求已投递给宿主的内容增量（llm 侧已过显示
	// 过滤，与用户看到的一致）。取消中断时把已交付前缀作为 interrupted
	// assistant 消息并入历史（dsh 语义）：下次请求包含用户看到的内容，
	// 续答不会从头复述。
	var delivered strings.Builder
	hooks := llm.StreamHooks{
		Content: func(delta string) {
			delivered.WriteString(delta)
			if a.onContent != nil {
				a.onContent(delta)
			}
		},
		Reasoning: a.onReason,
		ToolArgs:  a.onToolArgs,
	}

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
		delivered.Reset()
		reply, err := client.ChatStream(turnCtx, snapshot, a.reg.Defs(), hooks)
		if err != nil {
			if errors.Is(err, context.Canceled) || turnCtx.Err() != nil {
				a.keepDeliveredPrefix(delivered.String())
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
			// 自己的完成通知。bgReminded 仅回合 goroutine 访问，不加锁。
			if !a.bgReminded && a.Jobs != nil && a.Jobs.NeedsAttention() {
				a.bgReminded = true
				a.EnqueueNotice("some background jobs finished but their output was never read; collect any still-relevant results with job_output now, or ignore them if they no longer matter")
				continue
			}
			return reply.Content, nil, nil
		}

		pending := a.dispatchBatch(turnCtx, reply.ToolCalls)
		if len(pending) > 0 {
			a.mu.Lock()
			a.pausedSteps = step + 1
			a.mu.Unlock()
			return "", &PausedTurn{Calls: pending, Steps: step + 1}, nil
		}
	}
	return "", nil, fmt.Errorf("reached max tool steps (%d), aborting", a.MaxSteps())
}

// parallelSafeTools 可与同级调用并行执行的工具：全部为只读且无跨调用
// 游标/会话状态（dsh 的 executionMode 分类语义——"安全性取决于比较同级
// 调用或资源的调用必须保持独占"：job_output 有读取游标、memory 有写
// 操作、bash/write/edit 有真实副作用，均不在列）。自定义/插件工具与
// client 工具一律按独占处理（副作用无法静态判断）。
var parallelSafeTools = map[string]bool{
	"read":       true,
	"read_image": true,
	"glob":       true,
	"grep":       true,
	"web_search": true,
	"skill":      true,
	"job_list":   true,
	"list_agents": true,
	"get_goal":   true,
}

// dispatchBatch 按模型顺序处理一批 tool_call：连续的并行安全工具成组
// 送入有界并发池（上限 MaxParallelTools），独占类与暂停类调用单独串行
// 执行并构成排序屏障（独占调用保序，dsh 语义）。返回转入暂停协议的调用。
// 结果并入历史的完成顺序与模型顺序无关——请求快照经
// canonicalizeToolResults 按发起 assistant 的 tool_calls 顺序重排闭合，
// 供应商侧线格式始终合法。回合已取消时不再分组：所有调用逐个走
// dispatchCall → execToolCall 的 aborted-before-dispatch 合成结果，
// 保证每个 tool_call 都被结果闭合。
func (a *Agent) dispatchBatch(turnCtx context.Context, calls []llm.ToolCall) []PauseCall {
	var (
		pending []PauseCall
		pendMu  sync.Mutex
	)
	pend := func(pc PauseCall, deferred bool) {
		if !deferred {
			return
		}
		pendMu.Lock()
		pending = append(pending, pc)
		pendMu.Unlock()
	}
	var group []llm.ToolCall
	flush := func() {
		if len(group) == 0 {
			return
		}
		g := group
		group = nil
		if len(g) == 1 {
			pc, d := a.dispatchCall(turnCtx, g[0])
			pend(pc, d)
			return
		}
		sem := make(chan struct{}, a.MaxParallelTools())
		var wg sync.WaitGroup
		for _, c := range g {
			wg.Add(1)
			sem <- struct{}{}
			go func(c llm.ToolCall) {
				defer wg.Done()
				defer func() { <-sem }()
				pc, d := a.dispatchCall(turnCtx, c)
				pend(pc, d)
			}(c)
		}
		wg.Wait()
	}
	for _, call := range calls {
		a.trackRepeat(call) // streak 计数只在回合 goroutine，无并发
		if parallelSafeTools[call.Name()] && turnCtx.Err() == nil {
			group = append(group, call)
			continue
		}
		flush() // 屏障：等本组全部落定再独占执行
		pc, d := a.dispatchCall(turnCtx, call)
		pend(pc, d)
	}
	flush()
	return pending
}

// repeatRemindThresholds 连续完全相同（工具名+参数）调用的递进提醒阈值
//（dsh repeat-tool-reminder：thresholds [3,5,8]）。只在恰好命中阈值时
// 提醒一次，超过最高阈值不再重复。
var repeatRemindThresholds = []int{3, 5, 8}

// trackRepeat 更新"连续相同调用"计数并在命中阈值时入队提醒（防模型
// 卡在同一失败调用上空转）。换任何一次不同调用即清零。仅回合 goroutine
// 调用（dispatchBatch），与 execToolCall 的执行结果无关——重复本身即是
// 信号，无论成败。
func (a *Agent) trackRepeat(call llm.ToolCall) {
	key := call.Name() + "\x00" + call.Arguments()
	if key == a.repeatKey {
		a.repeatCount++
	} else {
		a.repeatKey, a.repeatCount = key, 1
	}
	for _, th := range repeatRemindThresholds {
		if a.repeatCount != th {
			continue
		}
		switch th {
		case repeatRemindThresholds[0]:
			a.EnqueueNotice(fmt.Sprintf("you just called %s %d times in a row with identical arguments. If it keeps failing or changes nothing, repeating it unchanged will not help — analyze the latest result above and change the arguments or the approach", call.Name(), th))
		case repeatRemindThresholds[1]:
			a.EnqueueNotice(fmt.Sprintf("this is the %dth identical %s call in a row. Do not issue the same call again — it will keep producing the same result. Change strategy, use a different tool, or report the blocker", th, call.Name()))
		default:
			a.EnqueueNotice(fmt.Sprintf("the identical %s call has now been made %d times. Stop repeating it: move on to different work, or ask the user how to proceed with ask_user_question", call.Name(), th))
		}
		break
	}
}

// 工具结果裁剪网（dsh tool-result-pruner 语义）：超过阈值的工具结果在
// 进入历史前保留头尾、省略中段，防止单次超大输出挤占上下文。bash/glob/
// grep 等工具自身的截断+全文落盘（Artifacts）先行发生，这里是对全部
// 工具（含嵌入方自定义工具）统一的最后一道网；图像结果不受影响。
const (
	pruneThreshold = 8192 // 触发阈值（字节）
	pruneHead      = 4096 // 保留头部
	pruneTail      = 1024 // 保留尾部
)

// pruneResultText 头尾保留式裁剪；未超阈值原样返回。切点对齐 UTF-8
// 边界（不劈开多字节字符）：切点后首字节是续字节说明落在了字符中间，
// 回退到该字符首字节之前（整个字符归入省略段）。
func pruneResultText(s string) string {
	if len(s) <= pruneThreshold {
		return s
	}
	h := pruneHead
	for h > 0 && !utf8RuneStart(s[h]) {
		h--
	}
	head := s[:h]
	tail := s[len(s)-pruneTail:]
	for len(tail) > 0 && !utf8RuneStart(tail[0]) {
		tail = tail[1:]
	}
	omitted := len(s) - len(head) - len(tail)
	return head + fmt.Sprintf("\n…[middle omitted: %d characters — page through with the tool's offset/limit parameters, or read the full-output file when the result names one]…\n", omitted) + tail
}

// utf8RuneStart 字节是否为 UTF-8 序列首字节（10xxxxxx 为续字节）。
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// abortedBeforeDispatchText 回合取消时尚未开始的调用的合成结果文本
//（dsh ABORTED_BEFORE_DISPATCH）：调用从未运行、无任何效果，可直接
// 重发——与 outcome-unknown（见 canonicalizeToolResults 占位）相区分。
const abortedBeforeDispatchText = "[aborted before dispatch] the turn was cancelled before this tool call started, so it never ran and had no effect; re-issue it now if its outcome is still needed"

// keepDeliveredPrefix 取消中断的流式回复：把已投递给宿主的前缀并入历史
//（dsh interrupted 锚点语义）。空前缀不并入（sanitizeSnapshot 会剔除空
// assistant 残留，这里同样不制造）。
func (a *Agent) keepDeliveredPrefix(delivered string) {
	c := strings.TrimRight(delivered, " \n\t")
	if c == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.history = append(a.history, llm.Message{Role: "assistant",
		Content: c + "\n\n[interrupted: this reply was cut off mid-stream by a cancellation; the text above was already shown to the user — do not repeat it verbatim, continue from where it stopped]"})
}

// finishCall 以给定终态收尾一次未在本地执行的工具调用：可选的 "result"
// 行事件 + 结构化工具事件 + 结果并入历史。拒绝路径（审批拒绝 / 计划拦截 /
// 恢复侧审批拒绝）与暂停恢复的应答回填（client 工具结果、ask_user 应答）
// 共用。emitResult 控制是否发 "result" 行事件——ask_user 的应答是交互
// 结果而非工具输出行，不发（执行路径 execToolCall 的行事件语义不变）。
func (a *Agent) finishCall(call llm.ToolCall, callID string, res tools.Result, status string, emitResult bool) {
	res.Text = pruneResultText(res.Text)
	if emitResult {
		a.emit("result", firstLine(res.Text))
	}
	a.emitToolEvent(ToolEvent{CallID: callID, Name: call.Name(), Status: status,
		Input: call.Arguments(), Output: res.Text, Err: res.Err})
	a.appendToolResult(call, res)
}

// dispatchCall 处理单条 tool_call（runTurn 主循环的 per-call 下沉段），
// 四段式：事件（行 + 结构化 running）→ 暂停判定（deferCall）→ 审批模式
// 征求 → 计划模式拦截 → 本地执行（execToolCall）。deferIt=true 表示该
// 调用转入暂停协议、返回其暂停描述——不在本地执行，由主循环收集到轮末
// 统一暂停（同轮普通工具照常执行），语义见 runTurn 的 pending 段。
func (a *Agent) dispatchCall(turnCtx context.Context, call llm.ToolCall) (PauseCall, bool) {
	a.emit("tool", call.Name()+" "+llm.CompactArgs(call.Arguments(), 100))
	callID := call.ID
	if callID == "" {
		callID = fmt.Sprintf("call_gen_%d", toolEventSeq.Add(1))
	}
	a.emitToolEvent(ToolEvent{CallID: callID, Name: call.Name(), Status: ToolStatusRunning, Input: call.Arguments()})
	if pc, deferIt := a.deferCall(call, callID); deferIt {
		return pc, true
	}
	// 审批模式：执行前征求用户同意（全量参数，不截断）；拒绝连同
	// 用户反馈作为工具结果回给模型
	if a.ApproveEach() && a.host != nil {
		if d := a.host.ApproveTool(turnCtx, call.Name(), call.Arguments()); !d.Approved {
			a.finishCall(call, callID, tools.Result{Text: deniedResultText(d.Feedback)}, ToolStatusDenied, true)
			return PauseCall{}, false
		}
	}
	// 计划模式：改动类工具硬拦截（提示词软约束之外的兜底；子代理
	// 经 planGuard 跟随根），拒绝结果引导模型走调研→呈交计划路径。
	// 语义对齐 dsh：用户的对话式同意不构成批准——计划只能经
	// exit_plan_mode 的评审获得批准。
	if a.inPlan() && a.planBlocked(call.Name()) {
		denied := "[plan mode] 计划模式下禁止执行改动类工具（" + call.Name() +
			"）。继续用只读工具（read/grep/glob 等）把计划落到真实代码库上，" +
			"然后把决策完备的计划经 exit_plan_mode 呈交审批；不要用正文或 " +
			"ask_user_question 征求\"可以开始吗\"——用户在对话里的同意不构成批准，" +
			"批准只发生在 exit_plan_mode 的评审里（被拒会带回反馈，修改后重新呈交）"
		a.finishCall(call, callID, tools.Result{Text: denied}, ToolStatusDenied, true)
		return PauseCall{}, false
	}
	a.execToolCall(turnCtx, call, callID)
	return PauseCall{}, false
}

// execToolCall 执行一次工具调用并入历史（主循环与审批恢复共用）。
// 回合已取消时不再启动执行：合成 aborted-before-dispatch 结果闭合调用
//（无效果、可重发），避免"取消后仍真的执行副作用"与含混的 ctx 错误文本。
func (a *Agent) execToolCall(turnCtx context.Context, call llm.ToolCall, callID string) {
	t0 := time.Now()
	var res tools.Result
	if err := turnCtx.Err(); err != nil {
		res = tools.Result{Text: abortedBeforeDispatchText, Err: "aborted before dispatch"}
	} else {
		res = a.reg.Execute(turnCtx, call.Name(), call.Arguments())
	}
	res.Text = pruneResultText(res.Text)
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
	kind := pauseKindOf(a.reg, call.Name())
	// client 工具恒转暂停；question 与 approval 类只在 deferred 宿主下
	// 转暂停（非 deferred 的 ask_user 由本地工具处理器同步问用户）。
	// ask_user_question 不可能是 client 工具（RegisterClient 拒绝与
	// spec 冲突的名字），推断顺序不存在歧义。
	switch {
	case kind == PauseKindFunction:
		pc.Kind = kind
		return pc, true
	case deferred && kind == PauseKindQuestion:
		pc.Kind = kind
		return pc, true
	case deferred && a.ApproveEach():
		pc.Kind = PauseKindApproval
		return pc, true
	}
	return PauseCall{}, false
}

// pauseKindOf 按注册表与工具名推断暂停类别（deferCall 暂停时与
// applyResume 恢复时共用）。注意：恢复侧只按名字重推断是有意的——
// 跨重启恢复时 DeferredHost 的运行时状态（DeferInteractions 等）不可
// 靠，暂停事实已落在持久化历史的未闭合 tool_call 里，类别按注册关系
// 与名字即可还原。
func pauseKindOf(reg *tools.Registry, name string) string {
	if reg.IsClient(name) {
		return PauseKindFunction
	}
	if name == "ask_user_question" {
		return PauseKindQuestion
	}
	return PauseKindApproval
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
