// prompt_hostctx_test.go — 宿主环境简报段（HostContext）的拼装语义:
// 独立段、拼在 custom 身份段之后 / inner prompt 之前、空值不拼段。
package agent

import (
	"strings"
	"testing"
)

func TestBuildSystemPromptHostContext(t *testing.T) {
	base := BuildSystemPrompt(PromptOptions{Workspace: "/ws", Model: "m"}, "你是测试身份。")
	if strings.Contains(base, "宿主环境简报") {
		t.Fatalf("未配置 HostContext 时不应拼段:\n%s", base)
	}

	got := BuildSystemPrompt(PromptOptions{
		Workspace:   "/ws",
		Model:       "m",
		HostContext: "你运行在宿主桌面,当前用户 alice。",
	}, "你是测试身份。")

	iCustom := strings.Index(got, "你是测试身份。")
	iHost := strings.Index(got, "## 宿主环境简报")
	iInner := strings.Index(got, "## 内置工具")
	if iCustom < 0 || iHost < 0 {
		t.Fatalf("custom 段或宿主简报段缺失:\n%s", got)
	}
	if !(iCustom < iHost) {
		t.Fatalf("宿主简报段应在 custom 身份段之后(custom@%d host@%d)", iCustom, iHost)
	}
	if iInner >= 0 && iInner < iHost {
		t.Fatalf("宿主简报段应在 inner prompt 之前(inner@%d host@%d)", iInner, iHost)
	}
	if !strings.Contains(got, "你运行在宿主桌面,当前用户 alice。") {
		t.Fatalf("宿主简报正文缺失:\n%s", got)
	}
}
