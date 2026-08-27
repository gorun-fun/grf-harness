// ralph.go ralph 工具：前台"新鲜代理"迭代循环——每轮开一个无对话种子的
// 子代理，共享工作区即长期记忆，只以有界的结构化报告跨轮传递。任一 worker
// 报告 COMPLETE（目标达成）或 BLOCKER（具体阻塞）即返回；上限轮数内未
// 达成则汇总收尾。仅根会话可用（直接人类显式要求）。
package agent

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"

	"git.gorun.fun/share/grf-harness/tools"
)

const (
	ralphDefaultRounds = 5
	ralphMaxRounds     = 20
)

// ralphRound 计数器（防递归：运行中拒绝再次进入）。
var ralphRunning atomic.Bool

const ralphPromptTail = `
You are a Ralph round worker. Rules:
- The workspace is your only long-term memory: write progress notes, partial results and findings to files under the workspace so later rounds can build on them.
- Read what earlier rounds left behind before redoing work.
- When the objective is fully achieved, end your final message with a line exactly: COMPLETE: <one-line summary>
- When you hit a concrete blocker you cannot work around, end with: BLOCKER: <the concrete blocking condition>
- Otherwise end with: PROGRESS: <what got done and what remains>
Do not ask questions; make reasonable assumptions and keep working.`

// RegisterRalph 注册 ralph 工具。
func RegisterRalph(r *tools.Registry, a *Agent, m *Manager) {
	r.Register("ralph", func(ctx context.Context, args string) (tools.Result, error) {
		var p struct {
			Objective string `json:"objective"`
			MaxRounds int    `json:"maxRounds"`
		}
		if err := tools.DecodeJSON(args, &p); err != nil {
			return tools.Result{}, err
		}
		if strings.TrimSpace(p.Objective) == "" {
			return tools.Result{}, fmt.Errorf("objective is required")
		}
		if !a.isHumanTurn() {
			return tools.Result{}, fmt.Errorf("ralph is reserved for the direct human conversation (explicitly requested fresh-agent iteration)")
		}
		if !ralphRunning.CompareAndSwap(false, true) {
			return tools.Result{}, fmt.Errorf("a ralph loop is already running in this session")
		}
		defer ralphRunning.Store(false)

		rounds := p.MaxRounds
		if rounds <= 0 {
			rounds = ralphDefaultRounds
		}
		if rounds > ralphMaxRounds {
			rounds = ralphMaxRounds
		}

		var report string
		for i := 1; i <= rounds; i++ {
			if ctx.Err() != nil {
				return tools.Result{}, ctx.Err()
			}
			a.emit("phase", fmt.Sprintf("ralph round %d/%d", i, rounds))
			label := fmt.Sprintf("ralph-r%d", i)
			prompt := fmt.Sprintf("Ralph round %d of %d.\nObjective (immutable): %s\n%s",
				i, rounds, strings.TrimSpace(p.Objective), ralphPromptTail)
			// 新鲜子代理：不注入任何父对话种子
			_, out, err := m.Subagent(a, label, prompt, false)
			if err != nil {
				return tools.Result{}, fmt.Errorf("ralph round %d failed to start: %w", i, err)
			}
			report = out
			verdict, tail := parseRalphReport(out)
			switch verdict {
			case "COMPLETE":
				return tools.TextResult(fmt.Sprintf("Ralph loop finished: a worker reported completion at round %d/%d.\n\nWorker report:\n\n%s", i, rounds, out)), nil
			case "BLOCKER":
				return tools.TextResult(fmt.Sprintf("Ralph loop stopped: a worker reported a concrete blocker at round %d/%d.\n\nBlocker: %s\n\nWorker report:\n\n%s", i, rounds, tail, out)), nil
			}
		}
		return tools.TextResult(fmt.Sprintf("Ralph loop reached its round limit (%d) without COMPLETE or BLOCKER. Last worker report:\n\n%s", rounds, report)), nil
	})
}

// parseRalphReport 识别报告结尾的 COMPLETE/BLOCKER/PROGRESS 标记行。
func parseRalphReport(out string) (verdict, tail string) {
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "COMPLETE:"):
			return "COMPLETE", strings.TrimSpace(strings.TrimPrefix(t, "COMPLETE:"))
		case strings.HasPrefix(t, "BLOCKER:"):
			return "BLOCKER", strings.TrimSpace(strings.TrimPrefix(t, "BLOCKER:"))
		}
	}
	return "PROGRESS", ""
}
