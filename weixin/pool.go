// pool.go 每用户 agent 会话池：每个微信用户一个独立 agent（独立历史与
// 工具工作区上下文），首次触达时组装 harness；ask_user 经 weixinHost
// 转发到微信对话。
package weixin

import (
	"fmt"
	"sync"

	"git.gorun.fun/share/grf-harness/agent"
	"git.gorun.fun/share/grf-harness/llm"
	"git.gorun.fun/share/grf-harness/tools"
)

// agentPool 每个微信用户一个独立 agent 会话(独立历史与工具工作区上下文)。
// 消息经 userMux 分发到各用户的 goroutine 并发处理,pool 需加锁。
type agentPool struct {
	mu          sync.Mutex
	opts        agent.Options
	agents      map[string]*agent.Agent
	hosts       map[string]*weixinHost
	mux         *userMux // messageLoop 创建后回填(get 时必已就位)
	autoApprove bool
}

func newAgentPool(client *llm.Client, systemPrompt, workspace string, autoApprove bool) *agentPool {
	return &agentPool{
		opts: agent.Options{
			Workspace:    workspace,
			Client:       client,
			SystemPrompt: systemPrompt,
		},
		agents:      map[string]*agent.Agent{},
		hosts:       map[string]*weixinHost{},
		autoApprove: autoApprove,
	}
}

// drop 丢弃该用户的 agent 会话(下次消息重建,历史清零)。用于 /new 指令
// 与失败自愈:被污染的历史(如模型不支持的历史图片)会导致后续每轮都失败。
func (p *agentPool) drop(fromID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.agents, fromID)
}

// get 取该用户的 agent 与宿主;首次触达时组装 harness(ask_user 经
// weixinHost 转发到微信)并打印工具事件。
func (p *agentPool) get(fromID string) (*agent.Agent, *weixinHost, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a, ok := p.agents[fromID]; ok {
		return a, p.hosts[fromID], nil
	}
	host := &weixinHost{
		NoopHost: tools.NoopHost{AutoApprove: p.autoApprove},
		bot:      p.mux.bot,
		mux:      p.mux,
		pool:     p,
		fromID:   fromID,
	}
	opts := p.opts
	opts.Host = host
	h, err := agent.Build(opts)
	if err != nil {
		return nil, nil, err
	}
	root := h.Root
	root.SetOnEvent(func(kind, detail string) {
		switch kind {
		case "tool":
			fmt.Printf("   🔧 %s\n", detail)
		case "result":
			fmt.Printf("   ↳ %s\n", detail)
		case "done":
			fmt.Printf("   ✔ 一轮完成\n")
		}
	})
	p.agents[fromID] = root
	p.hosts[fromID] = host
	return root, host, nil
}
