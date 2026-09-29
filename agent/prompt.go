// prompt.go 系统提示词构建。custom 与 inner prompt 叠加而非二选一：
// custom 段（会话级，空则用内置默认）只承载身份定义与工作方式，
// inner prompt 段只承载内置工具的使用说明、按实际可用工具拼装——
// 两者恒同时存在，custom 覆盖不掉工具说明。段序按改变频率递增（缓存
// 友好）：最稳定的内容在前、最容易变的在后——服务端前缀缓存按最长
// 公共前缀命中，重排后即使某一段变化，也只作废它之后的尾部。改动本
// 文件时保持「稳定内容靠前、易变内容靠后」的排序，不要在靠前的段里
// 嵌入更高频变化的字符串：
//
//  1. custom 身份/工作方式段 —— 会话级（session meta / 配置文件；空用内置默认）
//  2. inner prompt 内置工具段 —— 会话级（按本角色可用工具拼装，同会话恒定）
//  3. 技能段                —— 项目级（SKILL.md 可被编辑）
//  4. 运行环境(工作区)      —— 项目级（同项目内恒定，会话内根/子代理共享）
//  5. 插件状态              —— 会话构建级，随每次连接结果变化
//  6. 子代理身份            —— 每个子代理唯一（同会话内亦变）
//  7. 模型身份行            —— 最易变：会话内可被 UpdateModelIdentity 改写
package agent

import (
	"fmt"
	"slices"
	"strings"

	"git.gorun.fun/share/grf-harness/tools"
)

// PromptOptions 系统提示词的运行时事实。
type PromptOptions struct {
	Workspace      string
	Model          string
	Skills         *tools.SkillCatalog // 可为 nil
	Subagent       bool                // 子代理身份（附加说明）
	Label          string              // 子代理标签
	PluginWarnings []string            // 不可用插件（为空不拼段）
	// DefaultSkill 默认技能名：技能目录中存在该技能时，其 SKILL.md 全文
	// 直接注入系统提示词（无需模型调用 skill 工具）。空值不注入任何默认
	// 技能；集成方经 agent/app Options 或 server 会话配置指定自己的默认
	// 技能，名字在目录中不存在时不拼默认技能段（技能清单不受影响）。
	DefaultSkill string
	// Tools 当前角色实际可用的工具名（registry.Registered() 的结果：已
	// 剔除禁用项与该角色未注册项）。inner prompt 只为其中出现的内置工具
	// 拼说明段；nil 视为全部内置工具可用（调用方负责按角色裁剪，gh 内部
	// systemPromptFor 一律传 registry 实况）。
	Tools []string
}

// 模型身份行（全文唯一，位于提示词末尾——最易变段）：会话构建期按当时
// client 的模型烘焙，切换模型后由 Agent.UpdateModelIdentity 原位重写，
// 保证模型自我认知与实际调用一致；放最后使改写只作废尾部缓存。
const (
	modelIdentityPrefix = "You are a coding agent powered by the "
	modelIdentitySuffix = " model.\n"
)

// ModelIdentityLine 生成模型身份行原文。
func ModelIdentityLine(model string) string {
	return modelIdentityPrefix + model + modelIdentitySuffix
}

// RewriteModelIdentity 把 system 提示词中的模型身份行改为 model，其余
// 逐字保留。身份行不存在（历史形状等）时原样返回。
func RewriteModelIdentity(system, model string) string {
	i := strings.Index(system, modelIdentityPrefix)
	if i < 0 {
		return system
	}
	j := strings.Index(system[i+len(modelIdentityPrefix):], modelIdentitySuffix)
	if j < 0 {
		return system
	}
	end := i + len(modelIdentityPrefix) + j + len(modelIdentitySuffix)
	return system[:i] + ModelIdentityLine(model) + system[end:]
}

