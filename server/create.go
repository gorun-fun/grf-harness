// create.go POST /v1/responses 主端点：参数解析与会话定位、请求级配置
// 应用、input 解析（文本/多模态/回传 item/交互应答）、回合执行（同步、
// SSE 流式、background）、终态拼装与落盘。ORS 合规核心路径。
package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"git.gorun.fun/share/grf-harness/agent"
	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/llm"
	"git.gorun.fun/share/grf-harness/term"
	"git.gorun.fun/share/grf-harness/tools"
)

// handleCreate POST /v1/responses。
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if !decodeJSON(w, r, &req, bodyLimitMain) {
		return
	}

	sess, ok := s.locateSession(w, &req)
	if !ok {
		return
	}
	claim, ok := s.claimTurn(w, sess)
	if !ok {
		return
	}
	handoff := false
	defer func() {
		if !handoff {
			sess.releaseClaim(claim)
		}
	}()

	// grf:history 截断（编辑重发）：在解析 input 与暂停判定之前替换
	// 历史，使 paused 状态按覆写后的历史计算（截掉未闭合 tool_call 即
	// 解除暂停，随后的普通文本输入即可继续）。
	if len(req.GrfHistory) > 0 {
		if err := sess.overrideHistory(req.GrfHistory); err != nil {
			writeError(w, 409, errInvalid, "grf:history: "+err.Error(), "grf:history")
			return
		}
	}

	if !s.applyRequestConfig(w, sess, &req) {
		return
	}

	// input 解析：文本/图像、回传历史 item、交互应答。
	text, images, resume, inject, ferr := parseInput(&req)
	if ferr != nil {
		writeError(w, 400, errInvalid, ferr.Error(), "input")
		return
	}
	if !validatePausedState(w, sess, text, resume, inject) {
		return
	}
	// 回传历史 item 并入会话历史（新输入之前）。
	if len(inject) > 0 {
		sess.root.RestoreFull(append(sess.root.History(), inject...), sess.root.SystemText())
	}

	store := req.Store == nil || *req.Store
	if req.Stream && req.Background {
		writeError(w, 400, errInvalid, "background and stream cannot be combined on this server (background results are retrieved via GET /v1/responses/{id})", "stream")
		return
	}

	respID := sess.nextResp()
	if req.Stream {
		s.runStreaming(w, r, sess, &req, respID, claim, text, images, resume, store)
		return
	}
	if req.Background {
		handoff = true // 占用随会话移交后台 goroutine，由其 execute 释放
		s.runBackground(w, sess, &req, respID, claim, text, images, resume, store)
		return
	}
	res, execErr := s.execute(r.Context(), sess, &req, respID, claim, text, images, resume, store, nil, nil)
	if execErr != nil {
		writeError(w, 400, errInvalid, execErr.Error(), "input")
		return
	}
	writeJSON(w, 200, res)
}

// locateSession 会话定位：previous_response_id 链尾校验 / 懒创建。
// 失败已写响应，返回 false。
func (s *Server) locateSession(w http.ResponseWriter, req *createRequest) (*session, bool) {
	if req.PreviousResponseID != "" {
		sid, _, err := parseRespID(req.PreviousResponseID)
		if err != nil {
			writeError(w, 400, errInvalid, "malformed previous_response_id", "previous_response_id")
			return nil, false
		}
		sess, err := s.getSession(sid)
		if err != nil {
			writeJSONError(w, err)
			return nil, false
		}
		if cur := sess.curRespID(); cur != req.PreviousResponseID {
			writeError(w, 409, errInvalid,
				fmt.Sprintf("previous_response_id %s is not the latest response (chain tail is %s); forking history is not supported",
					req.PreviousResponseID, cur), "previous_response_id")
			return nil, false
		}
		return sess, true
	}
	sess, err := s.newSession(req.GrfSession)
	if err != nil {
		writeError(w, 500, errServer, "create session: "+err.Error(), "")
		return nil, false
	}
	return sess, true
}

