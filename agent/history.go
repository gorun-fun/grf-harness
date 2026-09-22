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

// Restore 原样回放上一场会话的消息（system 用当前构建期提示词）。
func (a *Agent) Restore(msgs []llm.Message) { a.RestoreFull(msgs, "") }

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
	a.system = systemText
	a.history = append([]llm.Message{{Role: "system", Content: systemText}}, h...)
	a.mu.Unlock()
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

// UpdateModelIdentity 切换模型后原位重写系统提示词中的模型身份行：身份
// 行在会话构建期按当时的 client 烘焙（新会话为全局 default 模型），切换
// 只换 client 不更新会让模型以旧身份自我认知（如实际调用 glm 却按提示词
// 自称旧模型）。只改模型名、其余逐字保留；模型行按改变频率排在提示词
// 最末（见 prompt.go 文件头），切换模型时改写只作废其后的尾部缓存。
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
	a.system = next
	if len(a.history) > 0 && a.history[0].Role == "system" {
		a.history[0].Content = next
	}
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

// appendToolResult 把工具结果并入历史：纯文本 → tool 消息；带图像 →
// user 消息（多模态 parts，OpenAI tool 消息不支持图像内容）。
func (a *Agent) appendToolResult(call llm.ToolCall, res tools.Result) {
	if len(res.Images) == 0 {
		a.mu.Lock()
		a.history = append(a.history, llm.Message{
			Role: "tool", Content: res.Text, ToolCallID: call.ID, Name: call.Name(),
		})
		a.mu.Unlock()
		return
	}
	parts := []llm.Part{llm.TextPart(fmt.Sprintf("[tool result: %s] %s", call.Name(), res.Text))}
	for _, img := range res.Images {
		parts = append(parts, llm.ImagePart(img.Mime, img.B64))
	}
	a.mu.Lock()
	a.history = append(a.history, llm.Message{Role: "user", ContentParts: parts})
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
