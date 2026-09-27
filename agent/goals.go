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
	return marshalGoal(g.snapshot())
}

// marshalGoal 目标快照序列化（Marshal 与 applyGoalEdit 的结果文本共用；
// edit 在 GoalState 锁内序列化副本，不能走自带锁的 Marshal）。
func marshalGoal(s *Goal) string {
	if s == nil {
		return ""
	}
	data, _ := json.MarshalIndent(s, "", "  ")
	return string(data)
}

// Create 直接创建目标（斜杠指令 /goal 用；与 create_goal 工具同一套
// 规则）。maxGoalRounds <= 0 时取默认值。返回创建后的目标快照。
func (g *GoalState) Create(objective string, maxGoalRounds int) (*Goal, error) {
	objective = strings.TrimSpace(objective)
	if objective == "" {
		return nil, fmt.Errorf("objective is required")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cur != nil && (g.cur.Phase == "working" || g.cur.Phase == "paused") {
		return nil, fmt.Errorf("an active goal already exists (goal_id=%s, phase=%s); use update_goal to edit or complete it first", g.cur.ID, g.cur.Phase)
	}
	limit := goalDefaultRounds
	if maxGoalRounds > 0 {
		limit = maxGoalRounds
	}
	if limit > goalMaxRounds {
		limit = goalMaxRounds
	}
	g.seq++
	g.cur = &Goal{
		ID:         fmt.Sprintf("goal_%d_%s", g.seq, shortID()),
		Objective:  objective,
		Revision:   1,
		Phase:      "working",
		RoundLimit: limit,
		Armed:      true,
		CreatedAt:  time.Now().Format(time.RFC3339),
	}
	cp := *g.cur
	return &cp, nil
}

// goalUpdate update_goal 工具参数（GoalState.Update 的输入）。
type goalUpdate struct {
	GoalID        string `json:"goal_id"`
	Revision      int    `json:"revision"`
	Action        string `json:"action"`
	Objective     string `json:"objective"`
	MaxGoalRounds int    `json:"max_goal_rounds"`
	BlockedReason string `json:"blocked_reason"`
}

// Update 执行 update_goal 的一次状态变更：goal_id+revision 精确匹配后按
// action 分发到下方 applyGoal* 小函数（锁内完成校验与迁移，Agent 侧不
// 直接触碰 GoalState 的私有锁与字段）。emit 在锁内回放 goal 事件（与
// 旧版 handler 持锁 emit 行为一致）；返回工具结果文本。
func (g *GoalState) Update(p goalUpdate, emit func(detail string)) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	cur := g.cur
	if cur == nil {
		return "", fmt.Errorf("no goal exists in this session")
	}
	if p.GoalID != cur.ID {
		return "", fmt.Errorf("goal_id %q does not match the current goal %q; call get_goal and copy the exact id", p.GoalID, cur.ID)
	}
	if p.Revision != cur.Revision {
		return "", fmt.Errorf("revision %d does not match the current revision %d; call get_goal and copy the exact revision", p.Revision, cur.Revision)
	}
	switch p.Action {
	case "edit":
		return applyGoalEdit(cur, p, emit)
	case "pause":
		return applyGoalPause(cur, emit)
	case "resume":
		return applyGoalResume(cur, emit)
	case "complete":
		return applyGoalComplete(cur, emit)
	case "blocked":
		return applyGoalBlocked(cur, p, emit)
	}
	return "", fmt.Errorf("unknown action %q (use edit|pause|resume|complete|blocked)", p.Action)
}

// applyGoalEdit 修改目标文案与/或轮次上限。结果文本需要序列化迁移后的
// 目标：这里序列化锁内做的副本（Marshal 自带锁，锁内直接调用会自死锁）。
func applyGoalEdit(cur *Goal, p goalUpdate, emit func(string)) (string, error) {
	if strings.TrimSpace(p.Objective) == "" && p.MaxGoalRounds <= 0 {
		return "", fmt.Errorf("edit requires objective and/or max_goal_rounds")
	}
	if strings.TrimSpace(p.Objective) != "" {
		cur.Objective = strings.TrimSpace(p.Objective)
	}
	if p.MaxGoalRounds > 0 {
		cur.RoundLimit = p.MaxGoalRounds
	}
	cur.Revision++
	emit("edited (r" + fmt.Sprint(cur.Revision) + ")")
	cp := *cur
	return "goal updated; the objective now reads:\n" + marshalGoal(&cp), nil
}

