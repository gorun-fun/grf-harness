// options_test.go app.Options 的库接入行为：运行策略覆盖、配置文件
// 提示词回退、VerboseEvents 接线。GRF_HOME 指到临时目录隔离全局配置。
package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.gorun.fun/share/grf-harness/llm"
)

// isolateConfig 把 GRF_HOME 指到临时目录，可选写入一份 settings.json。
func isolateConfig(t *testing.T, settingsJSON string) {
	t.Helper()
	home := t.TempDir()
	if settingsJSON != "" {
		if err := os.WriteFile(filepath.Join(home, "settings.json"), []byte(settingsJSON), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GRF_HOME", home)
}

func testClient() *llm.Client {
	return llm.NewClient(llm.Config{BaseURL: "http://127.0.0.1:1", APIKey: "t", Model: "m"})
}

func TestPolicyOverrides(t *testing.T) {
	isolateConfig(t, `{"sandbox":"off","fs_policy":"off","auto_approve":false}`)
	yes := true
	a, err := New(Options{Workspace: t.TempDir(), Client: testClient(),
		Sandbox: "workspace-write", FsPolicy: "observe", AutoApprove: &yes})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	s := a.Harness.Settings
	if s.Sandbox != "workspace-write" || s.FsPolicy != "observe" || !s.AutoApprove {
		t.Fatalf("overrides not applied: %+v", s)
	}
}

func TestPolicyOverridesNilFollowsConfig(t *testing.T) {
	isolateConfig(t, `{"sandbox":"off","fs_policy":"off"}`)
	a, err := New(Options{Workspace: t.TempDir(), Client: testClient()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	s := a.Harness.Settings
	if s.Sandbox != "off" || s.FsPolicy != "off" {
		t.Fatalf("nil overrides should follow config file: %+v", s)
	}
}

func TestSystemPromptFallback(t *testing.T) {
	isolateConfig(t, `{"system_prompt":"配置文件提示词XYZ"}`)

	// Options 为空 → 用配置文件的 system_prompt（gh config prompt set 生效）
	a, err := New(Options{Workspace: t.TempDir(), Client: testClient()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if !strings.Contains(a.Root().SystemText(), "配置文件提示词XYZ") {
		t.Fatal("settings system_prompt not applied")
	}

	// Options 显式给出 → 覆盖配置文件
	a2, err := New(Options{Workspace: t.TempDir(), Client: testClient(), SystemPrompt: "选项提示词ABC"})
	if err != nil {
		t.Fatal(err)
	}
	defer a2.Close()
	txt := a2.Root().SystemText()
	if !strings.Contains(txt, "选项提示词ABC") || strings.Contains(txt, "配置文件提示词XYZ") {
		t.Fatal("Options.SystemPrompt should win over settings")
	}
}

func TestVerboseEventsWired(t *testing.T) {
	isolateConfig(t, "")
	var kinds []string
	a, err := New(Options{Workspace: t.TempDir(), Client: testClient(),
		VerboseEvents: func(kind, detail string) { kinds = append(kinds, kind) }})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.Root().SetPlanMode(true) // 触发一次 plan 事件
	found := false
	for _, k := range kinds {
		if k == "plan" {
			found = true
		}
	}
	if !found {
		t.Fatalf("VerboseEvents not wired, got %v", kinds)
	}
}
