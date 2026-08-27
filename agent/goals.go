// goals.go 同会话目标（goal）状态机：create_goal / get_goal / update_goal
// 三个工具 + 自动续跑驱动。目标仅根 agent 在人类回合可创建；blocked
// 需同一阻塞条件持续 ≥3 个 goal 轮次；update 校验 goal_id+revision 精确
// 匹配；resume/complete/blocked 驱动 RunAutoGoal 的续跑循环。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"git.gorun.fun/share/grf-harness/tools"
)

// Goal 目标状态。
type Goal struct {
	ID              string `json:"goal_id"`
	Objective       string `json:"objective"`
	Revision        int    `json:"revision"`
	Phase           string `json:"phase"` // working | paused | complete | blocked
	RoundsCompleted int    `json:"completed_continuation_rounds"`
	RoundLimit      int    `json:"round_limit"`
	BlockedReason   string `json:"blocked_reason,omitempty"`
	Armed           bool   `json:"another_continuation_armed"`
	CreatedAt       string `json:"created_at"`
}

const (
	goalDefaultRounds  = 10
	goalMaxRounds      = 50
	goalMinBlockRounds = 3
)

// GoalState 单会话目标（nil 目标 = 无活动目标）。
type GoalState struct {
	mu  sync.Mutex
	cur *Goal
	seq int
}

func (g *GoalState) snapshot() *Goal {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cur == nil {
		return nil
	}
	cp := *g.cur
	return &cp
}

// Active 当前活动（未完成/未阻塞/未暂停）目标。
func (g *GoalState) Active() *Goal {
	s := g.snapshot()
	if s == nil || s.Phase != "working" {
		return nil
	}
	return s
}

// DisarmAfterResume 会话恢复后解除武装（Restore 后调用）。
func (g *GoalState) DisarmAfterResume() {
	if g == nil {
		return
	}
	g.mu.Lock()
	if g.cur != nil && g.cur.Phase == "working" {
		g.cur.Armed = false
	}
	g.mu.Unlock()
}

// Marshal 当前目标 JSON（无目标时空）。
func (g *GoalState) Marshal() string {
	s := g.snapshot()
	if s == nil {
		return ""
	}
	data, _ := json.MarshalIndent(s, "", "  ")
	return string(data)
}

// RegisterGoalTools 注册三个 goal 工具（rootOnly 由调用方保证：子代理
// 不注册）。
func RegisterGoalTools(r *tools.Registry, a *Agent, goals *GoalState) {
	r.Register("create_goal", func(ctx context.Context, args string) (tools.Result, error) {
		var p struct {
			Objective     string `json:"objective"`
			MaxGoalRounds int    `json:"max_goal_rounds"`
		}
		if err := json.Unmarshal([]byte(args), &p); err != nil {
			return tools.Result{}, fmt.Errorf("invalid arguments: %w", err)
		}
		if strings.TrimSpace(p.Objective) == "" {
			return tools.Result{}, fmt.Errorf("objective is required")
		}
		if !a.isHumanTurn() {
			return tools.Result{}, fmt.Errorf("create_goal is rejected for non-human or subagent authority; goals belong to the direct human conversation")
		}
		goals.mu.Lock()
		defer goals.mu.Unlock()
		if goals.cur != nil && (goals.cur.Phase == "working" || goals.cur.Phase == "paused") {
			return tools.Result{}, fmt.Errorf("an active goal already exists (goal_id=%s, phase=%s); use update_goal to edit or complete it first", goals.cur.ID, goals.cur.Phase)
		}
		limit := goalDefaultRounds
		if p.MaxGoalRounds > 0 {
			limit = p.MaxGoalRounds
		}
		if limit > goalMaxRounds {
			limit = goalMaxRounds
		}
		goals.seq++
		goals.cur = &Goal{
			ID:         fmt.Sprintf("goal_%d_%s", goals.seq, shortID()),
			Objective:  p.Objective,
			Revision:   1,
			Phase:      "working",
			RoundLimit: limit,
			Armed:      true,
			CreatedAt:  time.Now().Format(time.RFC3339),
		}
		cp := *goals.cur
		a.emit("goal", fmt.Sprintf("created: %s", oneLine(cp.Objective, 80)))
		return tools.TextResult(goalCreatedText(&cp)), nil
	})

	r.Register("get_goal", func(ctx context.Context, args string) (tools.Result, error) {
		s := goals.snapshot()
		if s == nil {
			return tools.TextResult("no goal exists in this session; create one with create_goal when the direct human request is a long-running objective"), nil
		}
		return tools.TextResult(goals.Marshal()), nil
	})

	r.Register("update_goal", func(ctx context.Context, args string) (tools.Result, error) {
		var p struct {
			GoalID        string `json:"goal_id"`
			Revision      int    `json:"revision"`
			Action        string `json:"action"`
			Objective     string `json:"objective"`
			MaxGoalRounds int    `json:"max_goal_rounds"`
			BlockedReason string `json:"blocked_reason"`
		}
		if err := json.Unmarshal([]byte(args), &p); err != nil {
			return tools.Result{}, fmt.Errorf("invalid arguments: %w", err)
		}
		goals.mu.Lock()
		defer goals.mu.Unlock()
		cur := goals.cur
		if cur == nil {
			return tools.Result{}, fmt.Errorf("no goal exists in this session")
		}
		if p.GoalID != cur.ID {
			return tools.Result{}, fmt.Errorf("goal_id %q does not match the current goal %q; call get_goal and copy the exact id", p.GoalID, cur.ID)
		}
		if p.Revision != cur.Revision {
			return tools.Result{}, fmt.Errorf("revision %d does not match the current revision %d; call get_goal and copy the exact revision", p.Revision, cur.Revision)
		}
		switch p.Action {
		case "edit":
			if strings.TrimSpace(p.Objective) == "" && p.MaxGoalRounds <= 0 {
				return tools.Result{}, fmt.Errorf("edit requires objective and/or max_goal_rounds")
			}
			if strings.TrimSpace(p.Objective) != "" {
				cur.Objective = strings.TrimSpace(p.Objective)
			}
			if p.MaxGoalRounds > 0 {
				cur.RoundLimit = p.MaxGoalRounds
			}
			cur.Revision++
			a.emit("goal", "edited (r"+fmt.Sprint(cur.Revision)+")")
			return tools.TextResult("goal updated; the objective now reads:\n" + goals.Marshal()), nil
		case "pause":
			if cur.Phase != "working" {
				return tools.Result{}, fmt.Errorf("cannot pause a goal in phase %q", cur.Phase)
			}
			cur.Phase = "paused"
			cur.Armed = false
			cur.Revision++
			a.emit("goal", "paused")
			return tools.TextResult("goal paused; automatic continuation is disarmed until update_goal action resume"), nil
		case "resume":
			if cur.Phase != "working" && cur.Phase != "paused" {
				return tools.Result{}, fmt.Errorf("cannot resume a goal in phase %q", cur.Phase)
			}
			cur.Phase = "working"
			cur.Armed = true
			cur.Revision++
			a.emit("goal", "resumed/armed")
			return tools.TextResult("goal rearmed; automatic continuation will proceed toward the objective"), nil
		case "complete":
			if cur.Phase == "complete" {
				return tools.Result{}, fmt.Errorf("goal is already complete")
			}
			cur.Phase = "complete"
			cur.Armed = false
			cur.Revision++
			a.emit("goal", "complete")
			return tools.TextResult("goal marked complete; no further automatic continuation"), nil
		case "blocked":
			if strings.TrimSpace(p.BlockedReason) == "" {
				return tools.Result{}, fmt.Errorf("blocked_reason is required with action blocked")
			}
			if cur.RoundsCompleted < goalMinBlockRounds {
				return tools.Result{}, fmt.Errorf("blocked is rejected before the minimum of %d completed goal rounds (now %d); the same blocking condition must persist across rounds — keep working other angles meanwhile", goalMinBlockRounds, cur.RoundsCompleted)
			}
			cur.Phase = "blocked"
			cur.Armed = false
			cur.BlockedReason = strings.TrimSpace(p.BlockedReason)
			cur.Revision++
			a.emit("goal", "blocked: "+oneLine(cur.BlockedReason, 80))
			return tools.TextResult("goal marked blocked; report the concrete blocking condition to the user in your final answer"), nil
		}
		return tools.Result{}, fmt.Errorf("unknown action %q (use edit|pause|resume|complete|blocked)", p.Action)
	})
}

