// model_identity_test.go 切换模型后系统提示词身份行的原位重写：构建期
// 按全局 default 烘焙的模型名不得残留（模型会按提示词自称旧模型）。
package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"git.gorun.fun/share/grf-harness/llm"
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

// SwitchClient 必须同时换 root 与 harness 的 client：只换 root 时子代理仍拿
// 旧 client（旧模型 + 旧身份行），切换在同一会话的子代理身上静默失效。
func TestSwitchClientCoversRootAndChildren(t *testing.T) {
	f := newFakeLLM([]string{"ok"})
	defer f.Close()
	h := buildTestHarness(t, f, t.TempDir())
	defer h.Close()

	next := llm.NewClient(llm.Config{BaseURL: f.URL(), APIKey: "t", Model: "glm-5.3-flash"})
	h.SwitchClient(next)

	if got := h.Root.Model(); got != "glm-5.3-flash" {
		t.Fatalf("root client 未切换: %q", got)
	}
	if got := h.getClient().Model(); got != "glm-5.3-flash" {
		t.Fatalf("harness client 未切换（子代理会跑旧模型）: %q", got)
	}
	if next.CacheKey() != h.CacheKey() {
		t.Fatalf("缓存路由键未迁移: %q != %q", next.CacheKey(), h.CacheKey())
	}
	for label, a := range map[string]*Agent{"root": h.Root, "child": h.newChild(h.Root, "s1", nil)} {
		if !strings.Contains(a.SystemText(), ModelIdentityLine("glm-5.3-flash")) ||
			strings.Contains(a.SystemText(), "test-model") {
			t.Fatalf("%s 提示词身份行未重写: %q", label, a.SystemText())
		}
	}
}

// SwitchClient 可与运行中的回合并发调用（client 引用经锁保护；本用例
// 只在 -race 下有检测力——覆盖 turn 循环的 ChatStream 读、newChild 的
// 提示词装配读与 managedEnv 的 bash 事实变量读）。
func TestSwitchClientConcurrentWithTurn(t *testing.T) {
	f := newFakeLLM([]string{"ok"})
	defer f.Close()
	h := buildTestHarness(t, f, t.TempDir())
	defer h.Close()

	stop := make(chan struct{})
	switched := make(chan struct{})
	go func() {
		defer close(switched)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			h.SwitchClient(llm.NewClient(llm.Config{BaseURL: f.URL(), APIKey: "t", Model: fmt.Sprintf("m-%d", i)}))
		}
	}()
	for i := 0; i < 5; i++ {
		if _, err := h.Root.Run(context.Background(), "hi"); err != nil {
			t.Errorf("Run: %v", err)
		}
		_ = h.systemPromptFor(nil, true, "s1", "") // newChild 的提示词装配读路径
		_ = h.managedEnv()                    // bash 事实变量读路径
	}
	close(stop)
	<-switched
}
