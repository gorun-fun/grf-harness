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
	isolateConfig(t, `{"policy":"off","auto_approve":false}`)
	yes := true
	a, err := New(Options{Workspace: t.TempDir(), Client: testClient(),
		Policy: "write-workspace", AutoApprove: &yes})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	s := a.Harness.Settings
	if s.Policy != "write-workspace" || !s.AutoApprove {
		t.Fatalf("overrides not applied: %+v", s)
	}
	if a.Harness.Fs.ReadLimit {
		t.Fatal("explicit write-workspace should turn read boundary off")
	}
}

// TestPolicyCombinations 统一规则串的接线（含非法值回退 write-workspace）。
func TestPolicyCombinations(t *testing.T) {
	isolateConfig(t, "") // 无配置 → 默认 off
	ws := t.TempDir()

	// 默认 off：全关
	a, err := New(Options{Workspace: ws, Client: testClient()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.Harness.Fs.Observe || a.Harness.Fs.ReadLimit {
		t.Fatalf("default should be off, got %+v", a.Harness.Fs)
	}

	// 只开读边界
	a2, err := New(Options{Workspace: ws, Client: testClient(), Policy: "read-workspace"})
	if err != nil {
		t.Fatal(err)
	}
	defer a2.Close()
	if a2.Harness.Fs.Observe || !a2.Harness.Fs.ReadLimit {
		t.Fatalf("read-workspace only: %+v", a2.Harness.Fs)
	}

	// off 全关
	a3, err := New(Options{Workspace: ws, Client: testClient(), Policy: "off"})
	if err != nil {
		t.Fatal(err)
	}
	defer a3.Close()
	if a3.Harness.Fs.Observe || a3.Harness.Fs.ReadLimit {
		t.Fatalf("off should disable both: %+v", a3.Harness.Fs)
	}

	// 非法值 → 归一为 write-workspace（保写侧回退）
	a4, err := New(Options{Workspace: ws, Client: testClient(), Policy: "garbage"})
	if err != nil {
		t.Fatal(err)
	}
	defer a4.Close()
	if a4.Harness.Settings.Policy != "write-workspace" {
		t.Fatalf("invalid policy should normalize to write-workspace, got %q", a4.Harness.Settings.Policy)
	}
}

func TestPolicyOverridesNilFollowsConfig(t *testing.T) {
	isolateConfig(t, `{"policy":"off"}`)
	a, err := New(Options{Workspace: t.TempDir(), Client: testClient()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	s := a.Harness.Settings
	if s.Policy != "off" {
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
