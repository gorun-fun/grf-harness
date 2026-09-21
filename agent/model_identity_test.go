// model_identity_test.go 切换模型后系统提示词身份行的原位重写：构建期
// 按全局 default 烘焙的模型名不得残留（模型会按提示词自称旧模型）。
package agent

import (
	"strings"
	"testing"
)

func TestRewriteModelIdentity(t *testing.T) {
	sys := "静态指导段。\n\n## 运行环境\nYour working directory is /tmp.\n" +
		ModelIdentityLine("qwen/qwen3.8-27b") + "\n## 技能\n- s1\n"
	out := RewriteModelIdentity(sys, "glm-5.3-flash")
	if strings.Contains(out, "qwen") {
		t.Fatalf("旧模型名残留: %q", out)
	}
	if !strings.Contains(out, ModelIdentityLine("glm-5.3-flash")) {
		t.Fatalf("新身份行缺失: %q", out)
	}
	// 其余部分逐字保留(只替换身份行本身)
	want := strings.Replace(sys, ModelIdentityLine("qwen/qwen3.8-27b"), ModelIdentityLine("glm-5.3-flash"), 1)
	if out != want {
		t.Fatalf("rewrite must be surgical:\nwant %q\ngot  %q", want, out)
	}
	// 幂等:再次重写为同名应原样返回
	if RewriteModelIdentity(out, "glm-5.3-flash") != out {
		t.Fatal("rewrite to same model must be a no-op")
	}
	// 身份行不存在时原样返回(自定义提示词等)
	noLine := "no identity line here"
	if RewriteModelIdentity(noLine, "m") != noLine {
		t.Fatal("missing identity line must return input unchanged")
	}
}

func TestUpdateModelIdentitySyncsHistory(t *testing.T) {
	a := NewAgent("agent_root", nil, nil, nil, "", BuildSystemPrompt(PromptOptions{Workspace: "/tmp", Model: "qwen/qwen3.8-27b"}, ""))
	a.Reset() // history 头部带上 system
	a.UpdateModelIdentity("glm-5.3-flash")
	if !strings.Contains(a.SystemText(), ModelIdentityLine("glm-5.3-flash")) {
		t.Fatalf("system 未更新: %q", a.SystemText())
	}
	h := a.History()
	if len(h) == 0 || h[0].Role != "system" || !strings.Contains(h[0].Content, "glm-5.3-flash") {
		t.Fatalf("history 头部 system 未同步: %+v", h[:1])
	}
	// 空模型名不动作
	before := a.SystemText()
	a.UpdateModelIdentity("")
	if a.SystemText() != before {
		t.Fatal("empty model must be a no-op")
	}
}
