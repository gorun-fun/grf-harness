// prompt.go 系统提示词构建：静态指导段（工具使用纪律、后台作业、goal、
// workflow/ralph、子代理后台默认等）+ 运行时事实段（当前工作目录、实际
// 模型、技能目录）。静态段恒定在前、易变的运行时事实一律后置——服务端
// 前缀缓存按最长公共前缀命中，这样跨项目/跨模型也能共享静态前缀；改动
// 本文件时保持「稳定内容靠前、易变内容靠后」的排序，不要在静态段里嵌
// 入任何会话间会变化的字符串。
package agent

import (
	"fmt"
	"strings"

	"git.gorun.fun/share/grf-harness/tools"
)

// PromptOptions 系统提示词的运行时事实。
type PromptOptions struct {
	Workspace string
	Model     string
	Skills    *tools.SkillCatalog // 可为 nil
	Subagent  bool                // 子代理身份（附加说明）
	Label     string              // 子代理标签
}

// 模型身份行（运行环境段内、全文唯一）：会话构建期按当时 client 的模型
// 烘焙，切换模型后由 Agent.UpdateModelIdentity 原位重写，保证模型自我
// 认知与实际调用一致。
const (
	modelIdentityPrefix = "You are a coding agent powered by the "
	modelIdentitySuffix = " model.\n"
)

// ModelIdentityLine 生成模型身份行原文。
func ModelIdentityLine(model string) string {
	return modelIdentityPrefix + model + modelIdentitySuffix
}

// RewriteModelIdentity 把 system 提示词中的模型身份行改为 model，其余
// 逐字保留。身份行不存在（customBase 之外的历史形状等）时原样返回。
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

// BuildSystemPrompt 生成根会话 system prompt。段序（缓存友好）：静态指导
// 段（或 customBase 替换）→ 运行时事实 → 子代理身份 → 技能列表。customBase
// 非空时只替换静态指导部分（用户自定义提示词），运行时事实与技能列表仍
// 然附加。
func BuildSystemPrompt(o PromptOptions, customBase string) string {
	var b strings.Builder
	if customBase != "" {
		b.WriteString(strings.TrimRight(customBase, "\n"))
		b.WriteString("\n\n")
	} else {
		b.WriteString(rootPromptBody())
	}

	// 运行时事实段：工作区路径/模型名随会话变化，后置在静态段之后，
	// 保证静态前缀字节级稳定（customBase 会话同样携带）。
	b.WriteString("\n## 运行环境\n")
	fmt.Fprintf(&b, "Your working directory is %s. Use pwd via the bash tool to confirm the current working directory when path precision matters.\n", o.Workspace)
	b.WriteString(ModelIdentityLine(o.Model))

	if o.Subagent {
		b.WriteString("\n## 子代理身份\n")
		fmt.Fprintf(&b, "You are running as a background subagent (label %q). You do not see the parent conversation; rely only on the prompt you were given and the workspace. Complete the self-contained task and reply with the final result — not your intermediate steps. You may not ask the user questions and may not create session goals.\n", o.Label)
	}
	if o.Skills != nil && len(o.Skills.Names()) > 0 {
		if content, ok := o.Skills.Load(DefaultSkill); ok {
			b.WriteString("\n## 默认技能（已加载全文）\n")
			b.WriteString(content)
			b.WriteString("\n")
		}
		b.WriteString("\n## 可用技能（skill 工具加载全文）\n")
		for _, name := range o.Skills.Names() {
			fmt.Fprintf(&b, "- %s: %s\n", name, o.Skills.Summary(name))
		}
	}
	return b.String()
}

// DefaultSkill 默认加载技能名：目录中存在该技能时，全文直接注入系统
// 提示词（无需模型主动调用 skill 工具）。
const DefaultSkill = "grf-usage"