// claimTurn 占用回合槽：上面的预检与 execute 的 beginTurn 之间隔着请求级
// 配置、input 解析与斜杠指令（/compact 可达秒级），两个并发请求可同时
// 通过预检——漏网时 409 退化为 200+failed 且推进链尾。tryClaim 原子完成
// "检查+占用"；execute 的各出口按占用代号释放，本地 defer 兜底
// execute 未被调用的早退路径（代号匹配保证不误伤后续占用），background
// 模式随会话移交后台 goroutine。失败已写响应，返回 false。
func (s *Server) claimTurn(w http.ResponseWriter, sess *session) (uint64, bool) {
	if sess.root.IsRunning() {
		writeError(w, 409, errInvalid, "a turn is already running in this session; cancel it (POST /v1/responses/{id}/cancel) or retry later", "")
		return 0, false
	}
	claim, ok := sess.tryClaim()
	if !ok {
		writeError(w, 409, errInvalid, "a turn is already running in this session; cancel it (POST /v1/responses/{id}/cancel) or retry later", "")
		return 0, false
	}
	return claim, true
}

// applyRequestConfig 请求级配置：instructions / max_tool_calls /
// prompt_cache_key / model（即时切换）/ reasoning / tools。失败已写
// 响应，返回 false。
func (s *Server) applyRequestConfig(w http.ResponseWriter, sess *session, req *createRequest) bool {
	if req.Instructions != nil && *req.Instructions != "" {
		sess.root.SetSystemText(*req.Instructions)
	}
	if req.MaxToolCalls != nil && *req.MaxToolCalls >= 1 {
		sess.root.SetMaxSteps(*req.MaxToolCalls)
	}
	if req.PromptCacheKey != "" {
		sess.h.SetCacheKey(req.PromptCacheKey)
	}
	if req.Model != "" {
		s.trySwitchModel(sess, req.Model)
	}
	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		// 思考强度：作用在当前（可能刚切换的）client 上；换模型重建
		// client 后由后续请求重设。
		sess.root.SetReasoningEffort(req.Reasoning.Effort)
	}
	if err := s.applyTools(sess, req); err != nil {
		writeError(w, 400, errInvalid, err.Error(), "tools")
		return false
	}
	return true
}

// validatePausedState 暂停态输入校验：暂停中只收应答 item、非暂停不收
// 应答 item。失败已写响应，返回 false。
func validatePausedState(w http.ResponseWriter, sess *session, text string, resume []agent.ResumeInput, inject []llm.Message) bool {
	paused := sess.root.HasPausedCalls()
	switch {
	case paused && len(resume) == 0:
		writeError(w, 400, errInvalid,
			"this session is paused awaiting interaction; submit grf:input_result (or function_call_output) items as input", "input")
	case !paused && len(resume) > 0:
		writeError(w, 400, errInvalid, "no paused tool calls in this session; grf:input_result items are only valid while paused", "input")
	case paused && (text != "" || len(inject) > 0):
		writeError(w, 400, errInvalid, "session is paused; new input must only contain grf:input_result / function_call_output items", "input")
	default:
		return true
	}
	return false
}

