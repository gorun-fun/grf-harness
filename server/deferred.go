// deferred.go 链式交互宿主：ask_user 提问与逐工具审批不在本地阻塞等待，
// 而是让 agent 暂停回合（agent.PauseKind*），交互请求随 response 的
// incomplete_details 与 output items 返回，接入方用普通续聊请求
// （grf:input_result item）应答——协议形状 100% 是标准 previous_response_id
// 链。沙箱升级与计划审批不参与暂停，按 auto_approve 配置处理（fail-open
// 或 fail-closed，见 docs/server.md 安全模型）。
package server

import (
	"git.gorun.fun/share/grf-harness/tools"
)

// deferredHost gh server 的交互宿主（tools.DeferredHost 实现）。
type deferredHost struct {
	tools.NoopHost
}

// DeferInteractions 启用链式交互（agent 循环据此把交互点转为暂停）。
func (h *deferredHost) DeferInteractions() bool { return true }