// BuildSystemPrompt 生成 system prompt。custom 非空时作为身份/工作方式
// 段（替换内置默认 custom），但 inner prompt（内置工具使用说明）与其后
// 的运行时事实段仍然恒定附加——custom 与工具说明是叠加关系，不是二选一。
// 段序见文件头注释。
func BuildSystemPrompt(o PromptOptions, custom string) string {
	var b strings.Builder
	if custom != "" {
		b.WriteString(strings.TrimRight(custom, "\n"))
	} else {
		b.WriteString(strings.TrimRight(defaultCustomBody(), "\n"))
	}
	if inner := innerPromptBody(o.Tools); inner != "" {
		b.WriteString("\n\n")
		b.WriteString(inner)
	}
	b.WriteString("\n")

	// 技能段：项目级（SKILL.md 可被编辑）。默认技能名由集成方/终端
	// 形态显式配置（PromptOptions.DefaultSkill）：目录中存在该名字的
	// 技能时其 SKILL.md 全文注入；空值不注入任何默认技能——终端用法
	// 技能 grf-usage 只服务 gh CLI 终端形态（bootApp 显式配置），不强加
	// 给使用方式不同的三方集成。
	if o.Skills != nil && len(o.Skills.Names()) > 0 {
		if o.DefaultSkill != "" {
			if content, ok := o.Skills.Load(o.DefaultSkill); ok {
				b.WriteString("\n## 默认技能（已加载全文）\n")
				b.WriteString(content)
				b.WriteString("\n")
			}
		}
		b.WriteString("\n## 可用技能（skill 工具加载全文）\n")
		for _, name := range o.Skills.Names() {
			fmt.Fprintf(&b, "- %s: %s\n", name, o.Skills.Summary(name))
		}
	}

	// 工作区路径：项目级（同项目内恒定）——会话内根与全部子代理共享，
	// 前置到插件状态等更易变段之前，最大化公共前缀。
	b.WriteString("\n## 运行环境\n")
	fmt.Fprintf(&b, "Your working directory is %s. Use pwd via the bash tool to confirm the current working directory when path precision matters.\n", o.Workspace)

	// 插件状态：会话构建级（随每次构建的连接结果变化；根与子代理都拼
	// ——子代理同样注册了插件工具，需要知道哪些插件不可用）。
	if len(o.PluginWarnings) > 0 {
		b.WriteString("\n## 插件状态\n以下插件不可用，调用其工具会失败，请向用户说明并避免依赖：\n- ")
		b.WriteString(strings.Join(o.PluginWarnings, "\n- "))
		b.WriteString("\n")
	}

	// 子代理身份：每个子代理唯一（label 不同）。委派范围声明（dsh 对齐）：
	// 子代理的交互宿主是 NoopHost——越权升级构造性必拒且不可从内部扩宽，
	// 明说出来让模型被拒后带限制回报而非反复重试。
	if o.Subagent {
		b.WriteString("\n## 子代理身份\n")
		fmt.Fprintf(&b, "You are running as a background subagent (label %q). You do not see the parent conversation; rely only on the prompt you were given and the workspace. Complete the self-contained task and reply with the final result — not your intermediate steps. You may not ask the user questions and may not create session goals. You are a delegated subagent: your permission scope was fixed when you were started and cannot be widened from inside this session — operations that require approval are rejected automatically. When the job needs access beyond that scope, do not retry the denied operation; state the limitation in your reply so the delegating agent can handle it.\n", o.Label)
	}

	// 模型身份行：最易变（会话内可切换模型被改写），恒在末尾。
	b.WriteString("\n")
	b.WriteString(ModelIdentityLine(o.Model))
	return b.String()
}

// DefaultSkill gh CLI 终端交互形态的默认技能名：bootApp 显式配置为
// 本名字，目录中存在该技能时全文直接注入系统提示词（无需模型主动调用
// skill 工具）。终端用法技能只服务终端形态——三方集成（库嵌入方、
// server 接入方）默认不注入任何默认技能，经 Options/会话配置指定
// 自己的。
const DefaultSkill = "grf-usage"

// defaultCustomBody 内置默认 custom 段：身份定义 + 工作方式。刻意不含
// 任何内置工具的使用说明——那些由 innerPromptBody 恒定拼装，用户自定义
// custom 替换的本段时也不丢。不得嵌入工作区路径、模型名、日期等会话间
// 变化的值。
func defaultCustomBody() string {
	var b strings.Builder
	b.WriteString("You are an AI agent powered by grf-harness, talking to the user through a terminal fullscreen UI.\n\n")

	b.WriteString("The workspace root may contain an AGENTS.md with project conventions. It is NOT auto-loaded into your context — read it early when your task touches the codebase, and keep it in mind for the session.\n\n")

	b.WriteString("Paths prefixed with @ are files explicitly referenced by the user. Use the read tool when their contents are needed; do not claim to have inspected a file before reading it.\n\n")

	b.WriteString("When you successfully create or modify files, mention the primary outputs in your final response, formatted as Markdown inline code using the exact file-tool path.\n")
	return b.String()
}

