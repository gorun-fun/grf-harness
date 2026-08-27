// misc.go ask_user_question / todo_write / skill / exit_plan_mode 四个工具。
// exit_plan_mode 依赖计划模式状态与计划审批宿主回调，由 agent 包在注册时
// 注入 PlanController 实现。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ---- ask_user_question ----

// RegisterAskUser 注册 ask_user_question。host 为宿主 getter（TUI 会在
// agent 构建后才接管交互，运行期取当前宿主而非注册时的快照）。
func RegisterAskUser(r *Registry, host func() Host) {
	r.Register("ask_user_question", func(ctx context.Context, args string) (Result, error) {
		var a struct {
			Questions []Question `json:"questions"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return Result{}, err
		}
		if len(a.Questions) == 0 {
			return Result{}, fmt.Errorf("questions array is required")
		}
		if len(a.Questions) > 4 {
			a.Questions = a.Questions[:4]
		}
		for i, q := range a.Questions {
			if strings.TrimSpace(q.ID) == "" || strings.TrimSpace(q.Question) == "" {
				return Result{}, fmt.Errorf("question %d: id and question are both required", i)
			}
			if len(q.Options) > 4 {
				a.Questions[i].Options = q.Options[:4]
			}
		}
		h := host()
		if h == nil {
			return Result{}, fmt.Errorf("no interactive user is available in this session; proceed without asking and state assumptions")
		}
		answers, err := h.AskUser(ctx, a.Questions)
		if err != nil {
			return Result{}, fmt.Errorf("the user could not answer: %v", err)
		}
		out, err := MarshalAnswers(answers)
		if err != nil {
			return Result{}, err
		}
		return TextResult(out), nil
	})
}

// RegisterAskUserDenied 子代理版本：明确拒绝（子代理无法触达最终用户）。
func RegisterAskUserDenied(r *Registry) {
	r.Register("ask_user_question", func(ctx context.Context, args string) (Result, error) {
		return Result{}, fmt.Errorf("ask_user_question is only available to the agent talking directly with the human; as a subagent, proceed without asking and state assumptions in your result")
	})
}

// ---- todo_write ----

// Todo 单条任务事项。
type Todo struct {
	Content string `json:"content"`
	Status  string `json:"status"` // pending | in_progress | completed
}

// TodoState 任务列表状态（整表替换语义）。
type TodoState struct {
	Items    []Todo
	OnChange func([]Todo)
}

// RegisterTodoWrite 注册 todo_write。state 由 agent 持有，UI 通过
// NotifyEvent("todos", json) 展示。
func RegisterTodoWrite(r *Registry, state *TodoState) {
	r.Register("todo_write", func(ctx context.Context, args string) (Result, error) {
		var a struct {
			Todos []Todo `json:"todos"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return Result{}, err
		}
		seen := map[string]bool{}
		for _, td := range a.Todos {
			switch td.Status {
			case "pending", "in_progress", "completed":
			default:
				return Result{}, fmt.Errorf("invalid status %q (use pending|in_progress|completed)", td.Status)
			}
			if strings.TrimSpace(td.Content) == "" {
				return Result{}, fmt.Errorf("todo content must not be empty")
			}
			if seen[td.Content] {
				return Result{}, fmt.Errorf("duplicate todo content %q", td.Content)
			}
			seen[td.Content] = true
		}
		state.Items = a.Todos
		if state.OnChange != nil {
			state.OnChange(a.Todos)
		}
		return TextResult(fmt.Sprintf("todo list updated (%d item%s)", len(a.Todos), plural(len(a.Todos)))), nil
	})
}

// ---- skill ----

// SkillCatalog 技能目录：workspace/.grf/skills 与 ~/.grf/skills 下的
// <name>/SKILL.md。
type SkillCatalog struct {
	entries map[string]string // name -> SKILL.md 全文
	order   []string
}

// LoadSkills 加载两级技能目录（工作区优先）。
func LoadSkills(workspace string) *SkillCatalog {
	c := &SkillCatalog{entries: map[string]string{}}
	home := userGrfHome()
	dirs := []string{filepath.Join(home, "skills"), filepath.Join(workspace, ".grf", "skills")}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			path := filepath.Join(dir, e.Name(), "SKILL.md")
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			name := e.Name()
			if _, ok := c.entries[name]; !ok {
				c.order = append(c.order, name)
			}
			c.entries[name] = string(data)
		}
	}
	sort.Strings(c.order)
	return c
}

