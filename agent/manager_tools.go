// manager_tools.go 子代理树工具注册（自 manager.go 拆出，纯移动）：
// subagent / subagent_fork / send_message / interrupt_agent / list_agents
// 五个工具的参数解析与结果文案。子代理的启动/投递/打断逻辑本体仍在
// manager.go 的 Manager 上。
package agent

import (
	"context"
	"fmt"
	"strings"

	"git.gorun.fun/share/grf-harness/tools"
)

// RegisterAgentTools 注册 subagent / subagent_fork / send_message /
// interrupt_agent / list_agents 五个工具。
func RegisterAgentTools(r *tools.Registry, a *Agent, m *Manager) {
	sub := func(fork bool) tools.Handler {
		return func(ctx context.Context, args string) (tools.Result, error) {
			var p struct {
				Description     string `json:"description"`
				Prompt          string `json:"prompt"`
				RunInBackground *bool  `json:"run_in_background"`
			}
			if err := tools.DecodeJSON(args, &p); err != nil {
				return tools.Result{}, err
			}
			if strings.TrimSpace(p.Prompt) == "" {
				return tools.Result{}, fmt.Errorf("prompt is required (complete and self-contained — the subagent does not see this conversation)")
			}
			if strings.TrimSpace(p.Description) == "" {
				return tools.Result{}, fmt.Errorf("description is required (3-5 words, for display)")
			}
			bg := true
			if p.RunInBackground != nil {
				bg = *p.RunInBackground
			}
			var (
				id, out string
				err     error
			)
			if fork {
				id, out, err = m.Fork(a, oneLine(p.Description, 60), p.Prompt, bg)
			} else {
				id, out, err = m.Subagent(a, oneLine(p.Description, 60), p.Prompt, bg)
			}
			if err != nil {
				return tools.Result{}, err
			}
			if bg {
				return tools.TextResult(fmt.Sprintf("subagent started in the background: id=%s label=%q\nYou will be notified when it settles; recall continuable subagents with list_agents and give follow-up work via send_message.", id, p.Description)), nil
			}
			return tools.TextResult(fmt.Sprintf("subagent %s (%s) result:\n\n%s", id, p.Description, out)), nil
		}
	}
	r.Register("subagent", sub(false))
	r.Register("subagent_fork", sub(true))

	r.Register("send_message", func(ctx context.Context, args string) (tools.Result, error) {
		var p struct {
			SubagentID string `json:"subagent_id"`
			Message    string `json:"message"`
		}
		if err := tools.DecodeJSON(args, &p); err != nil {
			return tools.Result{}, err
		}
		if strings.TrimSpace(p.Message) == "" {
			return tools.Result{}, fmt.Errorf("message is required")
		}
		if err := m.SendMessage(a, p.SubagentID, p.Message); err != nil {
			return tools.Result{}, fmt.Errorf("message NOT delivered: %v", err)
		}
		return tools.TextResult(fmt.Sprintf("message delivered to %s; it becomes the subagent's next turn (queued if it is still working). No answer comes back through this call — the settle notice will carry its outcome.", p.SubagentID)), nil
	})

	r.Register("interrupt_agent", func(ctx context.Context, args string) (tools.Result, error) {
		var p struct {
			AgentID string `json:"agent_id"`
		}
		if err := tools.DecodeJSON(args, &p); err != nil {
			return tools.Result{}, err
		}
		if err := m.Interrupt(a, p.AgentID); err != nil {
			return tools.Result{}, err
		}
		return tools.TextResult(fmt.Sprintf("stop request accepted for %s; only its current turn stops — queued messages stay parked and the agent stays available", p.AgentID)), nil
	})

	r.Register("list_agents", func(ctx context.Context, args string) (tools.Result, error) {
		var p struct {
			Scope string `json:"scope"`
		}
		if err := tools.DecodeJSON(args, &p); err != nil {
			return tools.Result{}, err
		}
		if p.Scope == "" {
			p.Scope = "children"
		}
		if p.Scope != "children" && p.Scope != "descendants" {
			return tools.Result{}, fmt.Errorf("scope must be children or descendants")
		}
		return tools.TextResult(m.ListAgents(a, p.Scope)), nil
	})
}