// innerPromptBody 内置工具使用说明段（原 rootPrompt 解耦而来：只描述
// 内置工具的使用）。每个说明段对应一个/一组内置工具，仅当该工具实际
// 可用（未被 Options.DisabledTools 禁用、本角色已注册，即出现在 avail
// 中）时拼入；avail 为 nil 视为全部内置工具可用。段序固定（与 spec
// 文档顺序对齐），同配置下字节级稳定。
func innerPromptBody(avail []string) string {
	has := func(names ...string) bool {
		if avail == nil {
			return true
		}
		for _, n := range names {
			if slices.Contains(avail, n) {
				return true
			}
		}
		return false
	}
	var secs []string
	add := func(names []string, text string) {
		if has(names...) {
			secs = append(secs, text)
		}
	}

	add([]string{"ask_user_question"},
		"When you genuinely need the user's confirmation, a choice, or missing information before proceeding, ask with ask_user_question — concise questions, one stable id per question — instead of guessing on consequential decisions.")

	add([]string{"bash"},
		"Check the [exit code: N] marker on every bash result; investigate failures before moving on.")

	add([]string{"create_goal", "get_goal", "update_goal"},
		"Use goal tools for one long-running completion objective in the current session. create_goal may infer goal intent from a direct human request in any language; do not create a goal for routine single-turn work. Call get_goal before update_goal and copy its exact goal_id and revision. After session resume or fork, an active goal is disarmed: when a human asks to continue or resume in any wording or language, use update_goal action resume to rearm it. Mark complete only when the objective is actually achieved. Mark blocked only after the same blocking condition persists for at least 3 consecutive goal rounds, and report that concrete condition in blocked_reason; difficulty, uncertainty, or useful remaining work is not blocked.")

	add([]string{"edit"},
		"Use the edit tool for targeted changes to existing UTF-8 text files. It replaces literal old_string with new_string; by default old_string must appear exactly once. If old_string appears multiple times, provide a more specific old_string or set replace_all to true. Read the file first (the default fs-observation-policy requires it), unless you just created or edited it in this session.")

	add([]string{"exit_plan_mode"},
		"Plan mode: stay in it until exit_plan_mode is approved. Investigate freely but change nothing — mutating tools (write/edit/bash/ralph/workflow) are rejected outright. Explore first: ground the plan in the actual repository with non-mutating reads, searches and checks; prefer existing functions and patterns over new machinery, and resolve discoverable facts by inspection — ask_user_question is only for user-owned choices or material ambiguity inspection cannot answer. A user's conversational agreement (including an answer confirming something you asked) approves nothing and does not end plan mode: fold the confirmed decision into the plan and submit it through exit_plan_mode. Do not track this planning phase with todo_write — it tracks implementation after an approved plan. Make the plan decision-complete: goal and success criteria, changes grouped by subsystem, public API/schema/data-flow changes, edge cases, failure modes, tests, and explicit assumptions — concise enough to review, detailed enough that another engineer could implement it without making design decisions. Present it as complete markdown starting with a # heading, make exit_plan_mode the only tool call of that response, and wait: approval starts implementation in a later step; rejection returns feedback — incorporate it and present again.")

	add([]string{"enter_plan_mode"},
		"The user can enable plan mode manually; for a non-trivial implementation task — a new feature, a multi-file change, an architectural choice, unclear requirements — you may also request it yourself by calling enter_plan_mode with a short reason BEFORE making changes. The user must approve the switch; a decline with feedback means you continue executing directly. Skip plan mode for trivial or well-specified work.")

	add([]string{"glob"},
		"Use the glob tool to find files by path pattern when you need paths rather than contents; it returns file paths only (including hidden and ignored files) in modification-time order.")

	add([]string{"grep"},
		"Use the grep tool — not shell grep or rg — to search file contents. Use read on a matched file when you need surrounding context.")

	add([]string{"interrupt_agent", "list_agents", "send_message"},
		"You are notified in-session when a background subagent finishes — do not poll for completion. Use list_agents to recall which subagents are continuable, send_message to hand a subagent more work (the message waits for its current turn and cannot redirect work already underway), and interrupt_agent to cancel a background agent's current turn.")

	add([]string{"job_kill", "job_list", "job_output"},
		"Track every background job id you start. You are notified in-session when a job finishes — do not busy-poll or sleep on one; keep working on independent steps and do not duplicate a running job's work. Before giving a final answer, collect every still-relevant job with job_output (set wait: true only when you are genuinely blocked on it), and job_kill jobs that stopped mattering.")

	add([]string{"memory"},
		"Long-term memory persists across sessions as JSON entry stores reached ONLY through the memory tool: project memory (the default scope) at .grf/memory.json in this workspace for project conventions, decisions and environment quirks, and user memory at ~/.grf/memory.json for the user's cross-project preferences. List memory early in a substantive session; when the user states a durable preference, decision or correction worth keeping, add it under a short unique topic key of your choosing (e.g. build-cmd); recall entries later with find, which matches exact keys first and then substrings of keys or contents. Each store is capped at 4KB — when full, update or delete stale entries instead of adding more. Never record secrets or transient task state, and never touch these files with read/write/edit.")

	add([]string{"ralph"},
		"Use ralph — a foreground fresh-agent loop toward one immutable objective — only when the user explicitly asks for Ralph or fresh-agent iteration; ordinary long-running same-session work belongs to goal tools.")

	add([]string{"read"},
		"Use the read tool — not shell commands like cat — to inspect text files. Results include line numbers. Use offset and limit to continue reading large files.")

	add([]string{"read_image"},
		"Use the read_image tool to look at PNG/JPEG/WebP/GIF images — the harness validates and downscales them for the model; do not install image libraries or shell out to create thumbnails merely to inspect an image.")

	add([]string{"set_policy"},
		"Use set_policy only when the current sandbox policy genuinely blocks the requested work: you only trigger the prompt and give a reason, the user personally picks the new rule, and a decline is final — do not retry or work around it.")

	add([]string{"skill"},
		"Call the skill tool with the exact skill name from the session skill catalog to load a skill's full instructions before acting on a task that names or clearly matches that skill.")

	add([]string{"subagent", "subagent_fork"},
		"Delegate focused, independent work with subagent (fresh context) or subagent_fork (the child inherits this conversation's completed turns) to keep it out of this context. Give subagent a complete standalone prompt — it does not see this conversation. Both run in the background and notify you on completion; you get the final result, not the intermediate steps.")

	add([]string{"todo_write"},
		"Use the todo_write tool to plan and track multi-step work in the session task list, shown to the user as a live todo panel. Before starting non-trivial work, break it into concrete steps — one todo per step — and send the ENTIRE list every call: it replaces the previous list (no partial updates). Mark the todo you are actively working on in_progress (several only when work genuinely runs in parallel, e.g. concurrent subagents or background commands) and mark a todo completed the moment it is done — never batch completions. While work remains, at least one todo stays in_progress; all completed means the task is done. Skip the list for trivial single-step work.")

	add([]string{"web_search"},
		"Use the web_search tool to discover current information on the web. The required queries array accepts 1–4 non-empty search queries; use a one-item array for a single search. It returns an optional answer plus a list of source URLs. Use the returned source snippets when available, and cite the relevant URLs as markdown links.")

	add([]string{"workflow"},
		"Use the workflow tool ONLY when the user explicitly asks for a workflow or for large multi-agent orchestration: you write a JavaScript script (the tool description documents the exact format) that fans work out across many subagents with phases and structured results. For one or two delegations, prefer plain subagent calls.")

	add([]string{"write"},
		"Use the write tool to create files or completely replace file contents. Existing files are overwritten, so read an existing file first (the default fs-observation-policy requires it) and prefer edit for targeted changes.")

	if len(secs) == 0 {
		return ""
	}
	return "## 内置工具使用说明\n\n" + strings.Join(secs, "\n\n")
}