// execute 回合执行：接线事件桥、跑 RunTurn/Resume、拼终态资源。
// sink 非空时事件实时流出（SSE）；onStart 在回合开跑前回调一次
// （流式模式借此发 response.created/in_progress）。claim 是 handleCreate
// 占用的回合槽代号，各出口据此释放。返回的 error 非空表示调用方输入
// 问题（agent.ErrResumeInput 类），应映射 400、不生成资源；执行类失败
// 进资源的 failed 终态。
func (s *Server) execute(ctx context.Context, sess *session, req *createRequest, respID string, claim uint64,
	text string, images []tools.Image, resume []agent.ResumeInput, store bool,
	sink func(*event), onStart func(base *ResponseResource)) (*ResponseResource, error) {

	// 供应商同步热生效：未显式指定 model 的会话在回合边界跟随
	// settings.json 的新默认——宿主（openos AISyncer 等）直接落盘改配置，
	// 存量会话与常驻进程无需重启/空闲回收即可生效（旧口径"gh 仅启动时
	// 读盘"的竞态消除）。显式 model 由 handleCreate 的 trySwitchModel
	// 处理（本就即时读盘）。
	s.refreshDefaultClient(sess, req.Model)

	base := s.buildResource(sess, req, respID)
	if onStart != nil {
		onStart(base)
	}
	pre := sess.root.LLMStats()

	// 斜杠指令拦截（grf-harness 会话级行为，不经模型或由 harness 定义任务）：
	// 纯本地指令直接完成回合；任务型指令改写输入后继续正常执行。
	if resume == nil && len(images) == 0 && isSlashCommand(text) {
		out, rewrite, handled, cerr := s.runSlashCommand(ctx, sess, text)
		if cerr != nil {
			sess.rollbackResp(respID)
			sess.releaseClaim(claim) // 斜杠路径未走 beginTurn，这里释放（background 场景无人兜底）
			return nil, cerr
		}
		if handled {
			now := time.Now().Unix()
			base.CompletedAt = &now
			base.Status = statusCompleted
			base.Output = []Item{{
				Type: itemMessage, ID: "it_1", Status: "completed", Role: "assistant",
				Content: []contentPart{{Type: "output_text", Text: out}},
			}}
			base.Usage = usageFrom(sess.root.LLMStats(), pre)
			s.persistTurn(sess, base, store)
			sess.releaseClaim(claim)
			return base, nil
		}
		text = rewrite
	}

	br := newBridge(sink, sess.nextSeq)
	sess.root.SetStreamHooks(br.OnContent, br.OnReasoning)
	sess.root.SetToolArgsHook(br.OnToolArgs)
	sess.root.SetOnToolEvent(br.OnToolEvent)
	sess.root.SetOnEvent(br.OnNotice)

	tctx, cancel := context.WithCancel(ctx)
	sess.beginTurn(cancel)
	var reply string
	var pausedTurn *agent.PausedTurn
	var runErr error
	if resume != nil {
		reply, pausedTurn, runErr = sess.root.Resume(tctx, resume)
	} else if len(images) > 0 {
		parts := []llm.Part{llm.TextPart(text)}
		for _, img := range images {
			parts = append(parts, llm.ImagePart(img.Mime, img.B64))
		}
		reply, pausedTurn, runErr = sess.root.RunTurnParts(tctx, parts, true)
	} else {
		reply, pausedTurn, runErr = sess.root.RunTurn(tctx, text, true)
	}
	// 外部取消要在自身 cancel() 之前采样（否则 tctx.Err() 恒真，
	// 一切失败都会被误判为 cancelled）。
	extCanceled := ctx.Err() != nil
	sess.endTurn(claim)
	cancel()

	// 调用方输入校验失败：不生成终态资源（链尾不动，客户端可用原
	// previous_response_id 重试）。
	if runErr != nil && errors.Is(runErr, agent.ErrResumeInput) {
		sess.rollbackResp(respID)
		return nil, runErr
	}

	now := time.Now().Unix()
	base.CompletedAt = &now
	base.Usage = usageFrom(sess.root.LLMStats(), pre)
	finalizeStatus(base, reply, pausedTurn, runErr, br, extCanceled)
	s.persistTurn(sess, base, store)
	return base, nil
}

