// prompt_cache_test.go 系统提示词的前缀缓存不变量：段序按改变频率递增，
// 静态段（custom + inner prompt，同会话恒定）字节级稳定地位于最前，模型
// 身份行（会话内唯一可被改写的段）恒在末尾——某段变化时只作废其后的
// 尾部缓存。
package agent

import (
	"strings"
	"testing"

	"git.gorun.fun/share/grf-harness/tools"
)

// defaultStaticSegment 默认形态（无 custom 覆盖、全量工具）的静态段原文
// （custom 默认段 + inner prompt 段），与 BuildSystemPrompt 的拼装逐字
// 一致——作为不同工作区/模型提示词的公共前缀基准。
func defaultStaticSegment() string {
	return strings.TrimRight(defaultCustomBody(), "\n") + "\n\n" + innerPromptBody(nil) + "\n"
}

// 不同工作区/模型的提示词必须共享完整静态段作为公共前缀
// （服务端前缀缓存按最长公共前缀命中）。
func TestBuildSystemPromptStableStaticPrefix(t *testing.T) {
	a := BuildSystemPrompt(PromptOptions{Workspace: "/tmp/wsA", Model: "model-a"}, "")
	b := BuildSystemPrompt(PromptOptions{Workspace: "/tmp/other/wsB", Model: "model-b"}, "")

	static := defaultStaticSegment()
	if !strings.HasPrefix(a, static) || !strings.HasPrefix(b, static) {
		t.Fatalf("静态段必须是所有提示词的公共前缀（缓存前缀被破坏）")
	}
	// 静态段要足够长，重排才有意义（防止退化成一行前缀）。
	if len(static) < 4000 {
		t.Fatalf("静态段过短：%d 字节", len(static))
	}

	// 运行时事实必须出现在静态段之后。
	for _, p := range []string{a, b} {
		idxStatic := strings.LastIndex(p, "## 内置工具使用说明")
		idxRuntime := strings.Index(p, "## 运行环境")
		if idxStatic == -1 || idxRuntime == -1 || idxRuntime < idxStatic {
			t.Fatalf("运行时事实段必须位于静态段之后")
		}
	}
	if !strings.Contains(a, "/tmp/wsA") || !strings.Contains(a, "model-a") {
		t.Fatalf("运行时事实缺失")
	}
}

// 段序不变量：custom/inner 静态段 → 技能 → 运行环境(工作区) → 插件状态
// → 子代理身份 → 模型身份行，按改变频率递增。
func TestSectionOrderByVolatility(t *testing.T) {
	c := tools.NewSkillCatalog()
	c.Add("grf-usage", "# 用法\n一些说明")
	p := BuildSystemPrompt(PromptOptions{
		Workspace:      "/tmp/wsA",
		Model:          "model-a",
		Skills:         c,
		Subagent:       true,
		Label:          "s1",
		PluginWarnings: []string{"grf-browser: connect refused"},
	}, "")
	idx := func(marker string) int {
		i := strings.Index(p, marker)
		if i < 0 {
			t.Fatalf("缺少段 %q", marker)
		}
		return i
	}
	order := []int{
		idx("the exact file-tool path."), // custom 默认段末尾（inner 段之前）
		idx("## 内置工具使用说明"),        // inner prompt 段
		idx("## 可用技能"),
		idx("## 运行环境"),
		idx("## 插件状态"),
		idx("## 子代理身份"),
		idx(ModelIdentityLine("model-a")),
	}
	for i := 1; i < len(order); i++ {
		if order[i] <= order[i-1] {
			t.Fatalf("段序违背改变频率递增：%v", order)
		}
	}
	// 模型身份行必须位于最末（其后只有换行）。
	if tail := p[idx(ModelIdentityLine("model-a"))+len(ModelIdentityLine("model-a")):]; strings.TrimSpace(tail) != "" {
		t.Fatalf("模型身份行之后不应再有其它段: %q", tail)
	}
}

// custom 只替换身份/工作方式段：inner prompt（内置工具说明）与运行时
// 事实段仍然附加（叠加关系，非二选一）；且与后续段使用相同的单空行分隔。
func TestCustomBaseKeepsInnerPromptAndRuntimeFacts(t *testing.T) {
	p := BuildSystemPrompt(PromptOptions{Workspace: "/tmp/wsA", Model: "model-a"}, "自定义指导")
	if !strings.HasPrefix(p, "自定义指导\n\n") {
		t.Fatalf("custom 应作为开头，got %q", p[:min(30, len(p))])
	}
	if !strings.Contains(p, "## 内置工具使用说明") {
		t.Fatalf("custom 不应替换 inner prompt（内置工具说明段恒定拼装）")
	}
	if !strings.Contains(p, "/tmp/wsA") || !strings.Contains(p, "model-a") {
		t.Fatalf("custom 会话仍应携带运行时事实（工作区/模型）")
	}
	if strings.Contains(p, "\n\n\n") {
		t.Fatalf("custom 与后续段之间应恰好一个空行: %q", p[:40])
	}
}

