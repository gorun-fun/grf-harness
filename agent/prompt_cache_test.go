// prompt_cache_test.go 系统提示词的前缀缓存不变量：静态段字节级稳定地
// 位于最前，运行时事实（工作区/模型）与技能段一律后置。
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

// customBase 只替换静态指导段，运行时事实仍须附加（文档语义）。
func TestCustomBaseKeepsRuntimeFacts(t *testing.T) {
	p := BuildSystemPrompt(PromptOptions{Workspace: "/tmp/wsA", Model: "model-a"}, "自定义指导")
	if !strings.HasPrefix(p, "自定义指导\n\n") {
		t.Fatalf("customBase 应作为开头，got %q", p[:min(30, len(p))])
	}
	if !strings.Contains(p, "/tmp/wsA") || !strings.Contains(p, "model-a") {
		t.Fatalf("customBase 会话仍应携带运行时事实（工作区/模型）")
	}
}

// 技能段位于运行时事实之后（最易变的内容最后拼接）。
func TestSkillsSectionOrderedLast(t *testing.T) {
	c := tools.NewSkillCatalog()
	c.Add("grf-usage", "# 用法\n一些说明")
	p := BuildSystemPrompt(PromptOptions{Workspace: "/tmp/wsA", Model: "model-a", Skills: c}, "")
	idxRuntime := strings.Index(p, "## 运行环境")
	idxSkill := strings.Index(p, "## 可用技能")
	if idxSkill == -1 || idxSkill < idxRuntime {
		t.Fatalf("技能段应在运行时事实段之后")
	}
}