// finalizeStatus 回合终态判定：暂停（含交互请求 / 纯客户端函数暂停）、
// 完成、取消、max steps 超限、失败五态写进 base。
func finalizeStatus(base *ResponseResource, reply string, pausedTurn *agent.PausedTurn, runErr error, br *bridge, extCanceled bool) {
	switch {
	case runErr == nil && pausedTurn != nil:
		hasInteraction, reqs := br.pausedItems(pausedTurn)
		base.Output = br.finish("")
		if hasInteraction {
			base.Status = statusIncomplete
			base.IncompleteDetails = &IncompleteDetails{Reason: inputRequired, Requests: reqs}
		} else {
			base.Status = statusCompleted // 纯客户端函数暂停：ORS function calling 形状
		}
	case runErr == nil:
		base.Status = statusCompleted
		base.Output = br.finish(reply)
	default:
		if errors.Is(runErr, context.Canceled) || extCanceled {
			base.Status = statusCancelled
			base.Output = br.finish("")
		} else if strings.Contains(runErr.Error(), "max tool steps") {
			base.Status = statusIncomplete
			base.IncompleteDetails = &IncompleteDetails{Reason: "max_tool_calls"}
			base.Output = br.finish("")
		} else {
			base.Status = statusFailed
			base.Error = &ErrorPayload{Type: "model_error", Code: nil, Message: runErr.Error()}
			base.Output = br.finish("")
		}
	}
}

// persistTurn 回合终态落盘：缓存键随历史一起存档，快照供会话详情端点。
func (s *Server) persistTurn(sess *session, base *ResponseResource, store bool) {
	if store {
		sess.h.Store.SetCacheKey(sess.h.CacheKey())
		_, _ = sess.h.Store.SaveSession(sess.root.History())
		sess.saveSnap(base)
	}
}

// runStreaming SSE 流式模式。
func (s *Server) runStreaming(w http.ResponseWriter, r *http.Request, sess *session,
	req *createRequest, respID string, claim uint64, text string, images []tools.Image,
	resume []agent.ResumeInput, store bool) {

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, errServer, "streaming unsupported", "")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)

	// send 并发前提（不加锁的依据）：llm 层 stream hooks 在 SSE 读取循环
	// 内同步调用（llm/client.go chatStreamOnce 的 scanner 循环），
	// RunTurn 返回后不再有任何回调——因此回合 goroutine 内经事件桥到
	// send 的写出，与 HTTP goroutine 自身的 send（onStart 在回合开跑前、
	// 终态/错误事件在 execute 返回后）先后不重叠，w 无需加锁。若 llm 层
	// 改为异步投递增量，需重新评估此处的并发安全（http.ResponseWriter
	// 非并发安全）。
	send := func(ev *event) {
		data, _ := json.Marshal(ev)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data)
		flusher.Flush()
	}

	res, execErr := s.execute(r.Context(), sess, req, respID, claim, text, images, resume, store, send,
		func(base *ResponseResource) {
			started := shallowCopy(base)
			started.Status = statusInProgress
			send(&event{Type: "response.created", SequenceNumber: sess.nextSeq(), Response: started})
			send(&event{Type: "response.in_progress", SequenceNumber: sess.nextSeq(), Response: started})
		})
	if execErr != nil {
		// 流已开（200），无法再改状态码：按 ORS 以错误事件收场。
		send(&event{Type: "error", SequenceNumber: sess.nextSeq(),
			Error: &ErrorPayload{Type: errInvalid, Message: execErr.Error(), Param: "input"}})
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}
	send(&event{Type: terminalEvent(res.Status), SequenceNumber: sess.nextSeq(), Response: res})
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// runBackground 后台模式：立即返回 queued，goroutine 执行，retrieve 取终态。
// 回合槽占用（claim）随本函数移交给后台 goroutine，由其 execute 释放。
func (s *Server) runBackground(w http.ResponseWriter, sess *session,
	req *createRequest, respID string, claim uint64, text string, images []tools.Image,
	resume []agent.ResumeInput, store bool) {

	base := s.buildResource(sess, req, respID)
	base.Status = statusQueued
	writeJSON(w, 200, base)
	go func() {
		res, execErr := s.execute(context.Background(), sess, req, respID, claim, text, images, resume, store, nil, nil)
		if execErr != nil {
			// 校验类失败：不落终态快照，retrieve 持续返回 queued 之外的
			// 404/在跑态之外——写入一个 failed 快照让轮询方拿到明确结果。
			now := time.Now().Unix()
			res = shallowCopy(base)
			res.Status = statusFailed
			res.CompletedAt = &now
			res.Error = &ErrorPayload{Type: errInvalid, Message: execErr.Error(), Param: "input"}
			sess.saveSnap(res)
		}
		// 回合终态事件外发（宿主无头编排：事件驱动收尾，轮询降级为兜底）。
		s.hook.postTurn(res)
	}()
}

