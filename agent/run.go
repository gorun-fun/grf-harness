// run.go Run* 入口族：CLI/TUI/app/weixin 用的不可暂停入口（Run/RunAs/
// RunWithImages）。实现全部委托 turn.go 的 runTurn；遇暂停点返回
// *PausedError——需要跨请求应答交互的接入方（gh server）改用
// RunTurn/RunTurnParts/Resume 驱动（见 pause.go）。
package agent

import (
	"context"

	"git.gorun.fun/share/grf-harness/llm"
	"git.gorun.fun/share/grf-harness/tools"
)

// Run 处理一条用户输入，执行完整工具调用循环，返回最终回复。
func (a *Agent) Run(ctx context.Context, userInput string) (string, error) {
	return a.RunAs(ctx, userInput, a.humanTurn)
}

// RunWithImages 处理一条带图像的用户输入：user 消息按多模态 parts
// （文本 + data URL 图像）发送。无图像时退化为普通 Run。
func (a *Agent) RunWithImages(ctx context.Context, userInput string, images []tools.Image) (string, error) {
	if len(images) == 0 {
		return a.RunAs(ctx, userInput, a.humanTurn)
	}
	parts := []llm.Part{llm.TextPart(userInput)}
	for _, img := range images {
		parts = append(parts, llm.ImagePart(img.Mime, img.B64))
	}
	return a.runMessage(ctx, llm.Message{Role: "user", ContentParts: parts}, a.humanTurn)
}

// RunAs 以指定身份（human / 系统）执行一轮。续跑与通知触发的回合用
// 非人类身份（goal 工具对非人类 turn 有限制）。
func (a *Agent) RunAs(ctx context.Context, userInput string, human bool) (string, error) {
	return a.runMessage(ctx, llm.Message{Role: "user", Content: userInput}, human)
}

// runMessage 单轮核心循环：并入给定 user 消息后执行工具调用循环直至
// 产生最终回复。遇暂停点返回 *PausedError。
func (a *Agent) runMessage(ctx context.Context, userMsg llm.Message, human bool) (string, error) {
	s, paused, err := a.runTurn(ctx, &userMsg, human, nil)
	if err != nil {
		return "", err
	}
	if paused != nil {
		return "", &PausedError{Turn: paused}
	}
	return s, nil
}
