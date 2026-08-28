// notice.go 异步通知：后台子代理落定等事件注入队列，空闲时触发
// OnIdleNotice 自动续跑；通知只在 LLM 请求前 drain（tool 结果必须紧跟
// assistant 消息，OpenAI 协议要求）。
package agent

import "time"

// EnqueueNotice 注入一条系统通知；空闲时触发 OnIdleNotice 自动续跑。
func (a *Agent) EnqueueNotice(text string) {
	a.mu.Lock()
	a.notices = append(a.notices, text)
	idle := !a.running
	a.mu.Unlock()
	if idle && a.OnIdleNotice != nil {
		go a.OnIdleNotice(text)
	}
}

// drainNotices 取走积压通知。
func (a *Agent) drainNotices() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.notices) == 0 {
		return nil
	}
	out := a.notices
	a.notices = nil
	return out
}

// hasBusyBackground 是否有仍在运行的后台作业或子代理。
func (a *Agent) hasBusyBackground() bool {
	if a.Jobs != nil && a.Jobs.ActiveCount() > 0 {
		return true
	}
	if a.mgr != nil {
		return a.mgr.hasRunningDescendants(a)
	}
	return false
}

// RunAndWaitBackground 等待全部后台子代理落定（最多 wait 上限）并返回
// 期间产生的通知（任务模式收尾用）。
func (a *Agent) RunAndWaitBackground(wait time.Duration) []string {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if !a.hasBusyBackground() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return a.drainNotices()
}
