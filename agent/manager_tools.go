// manager_tools.go 子代理树工具注册（自 manager.go 拆出，纯移动）：
// subagent / subagent_fork / send_message / interrupt_agent / list_agents
// 五个工具的参数解析与结果文案。子代理的启动/投递/打断逻辑本体仍在
// manager.go 的 Manager 上。router 非-nil 时（指挥官根会话）：subagent
// 派发先经集群路由层分流——description 命中远端节点（@name 或能力标签）
// 即派往该节点（完整 agent 会话，经既有 ORS 端点），send_message /
// interrupt_agent / list_agents 对远端句柄同样有效；未命中走本地 Manager。
package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"git.gorun.fun/share/grf-harness/cluster"
	"git.gorun.fun/share/grf-harness/tools"
)

// RegisterAgentTools 注册 subagent / subagent_fork / send_message /
// interrupt_agent / list_agents 五个工具。router 为集群路由层（可 nil：
// 集群未启用或非根会话）。
func RegisterAgentTools(r *tools.Registry, a *Agent, m *Manager, router *cluster.Router) {
	sub := func(fork bool) tools.Handler {
		return func(ctx context.Context, args string) (tools.Result, error) {
			var p struct {
				Description     string   `json:"description"`
				Prompt          string   `json:"prompt"`
				RunInBackground *bool    `json:"run_in_background"`
				SeedFiles       []string `json:"seed_files"`
				ReturnFiles     []string `json:"return_files"`
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
			// 集群分流（仅 subagent；fork 的历史种子是本地概念，不跨节点）
			if router != nil && !fork && router.ShouldRoute(p.Description) {
				return dispatchRemote(ctx, router, a, p.Description, p.Prompt, bg, p.SeedFiles, p.ReturnFiles)
			}
			// 文件传输参数只对集群路由派发有意义；本地子代理同机可见文件
			if len(p.SeedFiles) > 0 || len(p.ReturnFiles) > 0 {
				return tools.Result{}, fmt.Errorf("seed_files/return_files only apply to cluster-routed subagents (mention a cluster node like @name in the description); local subagents share this machine's filesystem already")
			}
			var (
				id, out string
				err     error
			)
			if fork {
				id, out, err = m.Fork(ctx, a, oneLine(p.Description, 60), p.Prompt, bg)
			} else {
				id, out, err = m.Subagent(ctx, a, oneLine(p.Description, 60), p.Prompt, bg)
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
		if router != nil && router.Owns(p.SubagentID) {
			if err := router.SendMessage(p.SubagentID, p.Message); err != nil {
				return tools.Result{}, fmt.Errorf("message NOT delivered: %v", err)
			}
			return tools.TextResult(fmt.Sprintf("message delivered to remote subagent %s; it becomes the subagent's next turn (queued if it is still working). No answer comes back through this call — the settle notice will carry its outcome.", p.SubagentID)), nil
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
		if router != nil && router.Owns(p.AgentID) {
			if err := router.Interrupt(p.AgentID); err != nil {
				return tools.Result{}, err
			}
			return tools.TextResult(fmt.Sprintf("stop request sent to remote node for %s; only its current turn stops — queued messages stay parked and the agent stays available", p.AgentID)), nil
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
		out := m.ListAgents(a, p.Scope)
		if router != nil {
			if extra := router.ListText(); extra != "" {
				out += "\nremote cluster subagents:\n" + extra
			}
		}
		return tools.TextResult(out), nil
	})
}

// dispatchRemote 集群派发：任务经路由层发往远端节点（完整 agent 会话，
// 节点本地工作区与工具），落定通知与本地子代理同格式回流（含节点标注）；
// 前台同步等终态。审批/提问经暂停协议直达本地 Host，模型无感知。
// M2 文件传输：seedFiles 打包下发（Fs 读边界）、returnFiles 声明回传，
// 产物落定时从本地共享区解包进工作区 cluster-artifacts/<句柄>/。
func dispatchRemote(ctx context.Context, router *cluster.Router, a *Agent, description, prompt string, bg bool, seedFiles, returnFiles []string) (tools.Result, error) {
	id, out, err := router.Dispatch(ctx, cluster.DispatchReq{
		Description: description,
		Prompt:      prompt,
		Background:  bg,
		SeedFiles:   seedFiles,
		ReturnFiles: returnFiles,
		Fs:          a.Fs,
		Host:        func() tools.Host { return a.host }, // 懒取：SetHost 后生效
		Notify: func(id, label, outcome, final string) {
			// 与 Manager.settle 同格式（远端标注），模型侧体验一致
			if final == "" {
				final = "(no final message)"
			}
			a.EnqueueNotice(fmt.Sprintf("your background subagent settled — id: %s, label: %s, outcome: %s\nfinal assistant message:\n%s\nUse send_message(subagent_id=%s, ...) to give it more work on the same conversation.", id, label, outcome, final, id))
		},
		Artifacts: func(id string, arts []cluster.ArtifactMeta) {
			a.EnqueueNotice(unpackArtifacts(router, a.Fs, id, arts))
		},
	})
	if err != nil {
		if errors.Is(err, cluster.ErrNoRoute) {
			// 竞态窗口（节点表刚清空）：回退本地不应发生（handler 已预判），
			// 如实报错让模型重试
			return tools.Result{}, fmt.Errorf("cluster: no matching node for this description")
		}
		return tools.Result{}, err
	}
	if bg {
		return tools.TextResult(fmt.Sprintf("remote subagent started in the background: id=%s label=%q\nYou will be notified when it settles; recall continuable subagents with list_agents and give follow-up work via send_message.", id, description)), nil
	}
	return tools.TextResult(fmt.Sprintf("remote subagent %s (%s) result:\n\n%s", id, description, out)), nil
}

// unpackArtifacts 产物落地：远端回传清单逐项从本地共享区取对象，解包到
// 工作区 cluster-artifacts/<句柄>/，返回给模型的说明文本（失败项如实
// 列出，不静默吞）。
func unpackArtifacts(router *cluster.Router, fs *tools.FsPolicy, handleID string, arts []cluster.ArtifactMeta) string {
	ws := ""
	if fs != nil {
		ws = fs.Workspace
	}
	var lines []string
	for _, am := range arts {
		switch {
		case am.Error != "":
			lines = append(lines, fmt.Sprintf("remote subagent %s: artifact return failed on the node: %s", handleID, am.Error))
		case am.Ref == "":
			continue
		default:
			f, _, err := router.Share().Open(am.Ref)
			if err != nil {
				lines = append(lines, fmt.Sprintf("remote subagent %s: artifacts unavailable locally (%v)", handleID, err))
				continue
			}
			dest := filepath.Join(ws, "cluster-artifacts", handleID)
			n, err := cluster.UnpackTo(f, dest)
			f.Close()
			if err != nil {
				lines = append(lines, fmt.Sprintf("remote subagent %s: artifact unpack failed (%v)", handleID, err))
				continue
			}
			lines = append(lines, fmt.Sprintf("remote subagent %s: %d artifact files unpacked to %s (relative to the workspace): %s",
				handleID, n, filepath.Join("cluster-artifacts", handleID), strings.Join(am.Files, ", ")))
		}
	}
	return strings.Join(lines, "\n")
}
