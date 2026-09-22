// prompt_cache_test.go 系统提示词的前缀缓存不变量：段序按改变频率递增，
// 静态段字节级稳定地位于最前，模型身份行（会话内唯一可被改写的段）恒在
// 末尾——某段变化时只作废其后的尾部缓存。
package agent

import (
	"strings"
	"testing"

	"git.gorun.fun/share/grf-harness/tools"
)

// 不同工作区/模型的提示词必须共享完整静态段作为公共前缀
// （服务端前缀缓存按最长公共前缀命中）。
func TestBuildSystemPromptStableStaticPrefix(t *testing.T) {
	a := BuildSystemPrompt(PromptOptions{Workspace: "/tmp/wsA", Model: "model-a"}, "")
	b := BuildSystemPrompt(PromptOptions{Workspace: "/tmp/other/wsB", Model: "model-b"}, "")

	static := rootPromptBody()
	if !strings.HasPrefix(a, static) || !strings.HasPrefix(b, static) {
		t.Fatalf("静态指导段必须是所有提示词的公共前缀（缓存前缀被破坏）")
	}
	// 静态段要足够长，重排才有意义（防止退化成一行前缀）。
	if len(static) < 4000 {
		t.Fatalf("静态段过短：%d 字节", len(static))
	}

	// 运行时事实必须出现在静态段之后。
	for _, p := range []string{a, b} {
		idxStatic := strings.LastIndex(p, "the exact file-tool path.")
		idxRuntime := strings.Index(p, "## 运行环境")
		if idxStatic == -1 || idxRuntime == -1 || idxRuntime < idxStatic {
			t.Fatalf("运行时事实段必须位于静态段之后")
		}
	}
	if !strings.Contains(a, "/tmp/wsA") || !strings.Contains(a, "model-a") {
		t.Fatalf("运行时事实缺失")
	}
}

// 段序不变量：静态段 → 技能 → 运行环境(工作区) → 插件状态 → 子代理身份
// → 模型身份行，按改变频率递增。
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
		idx("the exact file-tool path."), // 静态段末尾
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

// customBase 只替换静态指导段，其余段仍须附加（文档语义）；且与内置
// 静态段路径使用相同的单空行分隔。
func TestCustomBaseKeepsRuntimeFacts(t *testing.T) {
	p := BuildSystemPrompt(PromptOptions{Workspace: "/tmp/wsA", Model: "model-a"}, "自定义指导")
	if !strings.HasPrefix(p, "自定义指导\n\n") {
		t.Fatalf("customBase 应作为开头，got %q", p[:min(30, len(p))])
	}
	if !strings.Contains(p, "/tmp/wsA") || !strings.Contains(p, "model-a") {
		t.Fatalf("customBase 会话仍应携带运行时事实（工作区/模型）")
	}
	if strings.Contains(p, "\n\n\n") {
		t.Fatalf("customBase 与后续段之间应恰好一个空行: %q", p[:40])
	}
}

// 技能段位于静态段之后、运行环境（项目级同伴段）之前。
func TestSkillsSectionOrder(t *testing.T) {
	c := tools.NewSkillCatalog()
	c.Add("grf-usage", "# 用法\n一些说明")
	p := BuildSystemPrompt(PromptOptions{Workspace: "/tmp/wsA", Model: "model-a", Skills: c}, "")
	idxStatic := strings.LastIndex(p, "the exact file-tool path.")
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

// 插件警告为空时不拼段。
func TestNoPluginSectionWithoutWarnings(t *testing.T) {
	p := BuildSystemPrompt(PromptOptions{Workspace: "/tmp/wsA", Model: "model-a"}, "")
	if strings.Contains(p, "## 插件状态") {
		t.Fatalf("无插件警告时不应出现插件状态段")
	}
}