// rootPromptBody 静态指导段：与运行环境无关的固定内容（缓存前缀）。
// 不得嵌入工作区路径、模型名、日期等会话间变化的值。
func rootPromptBody() string {
	var b strings.Builder
	b.WriteString("You are an AI agent powered by grf-harness, talking to the user through a terminal fullscreen UI.\n\n")

	b.WriteString("The workspace root may contain an AGENTS.md with project conventions. It is NOT auto-loaded into your context — read it early when your task touches the codebase, and keep it in mind for the session.\n\n")

	b.WriteString("Long-term memory persists across sessions as JSON entry stores reached ONLY through the memory tool: project memory (the default scope) at .grf/memory.json in this workspace for project conventions, decisions and environment quirks, and user memory at ~/.grf/memory.json for the user's cross-project preferences. List memory early in a substantive session; when the user states a durable preference, decision or correction worth keeping, add it under a short unique topic key of your choosing (e.g. build-cmd); recall entries later with find, which matches exact keys first and then substrings of keys or contents. Each store is capped at 4KB — when full, update or delete stale entries instead of adding more. Never record secrets or transient task state, and never touch these files with read/write/edit.\n\n")

	b.WriteString("Paths prefixed with @ are files explicitly referenced by the user. Use the read tool when their contents are needed; do not claim to have inspected a file before reading it.\n\n")

	b.WriteString("Use the read tool — not shell commands like cat — to inspect text files. Results include line numbers. Use offset and limit to continue reading large files.\n\n")

	b.WriteString("Use the write tool to create files or completely replace file contents. Existing files are overwritten, so read an existing file first (the default fs-observation-policy requires it) and prefer edit for targeted changes.\n\n")

	b.WriteString("Use the edit tool for targeted changes to existing UTF-8 text files. It replaces literal old_string with new_string; by default old_string must appear exactly once. If old_string appears multiple times, provide a more specific old_string or set replace_all to true. Read the file first (the default fs-observation-policy requires it), unless you just created or edited it in this session.\n\n")

	b.WriteString("Use the grep tool — not shell grep or rg — to search file contents. Use read on a matched file when you need surrounding context.\n\n")

	b.WriteString("Check the [exit code: N] marker on every bash result; investigate failures before moving on.\n\n")

	b.WriteString("Track every background job id you start. You are notified in-session when a job finishes — do not busy-poll or sleep on one; keep working on independent steps and do not duplicate a running job's work. Before giving a final answer, collect every still-relevant job with job_output (set wait: true only when you are genuinely blocked on it), and job_kill jobs that stopped mattering.\n\n")

	b.WriteString("Use the web_search tool to discover current information on the web. The required queries array accepts 1–4 non-empty search queries; use a one-item array for a single search. It returns an optional answer plus a list of source URLs. Use the returned source snippets when available, and cite the relevant URLs as markdown links.\n\n")

	b.WriteString("Use goal tools for one long-running completion objective in the current session. create_goal may infer goal intent from a direct human request in any language; do not create a goal for routine single-turn work. Call get_goal before update_goal and copy its exact goal_id and revision. After session resume or fork, an active goal is disarmed: when a human asks to continue or resume in any wording or language, use update_goal action resume to rearm it. Mark complete only when the objective is actually achieved. Mark blocked only after the same blocking condition persists for at least 3 consecutive goal rounds, and report that concrete condition in blocked_reason; difficulty, uncertainty, or useful remaining work is not blocked.\n\n")

	b.WriteString("Use the workflow tool ONLY when the user explicitly asks for a workflow or for large multi-agent orchestration: you write a JavaScript script (the tool description documents the exact format) that fans work out across many subagents with phases and structured results. For one or two delegations, prefer plain subagent calls.\n\n")

	b.WriteString("Plan mode: investigate freely but change nothing — mutating tools (write/edit/bash/ralph/workflow) are rejected outright; present your implementation plan with exit_plan_mode and wait for the user's approval before executing it. The user can enable it manually; for a non-trivial implementation task — a new feature, a multi-file change, an architectural choice, unclear requirements — you may also request it yourself by calling enter_plan_mode with a short reason BEFORE making changes. The user must approve the switch; a decline with feedback means you continue executing directly. Skip plan mode for trivial or well-specified work.\n\n")

	b.WriteString("When you successfully create or modify files, mention the primary outputs in your final response, formatted as Markdown inline code using the exact file-tool path.\n")
	return b.String()
}
