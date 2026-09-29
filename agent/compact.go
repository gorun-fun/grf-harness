// compact.go 会话压缩（dsh compaction-basic + /compact 的本仓形态）：
// 历史估算超过阈值时（自动：新回合入口、并入本回合输入之前）或用户显式
// 要求时，用当前模型把既有会话总结成一条消息，历史收敛为 [system 原文,
// 总结消息]——system 头不动，goal/todo/jobs 等会话状态不受影响。压缩只
// 在回合边界发生：运行中与暂停态（历史尾部未闭合 tool_call）一律拒绝，
// 压缩永不制造孤儿 tool 消息、永不打断进行中的回合。自动路径失败开放：
// 压缩出错仅提示，回合照常进行。
package agent

import (
	"context"
	"fmt"
	"strings"

	"git.gorun.fun/share/grf-harness/llm"
)

// DefaultCompactThreshold 自动压缩的默认 token 阈值（估算口径）。现役
// 对话模型上下文下限普遍 128k，取 ~78% 留出单回合余量；更小窗口的端点
// 在到达本阈值前就会因超窗 400（既有行为，与压缩无关）。经
// Settings.CompactThreshold / SetCompactThreshold 覆盖，负值关闭。
const DefaultCompactThreshold = 100_000

// compactSummaryMarker 总结消息前缀：模型可据此识别这是压缩后的会话
// 载体而非用户原话（与 server/compact.go 的 ORS 压缩语义对齐：压缩产出
// 作为新链首、原链被替换）。
const compactSummaryMarker = "[conversation summary]"

// compactSystemPrompt 总结提示词（与 server/compact.go 的 ORS 端点同文；
// agent 不 import server，两处各自维护，改一处须同步另一处）。
const compactSystemPrompt = "You compact conversation transcripts. Produce a faithful, information-dense summary in the original language, preserving: task state, decisions made, file paths, commands run and their outcomes, open questions, and the next step. Output only the summary."

// SetCompactThreshold 设置自动压缩阈值（估算 token）：>0 显式阈值，0 用
// 默认（DefaultCompactThreshold），<0 关闭自动压缩。manual Compact 不受
// 该值影响。
func (a *Agent) SetCompactThreshold(n int) {
	a.mu.Lock()
	a.compactThreshold = n
	a.mu.Unlock()
}

// compactTokens 生效的自动压缩阈值（0 = 关闭）。
func (a *Agent) compactTokens() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.compactThreshold > 0:
		return a.compactThreshold
	case a.compactThreshold < 0:
		return 0
	default:
		return DefaultCompactThreshold
	}
}

// Compact 就地压缩会话历史：用当前模型总结全部既有消息（不含 system
// 头），历史替换为 [system 原文, 总结消息]，返回总结文本。运行中/暂停
// 态/空历史拒绝（见文件头：压缩只在回合边界发生）。
func (a *Agent) Compact(ctx context.Context) (string, error) {
	if a.IsRunning() {
		return "", fmt.Errorf("agent is running; compact at an idle boundary instead")
	}
	a.mu.Lock()
	if a.running { // 双检：IsRunning 与加锁之间进入的回合
		a.mu.Unlock()
		return "", fmt.Errorf("agent is running; compact at an idle boundary instead")
	}
	a.running = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.running = false
		a.mu.Unlock()
	}()
	return a.compactLocked(ctx)
}

// compactLocked 压缩本体（调用方已持 running 且不在回合中）。
func (a *Agent) compactLocked(ctx context.Context) (string, error) {
	if _, open := a.pausedCalls(); len(open) > 0 {
		return "", fmt.Errorf("paused tool calls present; resume or dismiss them before compacting")
	}
	transcript := a.transcript()
	if transcript == "" {
		return "", fmt.Errorf("nothing to compact")
	}
	msg, err := a.getClient().Chat(ctx, []llm.Message{
		{Role: "system", Content: compactSystemPrompt},
		{Role: "user", Content: transcript},
	}, nil)
	if err != nil {
		return "", fmt.Errorf("compact: %w", err)
	}
	// thinking 模型偶发把正文放 reasoning_content；transcript 的
	// "role: " 行风格会诱导个别模型用 <summary> 标签包裹输出——两处
	// 都规范化，否则压缩产出空壳摘要
	content := msg.Content
	if content == "" {
		content = msg.Reasoning
	}
	summary := normalizeSummary(content)
	if summary == "" {
		return "", fmt.Errorf("compact: empty summary")
	}
	a.mu.Lock()
	sys := a.system
	if len(a.history) > 0 && a.history[0].Role == "system" {
		sys = a.history[0].Content // 回放历史的构建期原文优先（缓存前缀不变式）
	}
	a.history = []llm.Message{
		{Role: "system", Content: sys},
		{Role: "user", Content: compactSummaryMarker + " " + summary},
	}
	a.mu.Unlock()
	a.emit("compact", firstLine(summary))
	return summary, nil
}

// maybeAutoCompact 新回合入口的自动压缩（runTurn 专用：已持 running、
// 本回合输入尚未并入——总结只覆盖既有会话）。失败开放：仅事件提示，
// 不阻塞本回合。
func (a *Agent) maybeAutoCompact(ctx context.Context) {
	th := a.compactTokens()
	if th <= 0 {
		return
	}
	a.mu.Lock()
	est := llm.EstimateMessagesTokens(a.history)
	a.mu.Unlock()
	if est < int64(th) {
		return
	}
	if _, err := a.compactLocked(ctx); err != nil {
		a.emit("notice", "auto-compact failed (continuing uncompacted): "+err.Error())
	}
}

// normalizeSummary 清洗总结输出：剥掉模型偶发包裹的 <summary> 标签与
// "summary:"/"总结:" 类前缀（transcript 的行风格会诱导部分模型这么包）。
func normalizeSummary(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "<summary>") {
		s = strings.TrimSpace(strings.TrimPrefix(s, "<summary>"))
		if i := strings.LastIndex(s, "</summary>"); i >= 0 {
			s = strings.TrimSpace(s[:i])
		}
	}
	for _, p := range []string{"summary:", "Summary:", "SUMMARY:", "总结：", "总结:", "摘要：", "摘要:"} {
		if strings.HasPrefix(s, p) {
			s = strings.TrimSpace(strings.TrimPrefix(s, p))
			break
		}
	}
	return s
}

// transcript 会话历史 → 压缩用 transcript：跳过 system，逐条
// "role: 内容" 拼接（Message.String 含工具调用摘要；与 server 侧
// buildTranscript 同构）。
func (a *Agent) transcript() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var sb strings.Builder
	for _, m := range a.history {
		if m.Role == "system" {
			continue
		}
		sb.WriteString(m.Role + ": " + m.String() + "\n")
	}
	return sb.String()
}
