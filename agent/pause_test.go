// pause_test.go 回合暂停/恢复协议测试：client 函数工具、deferred 宿主的
// 提问与审批暂停、跨"重启"（Restore 未闭合历史）恢复、Run* 遇暂停报错。
package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"git.gorun.fun/share/grf-harness/llm"
	"git.gorun.fun/share/grf-harness/tools"
)

// deferHost 启用链式交互的宿主（gh server 的 deferred host 形状）。
type deferHost struct {
	tools.NoopHost
}

func (h *deferHost) DeferInteractions() bool { return true }

func TestPauseClientFunction(t *testing.T) {
	f := newFakeLLM([]string{
		`TOOL:get_weather:{"city":"北京"}`,
		`北京今天晴，26 度`,
	})
	defer f.Close()
	h := buildTestHarness(t, f, t.TempDir())
	defer h.Close()
	h.Root.Reg().RegisterClient(llm.Tool{
		Name:        "get_weather",
		Description: "查天气",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
	})

	reply, paused, err := h.Root.RunTurn(context.Background(), "北京天气", true)
	if err != nil || reply != "" {
		t.Fatalf("期望暂停，得到 reply=%q err=%v", reply, err)
	}
	if paused == nil || len(paused.Calls) != 1 {
		t.Fatalf("期望 1 个暂停调用，得到 %+v", paused)
	}
	pc := paused.Calls[0]
	if pc.Kind != PauseKindFunction || pc.Name != "get_weather" || pc.CallID != "c1" || pc.Arguments != `{"city":"北京"}` {
		t.Fatalf("暂停调用字段不符: %+v", pc)
	}
	if !h.Root.HasPausedCalls() {
		t.Fatal("HasPausedCalls 应为 true")
	}

	reply, paused, err = h.Root.Resume(context.Background(), []ResumeInput{{CallID: "c1", Output: `{"temp":26,"cond":"sunny"}`}})
	if err != nil || paused != nil {
		t.Fatalf("Resume 失败: reply=%q paused=%v err=%v", reply, paused, err)
	}
	if reply != "北京今天晴，26 度" {
		t.Fatalf("最终回复不符: %q", reply)
	}
	if h.Root.HasPausedCalls() {
		t.Fatal("恢复完成后不应再有暂停调用")
	}
	// 客户端工具结果应作为 tool 消息并入历史
	found := false
	for _, m := range h.Root.History() {
		if m.Role == "tool" && m.ToolCallID == "c1" && strings.Contains(m.Content, "sunny") {
			found = true
		}
	}
	if !found {
		t.Fatal("历史中缺少注入的客户端工具结果")
	}
}

func TestPauseClientFunctionDefinitionSent(t *testing.T) {
	f := newFakeLLM([]string{`直接回答`})
	defer f.Close()
	h := buildTestHarness(t, f, t.TempDir())
	defer h.Close()
	h.Root.Reg().RegisterClient(llm.Tool{Name: "my_fn", Description: "d", Parameters: map[string]any{"type": "object"}})
	if _, _, err := h.Root.RunTurn(context.Background(), "hi", true); err != nil {
		t.Fatal(err)
	}
	sent := false
	for _, d := range f.gotDefs {
		if d.Name == "my_fn" {
			sent = true
		}
	}
	if !sent {
		t.Fatal("client 工具定义未随请求发送")
	}
}

func TestPauseAskUserQuestion(t *testing.T) {
	f := newFakeLLM([]string{
		`TOOL:ask_user_question:{"questions":[{"id":"q1","question":"口味？","options":[{"label":"甜"},{"label":"咸"}]}]}`,
		`选了甜，好的`,
	})
	defer f.Close()
	h := buildHarnessWithHost(t, f, t.TempDir(), &deferHost{})
	defer h.Close()

	_, paused, err := h.Root.RunTurn(context.Background(), "推荐早餐", true)
	if err != nil || paused == nil || len(paused.Calls) != 1 {
		t.Fatalf("期望提问暂停: err=%v paused=%+v", err, paused)
	}
	if paused.Calls[0].Kind != PauseKindQuestion {
		t.Fatalf("暂停类别应为 question: %+v", paused.Calls[0])
	}
	// 交互请求原文在 Arguments 里（server 层据此解析 questions）
	if !strings.Contains(paused.Calls[0].Arguments, `"id":"q1"`) {
		t.Fatalf("Arguments 应携带问题原文: %s", paused.Calls[0].Arguments)
	}

	reply, paused, err := h.Root.Resume(context.Background(), []ResumeInput{{CallID: "c1", Answers: map[string]any{"q1": "甜"}}})
	if err != nil || paused != nil {
		t.Fatalf("Resume 失败: %v / %+v", err, paused)
	}
	if reply != "选了甜，好的" {
		t.Fatalf("最终回复不符: %q", reply)
	}
	// 答案应按 MarshalAnswers 格式并入历史
	found := false
	for _, m := range h.Root.History() {
		if m.Role == "tool" && strings.Contains(m.Content, `"q1":"甜"`) {
			found = true
		}
	}
	if !found {
		t.Fatal("历史中缺少序列化的答案结果")
	}
}