// terminalEvent 终态对应的事件名。
func terminalEvent(status string) string {
	switch status {
	case statusFailed:
		return "response.failed"
	case statusIncomplete:
		return "response.incomplete"
	case statusCancelled:
		return "response.cancelled"
	}
	return "response.completed"
}

// shallowCopy 值拷贝资源（流式 started 快照用，Output 独立置空）。
func shallowCopy(r *ResponseResource) *ResponseResource {
	c := *r
	c.Output = []Item{}
	return &c
}

// buildResource 组装回显资源的静态段（终态字段由 execute 填）。
func (s *Server) buildResource(sess *session, req *createRequest, respID string) *ResponseResource {
	store := req.Store == nil || *req.Store
	bg := req.Background
	trunc := req.Truncation
	if trunc == "" {
		trunc = "auto"
	}
	return &ResponseResource{
		ID: respID, Object: "response", CreatedAt: time.Now().Unix(),
		// model 回报全名 "provider/model"：客户端可原样回传请求体切换模型；
		// 裸名会被 trySwitchModel 忽略，曾导致 UI 侧模型继承静默失效。
		Status: statusInProgress, Model: sess.root.ModelRef(),
		PreviousResponseID: req.PreviousResponseID, Instructions: req.Instructions,
		Output: []Item{}, Tools: req.Tools, ToolChoice: rawOrNull(req.ToolChoice),
		Truncation: trunc, ParallelToolCalls: req.ParallelToolCalls,
		Text:        &TextConfig{Format: "text"},
		Temperature: req.Temperature, TopP: req.TopP,
		MaxOutputTokens: req.MaxOutputTokens, MaxToolCalls: req.MaxToolCalls,
		Store: &store, Background: &bg, ServiceTier: req.ServiceTier,
		Metadata: req.Metadata, SafetyIdentifier: req.SafetyIdentifier,
		PromptCacheKey: req.PromptCacheKey, SessionID: sess.meta.ID,
	}
}

func rawOrNull(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return json.RawMessage(raw)
}

// usageFrom 回合用量 = 终态累计 - 回合前累计。
func usageFrom(post, pre llm.StatsSnapshot) *Usage {
	in := post.PromptTokens - pre.PromptTokens
	out := post.CompletionTokens - pre.CompletionTokens
	if in < 0 {
		in = 0
	}
	if out < 0 {
		out = 0
	}
	cached := post.CachedTokens - pre.CachedTokens
	if cached < 0 {
		cached = 0
	}
	return &Usage{
		InputTokens: int(in), OutputTokens: int(out), TotalTokens: int(in + out),
		InputTokensDetails:  &usageTokenKind{CachedTokens: int(cached)},
		OutputTokensDetails: &usageTokenKind{},
	}
}