func goalCreatedText(g *Goal) string {
	return fmt.Sprintf("goal created:\n\n{goal_id: %q, revision: %d, objective: %q, phase: %q, round_limit: %d}\n\nuse get_goal before update_goal and copy the exact goal_id and revision; mark complete only when the objective is actually achieved",
		g.ID, g.Revision, g.Objective, g.Phase, g.RoundLimit)
}

// RunAutoGoal 执行一轮用户输入；目标处于 working+armed 且未到轮次上限时
// 自动续跑（非人类回合），直到 complete/blocked/paused/上限/取消。
func (a *Agent) RunAutoGoal(ctx context.Context, userInput string) (string, error) {
	final, err := a.RunAs(ctx, userInput, true)
	if err != nil {
		return final, err
	}
	if a.goals == nil {
		return final, nil
	}
	for {
		if ctx.Err() != nil {
			return final, ctx.Err()
		}
		g := a.goals.snapshot()
		if g == nil || !g.Armed || g.Phase != "working" || g.RoundsCompleted >= g.RoundLimit {
			return final, nil
		}
		a.goals.mu.Lock()
		a.goals.cur.RoundsCompleted++
		round := a.goals.cur.RoundsCompleted
		limit := a.goals.cur.RoundLimit
		objective := a.goals.cur.Objective
		a.goals.mu.Unlock()
		a.emit("goal", fmt.Sprintf("continuation round %d/%d", round, limit))
		prompt := fmt.Sprintf("(automatic goal continuation, round %d of %d — this is not a new human message)\nContinue working toward the goal objective: %s\nCall get_goal first to refresh the exact id/revision; keep working autonomously, then complete the goal with update_goal action complete when achieved, or blocked after the condition persisted across rounds.", round, limit, objective)
		var rerr error
		final, rerr = a.RunAs(ctx, prompt, false)
		if rerr != nil {
			if ctx.Err() != nil {
				return final, nil
			}
			return final, rerr
		}
	}
}

func (a *Agent) isHumanTurn() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.humanTurn && a.mgr != nil && a.mgr.Root() == a
}

func oneLine(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

var idCounter int64
var idMu sync.Mutex

func shortID() string {
	idMu.Lock()
	defer idMu.Unlock()
	idCounter++
	return fmt.Sprintf("%x", time.Now().UnixNano()&0xfffff0>>4|idCounter&0xf)
}