// applyGoalPause 暂停目标（解除自动续跑武装）。
func applyGoalPause(cur *Goal, emit func(string)) (string, error) {
	if cur.Phase != "working" {
		return "", fmt.Errorf("cannot pause a goal in phase %q", cur.Phase)
	}
	cur.Phase = "paused"
	cur.Armed = false
	cur.Revision++
	emit("paused")
	return "goal paused; automatic continuation is disarmed until update_goal action resume", nil
}

// applyGoalResume 恢复并重新武装目标。
func applyGoalResume(cur *Goal, emit func(string)) (string, error) {
	if cur.Phase != "working" && cur.Phase != "paused" {
		return "", fmt.Errorf("cannot resume a goal in phase %q", cur.Phase)
	}
	cur.Phase = "working"
	cur.Armed = true
	cur.Revision++
	emit("resumed/armed")
	return "goal rearmed; automatic continuation will proceed toward the objective", nil
}

// applyGoalComplete 标记目标完成（终止自动续跑）。
func applyGoalComplete(cur *Goal, emit func(string)) (string, error) {
	if cur.Phase == "complete" {
		return "", fmt.Errorf("goal is already complete")
	}
	cur.Phase = "complete"
	cur.Armed = false
	cur.Revision++
	emit("complete")
	return "goal marked complete; no further automatic continuation", nil
}

// applyGoalBlocked 标记目标阻塞：同一阻塞条件须持续 ≥goalMinBlockRounds
// 个已完成轮次，否则拒绝。
func applyGoalBlocked(cur *Goal, p goalUpdate, emit func(string)) (string, error) {
	if strings.TrimSpace(p.BlockedReason) == "" {
		return "", fmt.Errorf("blocked_reason is required with action blocked")
	}
	if cur.RoundsCompleted < goalMinBlockRounds {
		return "", fmt.Errorf("blocked is rejected before the minimum of %d completed goal rounds (now %d); the same blocking condition must persist across rounds — keep working other angles meanwhile", goalMinBlockRounds, cur.RoundsCompleted)
	}
	cur.Phase = "blocked"
	cur.Armed = false
	cur.BlockedReason = strings.TrimSpace(p.BlockedReason)
	cur.Revision++
	emit("blocked: " + oneLine(cur.BlockedReason, 80))
	return "goal marked blocked; report the concrete blocking condition to the user in your final answer", nil
}

// bumpRound 推进一轮自动续跑（RunAutoGoal 用；调用方已按 snapshot 校验
// 过 armed/phase/limit），返回推进后的轮次、上限与目标文案。
func (g *GoalState) bumpRound() (round, limit int, objective string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cur.RoundsCompleted++
	return g.cur.RoundsCompleted, g.cur.RoundLimit, g.cur.Objective
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
		cp, err := goals.Create(p.Objective, p.MaxGoalRounds)
		if err != nil {
			return tools.Result{}, err
		}
		a.emit("goal", fmt.Sprintf("created: %s", oneLine(cp.Objective, 80)))
		return tools.TextResult(goalCreatedText(cp)), nil
	})

	r.Register("get_goal", func(ctx context.Context, args string) (tools.Result, error) {
		s := goals.snapshot()
		if s == nil {
			return tools.TextResult("no goal exists in this session; create one with create_goal when the direct human request is a long-running objective"), nil
		}
		return tools.TextResult(goals.Marshal()), nil
	})

	r.Register("update_goal", func(ctx context.Context, args string) (tools.Result, error) {
		var p goalUpdate
		if err := json.Unmarshal([]byte(args), &p); err != nil {
			return tools.Result{}, fmt.Errorf("invalid arguments: %w", err)
		}
		text, err := goals.Update(p, func(detail string) { a.emit("goal", detail) })
		if err != nil {
			return tools.Result{}, err
		}
		return tools.TextResult(text), nil
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
		round, limit, objective := a.goals.bumpRound()
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