// Names 可用技能名。
func (c *SkillCatalog) Names() []string { return append([]string(nil), c.order...) }

// Load 取技能全文。
func (c *SkillCatalog) Load(name string) (string, bool) {
	s, ok := c.entries[name]
	return s, ok
}

// Summary 首行摘要（system prompt 列表用）。
func (c *SkillCatalog) Summary(name string) string {
	s, _ := c.entries[name]
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "#")); t != "" {
			return clipRunes(t, 80)
		}
	}
	return ""
}

// RegisterSkill 注册 skill 工具。
func RegisterSkill(r *Registry, catalog *SkillCatalog) {
	r.Register("skill", func(ctx context.Context, args string) (Result, error) {
		var a struct {
			Name string `json:"name"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return Result{}, err
		}
		if catalog == nil || len(catalog.order) == 0 {
			return TextResult("no skills are available in this session"), nil
		}
		if a.Name == "" {
			return Result{}, fmt.Errorf("name is required; available skills: %v", catalog.Names())
		}
		content, ok := catalog.Load(a.Name)
		if !ok {
			return Result{}, fmt.Errorf("unknown skill %q; available skills: %v", a.Name, catalog.Names())
		}
		return TextResult(fmt.Sprintf("skill %q loaded; follow its instructions for the task at hand:\n\n%s", a.Name, content)), nil
	})
}

// ---- exit_plan_mode ----

// PlanController 计划模式控制器（agent 包实现）。
type PlanController interface {
	// InPlanMode 当前是否处于计划模式。
	InPlanMode() bool
	// SubmitPlan 呈交计划：返回 (已批准, 用户反馈)。反馈仅在未批准时有值。
	SubmitPlan(ctx context.Context, plan string) (bool, string)
}

// PlanNoop 默认控制器：不在计划模式。
type PlanNoop struct{}

func (PlanNoop) InPlanMode() bool                                           { return false }
func (PlanNoop) SubmitPlan(ctx context.Context, plan string) (bool, string) { return false, "" }

// RegisterExitPlanMode 注册 exit_plan_mode。
func RegisterExitPlanMode(r *Registry, pc PlanController, host Host) {
	r.Register("exit_plan_mode", func(ctx context.Context, args string) (Result, error) {
		var a struct {
			Plan string `json:"plan"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return Result{}, err
		}
		if strings.TrimSpace(a.Plan) == "" {
			return Result{}, fmt.Errorf("plan is required (the COMPLETE plan as markdown, starting with a # heading)")
		}
		if !strings.HasPrefix(strings.TrimSpace(a.Plan), "#") {
			return Result{}, fmt.Errorf("the plan must be markdown starting with a # heading that names it")
		}
		if !pc.InPlanMode() {
			return Result{}, fmt.Errorf("not in plan mode; exit_plan_mode is only usable in plan mode")
		}
		if host == nil {
			return Result{}, fmt.Errorf("no host available to review the plan")
		}
		approved, feedback := pc.SubmitPlan(ctx, a.Plan)
		if approved {
			return TextResult("The user approved the plan. You are now out of plan mode — carry out the plan from your next step."), nil
		}
		if strings.TrimSpace(feedback) == "" {
			feedback = "The user kept planning without written feedback. Ask what to adjust or revise the plan."
		}
		return TextResult("The plan was NOT approved. The user's feedback:\n\n" + feedback + "\n\nRevise the plan and present it again with exit_plan_mode."), nil
	})
}

// MarshalTodos 序列化 todo 列表（事件通知用）。
func MarshalTodos(todos []Todo) string {
	data, _ := json.Marshal(todos)
	return string(data)
}

// userGrfHome 用户级配置目录（GRF_HOME 覆盖 ~/.grf）。
func userGrfHome() string {
	if v := os.Getenv("GRF_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".grf"
	}
	return filepath.Join(home, ".grf")
}
