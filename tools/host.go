// host.go 工具与宿主环境（UI / 嵌入应用）之间的回调接口。TUI、单任务
// 命令行与作为库嵌入的三种形态各自提供实现。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

// Question ask_user_question 的单个问题。
type Question struct {
	ID          string   `json:"id"`
	Question    string   `json:"question"`
	Header      string   `json:"header,omitempty"`
	Options     []Option `json:"options,omitempty"`
	MultiSelect bool     `json:"multi_select,omitempty"`
}

// Option 单个选项。
type Option struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// ApproveDecision 审批模式下逐工具确认的结果。
type ApproveDecision struct {
	Approved bool   // 是否放行
	Feedback string // 未批准时的用户反馈（可空）；非空时随拒绝原因作为工具结果回给模型
}

// Host 宿主回调：交互能力（提问/审批）与事件通知。
type Host interface {
	// AskUser 展示问题并等待用户作答。返回值须为每个 id 对应的答案
	// （字符串，或多选时的 []string）；用户取消返回 error。
	AskUser(ctx context.Context, qs []Question) (map[string]any, error)
	// ApproveEscalation 沙箱升级审批：同一命令被沙箱拒绝后重试时征求
	// 用户同意。返回是否放行。
	ApproveEscalation(ctx context.Context, tool, command, mode, justification string) bool
	// ApprovePlan 计划审批：返回 (是否批准, 用户反馈文本)。反馈仅在未
	// 批准时使用。
	ApprovePlan(ctx context.Context, plan string) (bool, string)
	// ApproveTool 审批模式下的逐工具确认：执行前征求用户同意。args 为
	// 完整参数 JSON（不截断，展示裁剪由宿主自理）；未批准时 Feedback
	// 可携带用户反馈（如"别动这个文件，改用 …"），模型可据此调整方案。
	ApproveTool(ctx context.Context, name, args string) ApproveDecision
	// NotifyEvent 过程事件通知（UI 展示）：kind 见各工具实现。
	NotifyEvent(kind, detail string)
}

// NoopHost 非交互宿主：提问失败、审批按 autoApprove、事件丢弃。
type NoopHost struct {
	AutoApprove bool
	OnEvent     func(kind, detail string)
}

func (h *NoopHost) AskUser(ctx context.Context, qs []Question) (map[string]any, error) {
	return nil, fmt.Errorf("no interactive user is available in this session; proceed without asking, state assumptions in the final answer")
}

func (h *NoopHost) ApproveEscalation(ctx context.Context, tool, command, mode, justification string) bool {
	return h.AutoApprove
}

func (h *NoopHost) ApprovePlan(ctx context.Context, plan string) (bool, string) {
	if h.AutoApprove {
		return true, ""
	}
	return false, "plan approval is unavailable without an interactive user; continue planning or finish with a summary"
}

func (h *NoopHost) ApproveTool(ctx context.Context, name, args string) ApproveDecision {
	// 无交互用户（子代理/嵌入）：审批模式只约束有交互宿主的根会话
	return ApproveDecision{Approved: true}
}

func (h *NoopHost) NotifyEvent(kind, detail string) {
	if h.OnEvent != nil {
		h.OnEvent(kind, detail)
	}
}

// MarshalAnswers 把答案 map 序列化为工具结果文本。
func MarshalAnswers(answers map[string]any) (string, error) {
	data, err := json.Marshal(answers)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// DeferredHost 可选宿主能力：交互（ask_user 提问与逐工具审批）不在本地
// 阻塞等待，而是让 agent 暂停回合、把交互请求随 PausedTurn 返回，由接
// 入方经链式请求应答（gh server 的 Responses 接入用此模式）。实现本接
// 口且 DeferInteractions 返回 true 时：ask_user_question 调用与审批模式
// 下的工具调用都转为暂停；沙箱升级与计划审批不参与暂停，按宿主自身语
// 义（如 autoApprove）处理。
type DeferredHost interface {
	Host
	// DeferInteractions 是否启用链式交互（agent 据此决定暂停而非本地交互）。
	DeferInteractions() bool
}
