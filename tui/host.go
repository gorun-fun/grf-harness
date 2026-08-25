// host.go TUI 宿主：tools.Host 的全屏实现。工具 goroutine 发起请求 →
// 事件循环展示模态 → 通过 channel 回传结果（阻塞工具侧直到用户作答）。
package tui

import (
	"context"
	"fmt"
	"strings"

	"grf/config"
	"grf/store"
	"grf/tools"
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
	tool, command, mode, justification string
	resp                               chan bool
}

// toolApproveRequest 审批模式下的逐工具确认请求。
type toolApproveRequest struct {
	name, args string
	resp       chan bool
}

// tuiHost tools.Host 实现：请求转发到聊天事件循环。
type tuiHost struct{ c *Chat }

func newTUIHost(c *Chat) *tuiHost { return &tuiHost{c: c} }

func (h *tuiHost) AskUser(ctx context.Context, qs []tools.Question) (map[string]any, error) {
	req := &askRequest{qs: qs, resp: make(chan map[string]any, 1), errc: make(chan error, 1)}
	h.c.events <- req
	select {
	case a := <-req.resp:
		return a, nil
	case err := <-req.errc:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (h *tuiHost) ApproveEscalation(ctx context.Context, tool, command, mode, justification string) bool {
	req := &escRequest{tool: tool, command: command, mode: mode, justification: justification, resp: make(chan bool, 1)}
	select {
	case h.c.events <- req:
	case <-ctx.Done():
		return false
	}
	select {
	case v := <-req.resp:
		return v
	case <-ctx.Done():
		return false
	}
}

func (h *tuiHost) ApprovePlan(ctx context.Context, plan string) (bool, string) {
	req := &planRequest{plan: plan, approve: make(chan bool, 1), feedback: make(chan string, 1)}
	select {
	case h.c.events <- req:
	case <-ctx.Done():
		return false, ""
	}
	select {
	case ok := <-req.approve:
		return ok, <-req.feedback
	case <-ctx.Done():
		return false, ""
	}
}

func (h *tuiHost) ApproveTool(ctx context.Context, name, args string) bool {
	req := &toolApproveRequest{name: name, args: args, resp: make(chan bool, 1)}
	select {
	case h.c.events <- req:
	case <-ctx.Done():
		return false
	}
	select {
	case v := <-req.resp:
		return v
	case <-ctx.Done():
		return false
	}
}

func (h *tuiHost) NotifyEvent(kind, detail string) {
	// 事件已通过 agent onEvent 通道进入循环；此处无需重复。
}

// hostRequest 事件循环侧：安装对应模态。
func (c *Chat) hostRequest(m any) {
	switch req := m.(type) {
	case *askRequest:
		c.ask = newAskForm(req.qs, func(answers map[string]any, err error) {
			c.ask = nil
			if err != nil {
				req.errc <- err
			} else {
				req.resp <- answers
			}
		})
	case *planRequest:
		pv := &planView{plan: req.plan, editor: NewEditor("反馈: ")}
		pv.approve = func(approved bool, feedback string) {
			c.plan = nil
			req.approve <- approved
			req.feedback <- feedback
		}
		c.plan = pv
	case *escRequest:
		c.escv = &escApprove{tool: req.tool, command: req.command, mode: req.mode,
			justification: req.justification, approve: func(ok bool) {
				c.escv = nil
				req.resp <- ok
			}}
	case *toolApproveRequest:
		c.escv = &escApprove{title: "工具审批（审批模式）", tool: req.name, command: req.args,
			approve: func(ok bool) {
				c.escv = nil
				req.resp <- ok
			}}
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
		f.done(nil, fmt.Errorf("the user dismissed the questions"))
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

// planKey 计划审批按键。
func (c *Chat) planKey(k termKey) bool {
	f := c.plan
	if f.mode == 1 {
		switch k.Name {
		case "enter":
			feedback := f.editor.Value()
			f.approve(false, feedback)
			return false
		case "esc":
			f.mode = 0
			return false
		}
		editKey(f.editor, k)
		return false
	}
	switch k.Name {
	case "y", "Y":
		f.approve(true, "")
	case "f", "F":
		f.mode = 1
	case "esc":
		f.approve(false, "（用户关闭了计划审批，未给反馈；请继续规划或询问）")
	}
	return false
}

// escKey 升审批按键。
func (c *Chat) escKey(k termKey) bool {
	f := c.escv
	switch k.Name {
	case "y", "Y":
		f.approve(true)
	case "n", "N", "esc":
		f.approve(false)
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
		kind := p.kind
		c.pop = nil
		if p.onDone != nil {
			p.onDone(idx)
		}
		_ = kind
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
		c.emitBlock(block{kind: blkDim, text: "（尚未配置任何提供商，先运行 grf model 添加）"})
		return
	}
	c.pop = &popup{title: "选择模型", items: items, mark: cur, kind: "model",
		onDone: func(idx int) {
			spec := items[idx]
			parts := strings.SplitN(spec, "/", 2)
			if len(parts) != 2 {
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
	c.pop = &popup{title: "恢复会话", items: items, kind: "resume",
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
