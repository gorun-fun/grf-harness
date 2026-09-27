// host.go TUI 宿主：tools.Host 的全屏实现。工具 goroutine 发起请求 →
// 事件循环展示模态 → 通过 channel 回传结果（阻塞工具侧直到用户作答）。
package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/store"
	"git.gorun.fun/share/grf-harness/tools"
)

// askRequest / planRequest / escRequest Host 请求（经 events 通道入循环）。
type askRequest struct {
	qs   []tools.Question
	resp chan map[string]any
	errc chan error
}

type planRequest struct {
	plan     string
	approve  chan bool
	feedback chan string
}

type escRequest struct {
	tool, key, mode, justification string
	resp                           chan tools.EscalationDecision
}

// toolApproveRequest 审批模式下的逐工具确认请求（支持拒绝反馈）。
type toolApproveRequest struct {
	name, args string
	resp       chan tools.ApproveDecision
}

// tuiHost tools.Host 实现：请求转发到聊天事件循环。
type tuiHost struct{ c *Chat }

func newTUIHost(c *Chat) *tuiHost { return &tuiHost{c: c} }

// sendHostRequest 请求经 events 通道送入事件循环。发送也要带 ctx 兜底：
// events 缓冲占满（事件循环被阻塞）时裸发送会永久卡死且不响应取消；
// 返回 false 表示 ctx 已取消，调用方直接返回各自的零值。
func sendHostRequest[T any](ctx context.Context, events chan any, req T) bool {
	select {
	case events <- req:
		return true
	case <-ctx.Done():
		return false
	}
}