// inner prompt 按可用工具拼装：禁用（未出现在 Tools 中）的工具不拼对应
// 说明段，未禁用的必须都有说明。
func TestInnerPromptFollowsAvailableTools(t *testing.T) {
	o := PromptOptions{Workspace: "/tmp/wsA", Model: "model-a"}
	full := BuildSystemPrompt(o, "")
	trimmed := BuildSystemPrompt(PromptOptions{
		Workspace: o.Workspace,
		Model:     o.Model,
		Tools:     []string{"read", "write", "grep", "bash"},
	}, "")
	if !strings.Contains(trimmed, "## 内置工具使用说明") {
		t.Fatalf("有可用内置工具时 inner prompt 段必须存在")
	}
	// 未禁用工具的说明段必须在（用 inner 段独有文案断言，避开 custom
	// 默认段里对 read 工具的引用）。
	for _, present := range []string{
		"not shell commands like cat",  // read
		"completely replace file contents", // write
		"[exit code: N]",               // bash
		"not shell grep or rg",         // grep
	} {
		if !strings.Contains(trimmed, present) {
			t.Fatalf("未禁用工具的说明段缺失: %q", present)
		}
	}
	// 禁用的工具说明段必须消失（memory/todo/web_search/plan/goal/ralph
	// 等未出现在 Tools 中）。
	for _, absent := range []string{
		"Long-term memory",
		"todo_write tool",
		"web_search tool",
		"Plan mode:",
		"goal tools",
		"ralph",
	} {
		if strings.Contains(trimmed, absent) {
			t.Fatalf("禁用工具的说明段不应出现: %q", absent)
		}
	}
	// 全量形态（Tools 为 nil）必须覆盖全部内置工具的说明段。
	for _, present := range []string{
		"ask_user_question", "bash", "goal tools", "edit tool",
		"Plan mode:", "glob tool", "grep tool", "list_agents",
		"job_output", "Long-term memory", "ralph", "read tool",
		"read_image tool", "set_policy", "skill tool", "subagent",
		"todo_write tool", "web_search tool", "workflow tool", "write tool",
	} {
		if !strings.Contains(full, present) {
			t.Fatalf("全量形态缺少内置工具说明段: %q", present)
		}
	}
}

// 全部内置工具被禁用时 inner prompt 段整体消失（无可用工具可说明）。
func TestInnerPromptOmittedWhenNoTools(t *testing.T) {
	p := BuildSystemPrompt(PromptOptions{Workspace: "/tmp/wsA", Model: "model-a", Tools: []string{}}, "")
	if strings.Contains(p, "## 内置工具使用说明") {
		t.Fatalf("无可用内置工具时不应出现 inner prompt 段")
	}
	if !strings.Contains(p, "/tmp/wsA") {
		t.Fatalf("运行时事实仍应携带")
	}
}

// 技能段位于静态段之后、运行环境（项目级同伴段）之前。
func TestSkillsSectionOrder(t *testing.T) {
	c := tools.NewSkillCatalog()
	c.Add("grf-usage", "# 用法\n一些说明")
	p := BuildSystemPrompt(PromptOptions{Workspace: "/tmp/wsA", Model: "model-a", Skills: c}, "")
	idxStatic := strings.LastIndex(p, "## 内置工具使用说明")
	idxSkill := strings.Index(p, "## 可用技能")
	idxRuntime := strings.Index(p, "## 运行环境")
	idxModel := strings.Index(p, ModelIdentityLine("model-a"))
	if idxSkill == -1 || idxSkill < idxStatic {
		t.Fatalf("技能段应在静态段之后")
	}
	if idxRuntime == -1 || idxRuntime < idxSkill {
		t.Fatalf("运行环境段应在技能段之后")
	}
	if idxModel == -1 || idxModel < idxRuntime {
		t.Fatalf("模型身份行应在运行环境段之后（最易变段最后拼接）")
	}
}

// 默认技能按配置注入：指定名字时其全文注入（grf-usage 不再特殊）；
// 未配置（空值）不注入任何默认技能——终端用法技能 grf-usage 只由 gh
// CLI 终端形态显式配置，不强加给三方集成；配置的名字不存在时同样
// 不拼默认技能段（技能清单不受影响）。
func TestDefaultSkillConfigurable(t *testing.T) {
	c := tools.NewSkillCatalog()
	c.Add("grf-usage", "# 内置默认技能\n用法说明")
	c.Add("grf-start", "# 集成方总纲\n游戏创作指南")
	base := PromptOptions{Workspace: "/tmp/wsA", Model: "model-a", Skills: c}

	p := BuildSystemPrompt(base, "")
	if strings.Contains(p, "## 默认技能（已加载全文）") {
		t.Fatalf("未配置默认技能时不应注入任何默认技能（含 grf-usage）")
	}
	if !strings.Contains(p, "## 可用技能") || !strings.Contains(p, "- grf-usage: ") {
		t.Fatalf("技能清单仍应拼装且包含 grf-usage 条目")
	}

	p = BuildSystemPrompt(PromptOptions{Workspace: base.Workspace, Model: base.Model, Skills: c, DefaultSkill: "grf-start"}, "")
	if !strings.Contains(p, "## 默认技能（已加载全文）\n# 集成方总纲") {
		t.Fatalf("配置的默认技能应全文注入")
	}

	p = BuildSystemPrompt(PromptOptions{Workspace: base.Workspace, Model: base.Model, Skills: c, DefaultSkill: "nope"}, "")
	if strings.Contains(p, "## 默认技能（已加载全文）") {
		t.Fatalf("配置的默认技能名不存在时不应拼默认技能段")
	}
	if !strings.Contains(p, "## 可用技能") {
		t.Fatalf("技能清单仍应拼装")
	}
}

// 插件警告为空时不拼段。
func TestNoPluginSectionWithoutWarnings(t *testing.T) {
	p := BuildSystemPrompt(PromptOptions{Workspace: "/tmp/wsA", Model: "model-a"}, "")
	if strings.Contains(p, "## 插件状态") {
		t.Fatalf("无插件警告时不应出现插件状态段")
	}
}