func TestPauseAskUserDismiss(t *testing.T) {
	f := newFakeLLM([]string{
		`TOOL:ask_user_question:{"questions":[{"id":"q1","question":"口味？"}]}`,
		`好的，我自行假设`,
	})
	defer f.Close()
	h := buildHarnessWithHost(t, f, t.TempDir(), &deferHost{})
	defer h.Close()
	if _, paused, _ := h.Root.RunTurn(context.Background(), "推荐", true); paused == nil {
		t.Fatal("期望暂停")
	}
	reply, _, err := h.Root.Resume(context.Background(), []ResumeInput{{CallID: "c1", Dismiss: true}})
	if err != nil {
		t.Fatal(err)
	}
	if reply != "好的，我自行假设" {
		t.Fatalf("回复不符: %q", reply)
	}
	for _, m := range h.Root.History() {
		if m.Role == "tool" && m.ToolCallID == "c1" && !strings.Contains(m.Content, "dismissed") {
			t.Fatalf("Dismiss 应产生取消语义的工具结果: %q", m.Content)
		}
	}
}

func TestPauseApprovalApprovedExecutes(t *testing.T) {
	f := newFakeLLM([]string{
		`TOOL:bash:{"command":"echo paused-ok"}`,
		`命令已执行`,
	})
	defer f.Close()
	ws := t.TempDir()
	h := buildHarnessWithHost(t, f, ws, &deferHost{})
	defer h.Close()
	h.Root.SetApproveMode(true)

	_, paused, err := h.Root.RunTurn(context.Background(), "跑命令", true)
	if err != nil || paused == nil {
		t.Fatalf("期望审批暂停: %v / %+v", err, paused)
	}
	if paused.Calls[0].Kind != PauseKindApproval || paused.Calls[0].Name != "bash" {
		t.Fatalf("暂停类别不符: %+v", paused.Calls[0])
	}
	reply, _, err := h.Root.Resume(context.Background(), []ResumeInput{{CallID: "c1", Approved: true}})
	if err != nil {
		t.Fatal(err)
	}
	if reply != "命令已执行" {
		t.Fatalf("回复不符: %q", reply)
	}
	// 批准的工具应已真实执行（bash 输出并入历史）
	executed := false
	for _, m := range h.Root.History() {
		if m.Role == "tool" && strings.Contains(m.Content, "paused-ok") {
			executed = true
		}
	}
	if !executed {
		t.Fatal("批准后工具未执行")
	}
}

func TestPauseApprovalDeniedInjectsFeedback(t *testing.T) {
	f := newFakeLLM([]string{
		`TOOL:bash:{"command":"rm -rf x"}`,
		`已按反馈调整`,
	})
	defer f.Close()
	h := buildHarnessWithHost(t, f, t.TempDir(), &deferHost{})
	defer h.Close()
	h.Root.SetApproveMode(true)
	if _, paused, _ := h.Root.RunTurn(context.Background(), "跑", true); paused == nil {
		t.Fatal("期望暂停")
	}
	reply, _, err := h.Root.Resume(context.Background(), []ResumeInput{{CallID: "c1", Feedback: "别删文件"}})
	if err != nil {
		t.Fatal(err)
	}
	if reply != "已按反馈调整" {
		t.Fatalf("回复不符: %q", reply)
	}
	for _, m := range h.Root.History() {
		if m.Role == "tool" && m.ToolCallID == "c1" {
			if !strings.Contains(m.Content, "别删文件") {
				t.Fatalf("拒绝文本应携带反馈: %q", m.Content)
			}
			return
		}
	}
	t.Fatal("缺少拒绝结果")
}