// applyTools 请求级工具配置：注册 client 函数工具、应用 tool_choice 裁剪。
// tools 缺省（JSON 字段不存在）时沿用上轮注册的 client 工具——续交请求
// 通常不带 tools，清掉会导致暂停类别误判；显式空数组 = 清空。
func (s *Server) applyTools(sess *session, req *createRequest) error {
	reg := sess.root.Reg()
	reg.EnableAll()
	if req.Tools != nil {
		for _, name := range sess.clientTools { // 清理上一轮注册的 client 工具
			reg.Disable(name)
		}
		sess.clientTools = nil
		sess.meta.ClientTools = nil
		for _, t := range req.Tools {
			if t.Type != "function" {
				continue // 非 function 类型（含其它厂商前缀）宽容跳过
			}
			if t.Name == "" {
				return fmt.Errorf("function tool needs a name")
			}
			reg.RegisterClient(llm.Tool{Name: t.Name, Description: t.Description, Parameters: t.Parameters})
			sess.clientTools = append(sess.clientTools, t.Name)
			sess.meta.ClientTools = append(sess.meta.ClientTools, toolDef{
				Name: t.Name, Description: t.Description, Parameters: t.Parameters,
			})
		}
		_ = writeMeta(s.convDir(sess.meta.ID), sess.meta) // 持久化（暂停分类跨重启依赖）
	}
	// tool_choice：none 禁全部；allowed_tools 变体禁未列出的（保缓存语义）。
	choice := strings.TrimSpace(string(req.ToolChoice))
	switch {
	case choice == `"none"`:
		reg.Disable(reg.Registered()...)
	case strings.Contains(choice, `"allowed_tools"`):
		var ac struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(req.ToolChoice, &ac); err != nil {
			return fmt.Errorf("parse allowed_tools: %w", err)
		}
		allowed := map[string]bool{}
		for _, t := range ac.Tools {
			allowed[t.Name] = true
		}
		var disable []string
		for _, n := range reg.Registered() {
			if !allowed[n] {
				disable = append(disable, n)
			}
		}
		reg.Disable(disable...)
	}
	return nil
}

// trySwitchModel model 参数 best-effort：provider/model 匹配配置则切换，
// 否则沿用当前（保证任意 model 值可跑，ORS 合规测试语义）。
// 解析统一走 config.Settings.ResolveModel（限定名校验供应商与模型表；
// 裸模型名默认供应商优先、否则恰一家提供时用它；歧义/零命中忽略，不
// 静默换到非预期供应商）——宿主（如 openos 的 resolveModel shim、会话
// 模型继承）不需要自行补全限定名。
func (s *Server) trySwitchModel(sess *session, model string) {
	cfg, err := config.LoadFileForUpdate()
	if err != nil || cfg == nil {
		return
	}
	provider, name, ok := cfg.ResolveModel(model)
	if !ok {
		return
	}
	p := cfg.Providers[provider]
	nc := llm.NewFromProvider(provider, *p, name)
	// 缓存键迁移 + 子代理 client + 系统提示词身份行重写（身份行是会话构建期
	// 烘焙的，切换须同步，否则模型以旧身份自我认知、自称旧模型名）。
	sess.h.SwitchClient(nc)
	// 切换即读盘：client 与最新 settings.json 对齐，同步指纹避免下一回合
	// 被 refreshDefaultClient 误判"配置又变了"再切回默认。
	sess.clientFP = settingsFingerprint()
}

// refreshDefaultClient settings.json 变更后让未显式指定模型的会话跟随
// 新默认配置（client 重建 + 身份行重写走 SwitchClient）。指纹相同/文件
// 缺失/配置读失败均不动。只在回合边界调用（execute 开头），回合内并发
// 不会被换 client。
func (s *Server) refreshDefaultClient(sess *session, requestedModel string) {
	if requestedModel != "" {
		return // 显式模型路径由 trySwitchModel 即时读盘处理
	}
	fp := settingsFingerprint()
	if fp == 0 || fp == sess.clientFP {
		return
	}
	eff, err := config.Load()
	if err != nil {
		return
	}
	nc := llm.NewFromEffective(eff)
	sess.h.SwitchClient(nc)
	sess.clientFP = fp
}

