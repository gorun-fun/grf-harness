package agent

import (
	"context"
	"strings"
	"testing"

	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/llm"
	"git.gorun.fun/share/grf-harness/tools"
)

// wfFakeLLM workflow 场景的假服务：直接回文本。
func wfHarness(t *testing.T, responses []string) (*Harness, *fakeLLM) {
	t.Helper()
	f := newFakeLLM(responses)
	st := &config.Settings{Sandbox: "off", FsPolicy: "observe"}
	h, err := Build(Options{
		Workspace: t.TempDir(),
		Client:    llm.NewClient(llm.Config{BaseURL: f.URL(), APIKey: "t", Model: "m"}),
		Settings:  st,
		Host:      &tools.NoopHost{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	t.Cleanup(func() { h.Close() })
	return h, f
}

func TestWorkflowMetaValidation(t *testing.T) {
	h, _ := wfHarness(t, []string{"x"})
	res := h.Root.reg.Execute(context.Background(), "workflow", `{"script":"return 1"}`)
	if !strings.Contains(res.Text, "meta is required") {
		t.Fatalf("meta required: %s", res.Text)
	}
	res = h.Root.reg.Execute(context.Background(), "workflow", `{"script":"return 1","meta":{"name":"x"}}`)
	if !strings.Contains(res.Text, "name and description") {
		t.Fatalf("description required: %s", res.Text)
	}
	res = h.Root.reg.Execute(context.Background(), "workflow", `{"script":"return 1","meta":{"name":"has space","description":"d"}}`)
	if !strings.Contains(res.Text, "kebab-case") {
		t.Fatalf("name kebab: %s", res.Text)
	}
}

func TestWorkflowScriptBasics(t *testing.T) {
	// 脚本只用 phase/log/args，无 agent 调用
	h, _ := wfHarness(t, []string{"ok"})
	script := `
phase("collect");
log("args=" + JSON.stringify(args));
const items = args.files;
const out = items.map(f => f.toUpperCase());
return { count: out.length, files: out };
`
	args := `{"script":` + quoteJSON(script) + `,"meta":{"name":"demo","description":"demo workflow"},"args":{"files":["a.md","b.md"]}}`
	res := h.Root.reg.Execute(context.Background(), "workflow", args)
	if !strings.Contains(res.Text, `"count":2`) || !strings.Contains(res.Text, `"A.MD"`) {
		t.Fatalf("workflow result: %s", res.Text)
	}
}

func TestWorkflowWithAgents(t *testing.T) {
	// 两个并发 agent 调用：子代理直接回文本
	h, _ := wfHarness(t, []string{"result-A", "result-B", "final"})
	script := `
const out = await parallel([
  () => agent("task a"),
  () => agent("task b"),
]);
return out.filter(Boolean).join("|");
`
	args := `{"script":` + quoteJSON(script) + `,"meta":{"name":"two","description":"two agents"}}`
	res := h.Root.reg.Execute(context.Background(), "workflow", args)
	// 并发子代理从共享假服务取响应的顺序不定，两种排列均可
	ok := strings.Contains(res.Text, "result-A|result-B") || strings.Contains(res.Text, "result-B|result-A")
	if !ok {
		t.Fatalf("workflow agents: %s", res.Text)
	}
}

func TestWorkflowPipelineNulls(t *testing.T) {
	h, _ := wfHarness(t, []string{"ok"})
	script := `
const out = await pipeline([1, 2], (prev, item) => {
  if (item === 2) { throw new Error("nope"); }
  return prev * 10;
});
return out;
`
	args := `{"script":` + quoteJSON(script) + `,"meta":{"name":"p","description":"p"}}`
	res := h.Root.reg.Execute(context.Background(), "workflow", args)
	// item 2 抛错 → null
	if !strings.Contains(res.Text, `10,null`) {
		t.Fatalf("pipeline: %s", res.Text)
	}
}

func TestWorkflowHostMisuseKills(t *testing.T) {
	h, _ := wfHarness(t, []string{"ok"})
	script := `const v = await agent(123); return v;`
	args := `{"script":` + quoteJSON(script) + `,"meta":{"name":"bad","description":"bad"}}`
	res := h.Root.reg.Execute(context.Background(), "workflow", args)
	if !strings.Contains(res.Text, "error") || !strings.Contains(res.Text, "non-empty string prompt") {
		t.Fatalf("misuse should fail loudly: %s", res.Text)
	}
}

func quoteJSON(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