func TestRunSeriesReturnsPausedError(t *testing.T) {
	f := newFakeLLM([]string{`TOOL:get_weather:{"city":"x"}`})
	defer f.Close()
	h := buildTestHarness(t, f, t.TempDir())
	defer h.Close()
	h.Root.Reg().RegisterClient(llm.Tool{Name: "get_weather", Parameters: map[string]any{"type": "object"}})
	_, err := h.Root.Run(context.Background(), "天气")
	var pe *PausedError
	if !errors.As(err, &pe) || pe.Turn == nil || len(pe.Turn.Calls) != 1 {
		t.Fatalf("Run 应返回 PausedError，得到 %v", err)
	}
}

func TestResumeAcrossRestart(t *testing.T) {
	// 暂停状态全部在历史里：Restore 到新 harness 后仍可恢复续跑。
	f1 := newFakeLLM([]string{`TOOL:get_weather:{"city":"重启"}`})
	ws := t.TempDir()
	h1 := buildTestHarness(t, f1, ws)
	h1.Root.Reg().RegisterClient(llm.Tool{Name: "get_weather", Parameters: map[string]any{"type": "object"}})
	if _, paused, _ := h1.Root.RunTurn(context.Background(), "查天气", true); paused == nil {
		t.Fatal("期望暂停")
	}
	msgs := h1.Root.History()
	sys := h1.Root.SystemText()
	h1.Close()

	f2 := newFakeLLM([]string{`重启后恢复成功`})
	defer f2.Close()
	h2 := buildTestHarness(t, f2, ws)
	defer h2.Close()
	h2.Root.Reg().RegisterClient(llm.Tool{Name: "get_weather", Parameters: map[string]any{"type": "object"}})
	h2.Root.RestoreFull(msgs, sys)
	if !h2.Root.HasPausedCalls() {
		t.Fatal("Restore 后应识别出未闭合调用")
	}
	reply, paused, err := h2.Root.Resume(context.Background(), []ResumeInput{{CallID: "c1", Output: "sunny"}})
	if err != nil || paused != nil {
		t.Fatalf("跨重启恢复失败: %v / %+v", err, paused)
	}
	if reply != "重启后恢复成功" {
		t.Fatalf("回复不符: %q", reply)
	}
}

func TestResumeMissingInputRejected(t *testing.T) {
	f := newFakeLLM([]string{`TOOL:get_weather:{"city":"x"}`})
	defer f.Close()
	h := buildTestHarness(t, f, t.TempDir())
	defer h.Close()
	h.Root.Reg().RegisterClient(llm.Tool{Name: "get_weather", Parameters: map[string]any{"type": "object"}})
	if _, paused, _ := h.Root.RunTurn(context.Background(), "查", true); paused == nil {
		t.Fatal("期望暂停")
	}
	if _, _, err := h.Root.Resume(context.Background(), nil); err == nil {
		t.Fatal("inputs 未覆盖未闭合调用应报错")
	}
}

func TestStepsCarryAcrossPause(t *testing.T) {
	// MaxSteps 跨暂停保持：暂停已消耗 1 步，把上限设为 1，恢复后应立即
	// 报 max steps（证明步数没有重置为 0 后再跑一轮）。
	f := newFakeLLM([]string{
		`TOOL:get_weather:{"city":"x"}`,
		`TOOL:get_weather:{"city":"y"}`,
	})
	defer f.Close()
	h := buildTestHarness(t, f, t.TempDir())
	defer h.Close()
	h.Root.Reg().RegisterClient(llm.Tool{Name: "get_weather", Parameters: map[string]any{"type": "object"}})
	h.Root.SetMaxSteps(1)
	if _, paused, _ := h.Root.RunTurn(context.Background(), "查", true); paused == nil || paused.Steps != 1 {
		t.Fatalf("期望 steps=1 的暂停: %+v", paused)
	}
	_, _, err := h.Root.Resume(context.Background(), []ResumeInput{{CallID: "c1", Output: "o"}})
	if err == nil || !strings.Contains(err.Error(), "max tool steps") {
		t.Fatalf("恢复后应受剩余步数约束，得到 %v", err)
	}
}