func (h *tuiHost) AskUser(ctx context.Context, qs []tools.Question) (map[string]any, error) {
	req := &askRequest{qs: qs, resp: make(chan map[string]any, 1), errc: make(chan error, 1)}
	if !sendHostRequest(ctx, h.c.events, req) {
		return nil, ctx.Err()
	}
	select {
	case a := <-req.resp:
		return a, nil
	case err := <-req.errc:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (h *tuiHost) ApproveEscalation(ctx context.Context, tool, key, mode, justification string) tools.EscalationDecision {
	req := &escRequest{tool: tool, key: key, mode: mode, justification: justification,
		resp: make(chan tools.EscalationDecision, 1)}
	if !sendHostRequest(ctx, h.c.events, req) {
		return tools.EscalationDecision{}
	}
	select {
	case v := <-req.resp:
		return v
	case <-ctx.Done():
		return tools.EscalationDecision{}
	}
}

func (h *tuiHost) ApprovePlan(ctx context.Context, plan string) (bool, string) {
	req := &planRequest{plan: plan, approve: make(chan bool, 1), feedback: make(chan string, 1)}
	if !sendHostRequest(ctx, h.c.events, req) {
		return false, ""
	}
	select {
	case ok := <-req.approve:
		return ok, <-req.feedback
	case <-ctx.Done():
		return false, ""
	}
}

func (h *tuiHost) ApproveTool(ctx context.Context, name, args string) tools.ApproveDecision {
	req := &toolApproveRequest{name: name, args: args, resp: make(chan tools.ApproveDecision, 1)}
	if !sendHostRequest(ctx, h.c.events, req) {
		return tools.ApproveDecision{}
	}
	select {
	case v := <-req.resp:
		return v
	case <-ctx.Done():
		return tools.ApproveDecision{}
	}
}

func (h *tuiHost) NotifyEvent(kind, detail string) {
	// 事件已通过 agent onEvent 通道进入循环；此处无需重复。
}

// hostRequest 事件循环侧：安装对应模态。
func (c *Chat) hostRequest(m any) {
	switch req := m.(type) {
	case *askRequest:
		c.ask = newAskForm(req.qs, func(answers map[string]any, _ map[string]int, err error) {
			c.ask = nil
			if err != nil {
				req.errc <- err
			} else {
				req.resp <- answers
			}
		})
	case *planRequest:
		// 计划审批复用 ask 选择框：批准 / 继续计划，✎ 自定义输入作反馈。
		// 决定按选项下标判定（picked），不受选项文案改动或自定义输入
		// 恰与文案相同的影响
		q := tools.Question{ID: "approve_plan",
			Header:   "计划审批",
			Question: req.plan,
			Options: []tools.Option{
				{Label: "批准，按计划执行", Description: "退出计划模式并开始执行"},    // picked = 0
				{Label: "继续计划", Description: "不批准；用 ✎ 行输入反馈后重新提交"}, // picked = 1
			}}
		c.ask = newAskForm([]tools.Question{q}, func(answers map[string]any, picked map[string]int, err error) {
			c.ask = nil
			switch {
			case err != nil:
				req.approve <- false
				req.feedback <- "（用户关闭了计划审批，未给反馈；请继续规划或询问）"
			default:
				v, _ := answers["approve_plan"].(string)
				switch picked["approve_plan"] {
				case 0: // 批准，按计划执行
					req.approve <- true
					req.feedback <- ""
				case 1: // 继续计划
					req.approve <- false
					req.feedback <- ""
				default: // ✎ 自定义输入：视为拒绝并附反馈（空输入即无反馈）
					req.approve <- false
					req.feedback <- v
				}
			}
		})
		c.ask.head = "计划审批"
	case *escRequest:
		// 沙箱升级审批复用 ask 选择框：批准（仅此一次）/ 拒绝；
		// write/edit 额外提供"批准并授权所在目录"（持久化到工作区
		// .grf/grants.json，仅本工作区生效，之后该目录下写操作不再询问）
		var b strings.Builder
		b.WriteString("工具 " + req.tool + " 申请越权（模式 " + req.mode + "）")
		if req.key != "" {
			b.WriteString("\n参数:\n" + clipRunesT(req.key, 2000))
		}
		if req.justification != "" {
			b.WriteString("\n理由: " + req.justification)
		}
		opts := []tools.Option{
			{Label: "批准，仅此一次", Description: "放行这一次越权操作"}, // picked = 0
		}
		dir := ""
		if req.tool == "write" || req.tool == "edit" {
			dir = filepath.Dir(req.key)
			opts = append(opts, tools.Option{
				Label:       "批准，并授权目录 " + dir, // picked = 1（仅 write/edit 存在）
				Description: "保存到工作区 .grf，仅本工作区生效，之后该目录下写操作不再询问"})
		}
		opts = append(opts, tools.Option{Label: "拒绝", Description: "本次操作不放行（其他操作不受影响）"})
		q := tools.Question{ID: "approve_escalation", Question: b.String(), Options: opts}
		// 决定按选项下标判定（picked），不受选项文案改动或自定义输入
		// 恰与文案相同的影响
		c.ask = newAskForm([]tools.Question{q}, func(_ map[string]any, picked map[string]int, err error) {
			c.ask = nil
			d := tools.EscalationDecision{}
			if err == nil {
				switch picked["approve_escalation"] {
				case 0: // 批准，仅此一次
					d.Approved = true
				case 1: // write/edit 时为"批准，并授权目录"；其余工具此下标是"拒绝"
					if dir != "" {
						d.Approved = true
						d.GrantDir = dir
					}
				}
			}
			req.resp <- d
		})
		c.ask.head = "沙箱升级审批"
	case *toolApproveRequest:
		q := tools.Question{ID: "approve",
			Question: "工具 " + req.name + " · 参数:\n" + clipRunesT(req.args, 2000),
			Options: []tools.Option{
				{Label: "批准执行"}, // picked = 0
				{Label: "拒绝"},   // picked = 1
			}}
		c.ask = newAskForm([]tools.Question{q}, func(answers map[string]any, picked map[string]int, err error) {
			c.ask = nil
			d := tools.ApproveDecision{}
			switch {
			case err != nil:
				d.Feedback = "（用户关闭了工具审批，未给反馈）"
			default:
				switch picked["approve"] {
				case 0: // 批准执行
					d.Approved = true
				case 1: // 拒绝：不带反馈
				default: // ✎ 自定义输入视为拒绝并附反馈
					d.Feedback, _ = answers["approve"].(string)
				}
			}
			req.resp <- d
		})
		c.ask.head = "工具审批"
	}
	c.paint(true)
}

// askKey ask 表单按键。选项题的光标可落在末尾"✎ 自定义输入"行，回车
// 切入手输模式（Esc 返回选项）；多选勾选仅作用于真实选项行。
func (c *Chat) askKey(k termKey) bool {
	f := c.ask
	q := f.q()
	n := len(q.Options)
	switch k.Name {
	case "esc":
		if f.custom {
			f.custom = false
			return false
		}
		f.done(nil, nil, fmt.Errorf("the user dismissed the questions"))
		c.ask = nil
	case "up":
		if n > 0 && !f.custom && f.sel > 0 {
			f.sel--
		}
	case "down":
		if n > 0 && !f.custom && f.sel < n {
			f.sel++
		}
	case "space":
		if q.MultiSelect && f.sel < n {
			f.checked[f.sel] = !f.checked[f.sel]
		} else if f.text != nil && (n == 0 || f.custom) {
			f.text.InsertRune(' ')
		}
	case "enter":
		if n > 0 && !f.custom && f.sel == n {
			f.custom = true
			return false
		}
		f.finishQuestion()
		if c.ask != nil && c.ask.cur >= len(c.ask.qs) {
			c.ask = nil
		}
	default:
		if n == 0 || f.custom {
			editKey(f.text, k)
		}
	}
	return false
}

// popKey 列表弹窗按键。
func (c *Chat) popKey(k termKey) bool {
	p := c.pop
	switch k.Name {
	case "up":
		if p.sel > 0 {
			p.sel--
		}
	case "down":
		if p.sel < len(p.items)-1 {
			p.sel++
		}
	case "enter":
		idx := p.sel
		c.pop = nil
		if p.onDone != nil {
			p.onDone(idx)
		}
	case "esc":
		c.pop = nil
	}
	return false
}

// openModelPopup / openResumePopup 弹窗数据装配。
func (c *Chat) openModelPopup() {
	items, cur, err := c.app.ListModelChoices()
	if err != nil {
		c.emitBlock(block{kind: blkErr, text: "读取配置失败: " + err.Error()})
		return
	}
	if len(items) == 0 {
		c.emitBlock(block{kind: blkDim, text: "（尚未配置任何提供商，先运行 gh model 添加）"})
		return
	}
	c.pop = &popup{title: "选择模型", items: items, mark: cur,
		onDone: func(idx int) {
			spec := items[idx]
			parts := strings.SplitN(spec, "/", 2)
			if len(parts) != 2 {
				return
			}
			// 弹窗打开期间后台作业落定可自动续跑回合：执行时再校验一次
			if c.running {
				c.emitBlock(block{kind: blkErr, text: "当前有任务在执行，等空闲后再切换模型"})
				return
			}
			if err := c.app.SwitchModel(parts[0], parts[1]); err != nil {
				c.emitBlock(block{kind: blkErr, text: "切换模型失败: " + err.Error()})
			} else {
				c.emitBlock(block{kind: blkText, text: "已切换会话模型（仅本次会话，不改默认）: " + spec})
			}
		}}
	// 初始选中当前默认
	for i, it := range items {
		if it == cur {
			c.pop.sel = i
		}
	}
}

func (c *Chat) openResumePopup() {
	sessions := c.app.Harness.Store.ListSessions(20)
	if len(sessions) == 0 {
		c.emitBlock(block{kind: blkDim, text: "（没有可恢复的历史会话）"})
		return
	}
	var items []string
	for _, s := range sessions {
		when := s.When.Format("01-02 15:04")
		if s.When.IsZero() {
			when = "更早"
		}
		items = append(items, when+" · "+s.Title)
	}
	c.pop = &popup{title: "恢复会话", items: items,
		onDone: func(idx int) { c.doRestoreByInfo(sessions[idx]) }}
}

// doRestoreByInfo 按索引恢复。
func (c *Chat) doRestoreByInfo(info store.SessionInfo) {
	sess, err := store.LoadSessionFull(info.Path)
	if err != nil {
		c.emitBlock(block{kind: blkErr, text: "读取会话失败: " + err.Error()})
		return
	}
	c.app.Restore(sess)
	c.resetForReplay("已恢复会话: " + info.Title)
}

// loadSettings / saveMousePref 配置读写。
func loadSettings() (*config.Settings, error) { return config.LoadFileForUpdate() }

func (c *Chat) saveMousePref(on bool) {
	if s, err := loadSettings(); err == nil {
		v := on
		s.Mouse = &v
		_ = s.Save()
	}
}