// parseInput 解析 input（string 或 item 数组）：
//
//	text     新回合的用户文本（多条 user message 顺序拼接）
//	images   input_image 部件（仅支持 data URL）
//	resume   暂停应答（grf:input_result / function_call_output）
//	inject   回传历史 item（assistant / function_call / reasoning），由
//	         调用方并入会话历史（新回合输入之前）
func parseInput(req *createRequest) (text string, images []tools.Image, resume []agent.ResumeInput, inject []llm.Message, err error) {
	raw := strings.TrimSpace(string(req.Input))
	if raw == "" || raw == "null" {
		err = fmt.Errorf("input is required")
		return
	}
	if raw[0] == '"' { // 纯字符串输入
		var s string
		if e := json.Unmarshal(req.Input, &s); e != nil {
			err = fmt.Errorf("input: %w", e)
			return
		}
		text = s
		return
	}
	var items []inputItem
	if e := json.Unmarshal(req.Input, &items); e != nil {
		err = fmt.Errorf("input: %w", e)
		return
	}
	var texts []string
	for _, it := range items {
		switch it.Type {
		case inItemMessage:
			if it.Role == "user" {
				t, imgs, e := parseContentParts(it.Content)
				if e != nil {
					err = e
					return
				}
				if t != "" {
					texts = append(texts, t)
				}
				images = append(images, imgs...)
			} else if it.Role == "assistant" {
				t, _, e := parseContentParts(it.Content)
				if e != nil {
					err = e
					return
				}
				inject = append(inject, llm.Message{Role: "assistant", Content: t})
			}
			// system/developer 走 instructions 参数，input 里忽略
		case inItemFunctionCall:
			inject = append(inject, llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{
				ID: it.CallID, Type: "function", Function: llm.FunctionCall{Name: it.Name, Arguments: it.Arguments},
			}}})
		case inItemFCOutput:
			out, _, e := parseContentParts(it.Output)
			if e != nil {
				err = e
				return
			}
			resume = append(resume, agent.ResumeInput{CallID: it.CallID, Output: out})
		case inItemReasoning:
			var sb strings.Builder
			for _, p := range it.Summary {
				sb.WriteString(p.Text)
			}
			inject = append(inject, llm.Message{Role: "assistant", Reasoning: sb.String()})
		case inItemGrfInputResult:
			ri := agent.ResumeInput{
				CallID: it.CallID, Answers: it.Answers,
				Approved: it.Approved, Feedback: it.Feedback, Dismiss: it.Dismiss,
			}
			if it.Output != nil {
				out, _, e := parseContentParts(it.Output)
				if e != nil {
					err = e
					return
				}
				ri.Output = out
			}
			resume = append(resume, ri)
		default:
			// ORS 扩展规则：未知 item 类型优雅忽略（含其它厂商前缀）
		}
	}
	text = strings.Join(texts, "\n")
	return
}

// parseContentParts content 字段（string 或部件数组）→ 文本 + 图像。
func parseContentParts(raw json.RawMessage) (text string, images []tools.Image, err error) {
	if len(raw) == 0 {
		return
	}
	s := strings.TrimSpace(string(raw))
	if s[0] == '"' {
		var str string
		if e := json.Unmarshal(raw, &str); e != nil {
			return "", nil, e
		}
		return str, nil, nil
	}
	var parts []contentPart
	if e := json.Unmarshal(raw, &parts); e != nil {
		return "", nil, e
	}
	var texts []string
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "text":
			texts = append(texts, p.Text)
		case "input_image":
			img, e := imageDataURL(p.ImageURL)
			if e != nil {
				return "", nil, e
			}
			images = append(images, img)
		}
	}
	return strings.Join(texts, ""), images, nil
}

// imageDataURL data URL → Image（远程 URL 一期不支持）。截断用 rune 版
// term.ClipRunes（data URL 前缀为 ASCII，语义与按字节截断一致）。
func imageDataURL(u string) (tools.Image, error) {
	if !strings.HasPrefix(u, "data:") {
		return tools.Image{}, fmt.Errorf("input_image only supports data URLs (got %q)", term.ClipRunes(u, 64))
	}
	comma := strings.Index(u, ",")
	if comma < 0 {
		return tools.Image{}, fmt.Errorf("malformed data URL")
	}
	mime := strings.TrimSuffix(u[5:comma], ";base64")
	b64 := u[comma+1:]
	if _, err := base64.StdEncoding.DecodeString(b64); err != nil {
		return tools.Image{}, fmt.Errorf("decode image: %w", err)
	}
	return tools.Image{Mime: mime, B64: b64}, nil
}
