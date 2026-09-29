// history.go 历史管理：快照、会话回放（含 system 原文优先）、清空、
// 失败回滚、工具结果并入，以及暂停态判读（历史尾部未闭合的 tool_call）。
package agent

import (
	"fmt"

	"git.gorun.fun/share/grf-harness/llm"
	"git.gorun.fun/share/grf-harness/tools"
)

// History 历史快照（只读副本）。
func (a *Agent) History() []llm.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]llm.Message(nil), a.history...)
}

// RestoreFull 原样回放上一场会话的消息。systemText 非空时优先回放它
// （会话文件存的构建期提示词原文）——即使当前版本的提示词代码或运行时
// 事实已变化，请求前缀仍与上一场逐字一致，提示词缓存可命中；为空时
// 回退当前构建期提示词。
func (a *Agent) RestoreFull(msgs []llm.Message, systemText string) {
	if len(msgs) == 0 {
		return
	}
	if systemText == "" {
		systemText = a.system
	}
	h := make([]llm.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role != "system" {
			h = append(h, m)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.history = h // 回放列表已剔除全部 system 消息，头部由下面补
	a.setSystemLocked(systemText)
}

// Reset 清空历史与 todo，回到全新会话（保留 system 提示词与配置）。
func (a *Agent) Reset() {
	a.mu.Lock()
	a.history = []llm.Message{{Role: "system", Content: a.system}}
	a.mu.Unlock()
	if a.Todos != nil {
		a.Todos.Items = nil
		if a.Todos.OnChange != nil {
			a.Todos.OnChange(nil)
		}
	}
}

// setSystemLocked 更新系统提示词并同步 history 头部的 system 消息
// （调用方须持 a.mu；RestoreFull / SetSystemText / UpdateModelIdentity
// 三处共用）。前缀缓存不变量：a.system 与 history[0] 是同一份提示词的
// 两个视图——请求前缀按 history 序列化、a.system 供 Reset 重建与身份
// 行重写读取，两者不同步会让下次请求的前缀悄悄变化、提示词缓存静默
// 失效。history 头部不是 system 时（构建/回放路径均保证 system 打头，
// 理论不可达）补一条 system 头。
func (a *Agent) setSystemLocked(text string) {
	a.system = text
	if len(a.history) > 0 && a.history[0].Role == "system" {
		a.history[0].Content = text
		return
	}
	a.history = append([]llm.Message{{Role: "system", Content: text}}, a.history...)
}

// SetSystemText 替换静态系统提示词：构建期在工具注册完成后按实际可用
// 工具集组装 inner prompt 再回填（见 Harness.systemPromptFor）；运行期
// 由 Responses 请求级 instructions 生效。会改写请求前缀，提示词缓存将
// 失效（同步约束见 setSystemLocked）。
func (a *Agent) SetSystemText(text string) {
	if text == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.setSystemLocked(text)
}

// UpdateModelIdentity 切换模型后原位重写系统提示词中的模型身份行：身份
// 行在会话构建期按当时的 client 烘焙（新会话为全局 default 模型），切换
// 只换 client 不更新会让模型以旧身份自我认知（如实际调用 glm 却按提示词
// 自称旧模型）。只改模型名、其余逐字保留；模型行按改变频率排在提示词
// 最末（见 prompt.go 文件头），切换模型时改写只作废其后的尾部缓存。
// （history 头部同步见 setSystemLocked。）
func (a *Agent) UpdateModelIdentity(model string) {
	if model == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	next := RewriteModelIdentity(a.system, model)
	if next == a.system {
		return
	}
	a.setSystemLocked(next)
}

// sanitizeSnapshot 请求前清洗历史：剔除任何没有正文、没有多模态 parts
// 也没有工具调用的 assistant/user 消息（中断残留的半截消息序列化后
// content 整个省略，会被服务端以缺 messages.content 拒绝）。tool 消息
// 不在此列（协议要求保留，正文空由注册表兜底为 "(no output)"）。
func sanitizeSnapshot(h []llm.Message) []llm.Message {
	out := h[:0:0]
	for _, m := range h {
		if (m.Role == "assistant" || m.Role == "user") &&
			m.Content == "" && len(m.ContentParts) == 0 && len(m.ToolCalls) == 0 {
			continue
		}
		out = append(out, m)
	}
	return out
}

// canonicalizeToolResults 请求前把 tool 消息归位到发起它的 assistant
// (tool_calls) 紧后，并为缺闭合的调用补占位 tool 消息（只改请求快照，
// 不动存储历史）。严格校验的供应商（GLM 等）逐条检查 assistant.tool_calls
// 的紧后消息必须闭合全部 tool_call_id，两类历史损伤会触发其 400：
//  1. 旧版带图工具结果只落 "[tool result: …]" user 伪消息，tool_call 从未
//     被 tool 消息闭合（图像伪消息保留原位，模型仍能看到图）；
//  2. 暂停恢复等路径把闭合 tool 消息追加到队尾，与发起的 assistant 之间
//     隔了 user 消息（挪回紧后位置）。
//
// 归位时丢弃与发起 assistant 对不上的游离 tool 消息（异常历史残留，
// 留在原位同样会被供应商整单拒绝）。
func canonicalizeToolResults(h []llm.Message) []llm.Message {
	out := make([]llm.Message, 0, len(h))
	for i := 0; i < len(h); i++ {
		m := h[i]
		out = append(out, m)
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		// 扫到下一条 assistant 为止：该范围内的 tool 消息按 id 收拢
		// （同 id 重复闭合取首条），其余消息保持原序跟在闭合之后。
		results := map[string]llm.Message{}
		var rest []llm.Message
		j := i + 1
		for ; j < len(h) && h[j].Role != "assistant"; j++ {
			if h[j].Role == "tool" {
				if _, dup := results[h[j].ToolCallID]; !dup {
					results[h[j].ToolCallID] = h[j]
				}
				continue
			}
			rest = append(rest, h[j])
		}
		for _, c := range m.ToolCalls {
			if r, ok := results[c.ID]; ok {
				out = append(out, r)
			} else {
				// 未闭合调用（崩溃/中断残留）的合成占位：outcome 未知
				//（dsh TOOL_OUTCOME_UNKNOWN 语义）——该调用可能已执行、
				// 也可能从未运行，无从区分，引导模型只重试幂等操作。
				out = append(out, llm.Message{
					Role: "tool", Content: "(tool outcome unknown: the session was interrupted before this call's result was recorded — it may or may not have run. Retry only read-only or idempotent operations; when side effects are possible, verify the external state or ask the user before retrying)", ToolCallID: c.ID, Name: c.Name(),
				})
			}
		}
		out = append(out, rest...)
		i = j - 1
	}
	return out
}

// rollbackOrphanUser 本轮失败（LLM 出错/被取消）时移除尾部未被任何
// assistant 回复闭合的 user 输入，避免用户重发后出现连续两条重复 user
// 消息。只在本轮输入仍是最后一条消息时回滚（工具循环已推进则不动）。
func (a *Agent) rollbackOrphanUser(userMsg llm.Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := len(a.history)
	if n == 0 || a.history[n-1].Role != "user" {
		return
	}
	last := a.history[n-1]
	same := last.Content == userMsg.Content && len(last.ContentParts) == len(userMsg.ContentParts)
	if same {
		a.history = a.history[:n-1]
	}
}

// appendToolResult 把工具结果并入历史。纯文本 → tool 消息；带图像 →
// 两条消息：tool 文本消息先闭合 tool_call_id，再追加 user 多模态消息回传
// 图像。图像只能走 user 消息（OpenAI tool 消息不支持图像内容），但严格
// 校验的供应商（GLM 等）要求 assistant.tool_calls 紧后即有闭合每个
// tool_call_id 的 tool 消息——只落 user 伪消息会让会话此后每次请求都被
// 400 拒绝（"insufficient tool messages following tool_calls message"）。
func (a *Agent) appendToolResult(call llm.ToolCall, res tools.Result) {
	if len(res.Images) == 0 {
		a.mu.Lock()
		a.history = append(a.history, llm.Message{
			Role: "tool", Content: res.Text, ToolCallID: call.ID, Name: call.Name(),
		})
		a.mu.Unlock()
		return
	}
	text := res.Text
	if text == "" {
		text = "(image-only result; the image is attached in the next message)"
	}
	parts := []llm.Part{llm.TextPart(fmt.Sprintf("[tool result: %s] %s", call.Name(), res.Text))}
	for _, img := range res.Images {
		parts = append(parts, llm.ImagePart(img.Mime, img.B64))
	}
	a.mu.Lock()
	a.history = append(a.history,
		llm.Message{Role: "tool", Content: text, ToolCallID: call.ID, Name: call.Name()},
		llm.Message{Role: "user", ContentParts: parts})
	a.mu.Unlock()
}

// degradeImages 把历史中持久化的多模态图片 parts 降级为占位文本。模型
// 不支持 image_url 时请求会被 400 拒绝，而图片留在历史里会导致本会话
// 之后每次请求都带同一张图、反复失败——须清洗历史才能自愈。返回是否
// 发生了改写（调用方据此决定是否重试本轮请求）。
func (a *Agent) degradeImages() bool {
	const note = "[image omitted: this model endpoint rejected image input]"
	a.mu.Lock()
	defer a.mu.Unlock()
	changed := false
	for i, m := range a.history {
		if len(m.ContentParts) == 0 {
			continue
		}
		hasImg := false
		parts := make([]llm.Part, 0, len(m.ContentParts))
		for _, p := range m.ContentParts {
			if p.Type == "image_url" {
				hasImg = true
				continue
			}
			parts = append(parts, p)
		}
		if !hasImg {
			continue
		}
		parts = append(parts, llm.TextPart(note))
		if len(parts) == 1 && parts[0].Text == note {
			// 原消息只有图片没有文字（如 RunWithImages 的纯图输入）：
			// 收敛为纯文本，避免空 text part
			a.history[i].ContentParts = nil
			a.history[i].Content = note
		} else {
			a.history[i].ContentParts = parts
		}
		changed = true
	}
	return changed
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
