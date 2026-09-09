// plugin_test.go 插件管理器端到端测试：桩插件用测试二进制自复执行
// （TestStubPlugin 模式），覆盖配置接入、目录自动发现、重名拦截与
// 注册进 tools.Registry 后的真实执行链路。
package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/tools"
)

const stubEnv = "GRF_PLUGIN_TEST_STUB"

// stubExe 当前测试二进制路径（发现测试的脚本桩里引用）。
func stubExe(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("桩插件依赖 sh 脚本，不适用于 windows")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// stubCommand 测试二进制自复执行的桩插件命令行（env 前缀注入触发变量）。
func stubCommand(t *testing.T) []string {
	t.Helper()
	return []string{"/usr/bin/env", stubEnv + "=1", stubExe(t), "-test.run=TestStubPlugin", "--"}
}

func setupHome(t *testing.T, settingsJSON string) {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	if settingsJSON != "" {
		if err := os.WriteFile(filepath.Join(home, "settings.json"), []byte(settingsJSON), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GRF_HOME", home)
}

func TestLoadConfiguredAndExecute(t *testing.T) {
	cmd := stubCommand(t)
	b, _ := json.Marshal(map[string]any{"plugins": []map[string]any{{"name": "stub", "command": cmd}}})
	setupHome(t, string(b))

	st, err := config.LoadFileForUpdate()
	if err != nil {
		t.Fatal(err)
	}
	m := New()
	defer m.Close()
	m.Load(st)

	// 桩插件提供 stub_echo / stub_get / bash（bash 应被内置重名拦截）
	names := m.ToolNames()
	if len(names) != 2 || names[0] != "stub_echo" || names[1] != "stub_get" {
		t.Fatalf("工具清单 = %v，want [stub_echo stub_get]（bash 应被跳过）\n警告: %v\n连接: %+v",
			names, m.Warnings(), m.Conns())
	}
	if w := strings.Join(m.Warnings(), "\n"); !strings.Contains(w, "重名") {
		t.Fatalf("应包含重名警告，got %q", w)
	}

	reg := tools.NewRegistry()
	blocked := m.RegisterInto(reg)
	if !blocked["stub_echo"] || blocked["stub_get"] {
		t.Fatalf("计划模式拦截集不符: %v（非只读应拦截，只读放行）", blocked)
	}
	res := reg.Execute(context.Background(), "stub_echo", `{"msg":"hey"}`)
	if res.Err != "" || res.Text != "stub: hey" {
		t.Fatalf("执行 stub_echo = %+v", res)
	}
	if imgs := reg.Execute(context.Background(), "stub_get", `{}`).Images; len(imgs) != 1 || imgs[0].Mime != "image/png" {
		t.Fatalf("stub_get 应返回图像内容: %+v", imgs)
	}
}

func TestAutoDiscovery(t *testing.T) {
	exe := stubExe(t)
	setupHome(t, "") // plugins 键缺省 → 自动发现
	script := "#!/bin/sh\n" + stubEnv + "=1 exec " + scriptQuote(exe) + " -test.run=TestStubPlugin --\n"
	p := filepath.Join(PluginDir(), "grf-stub")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" { // Windows 无执行位/脚本桩，跳过发现测试
		t.Skip("stdio 脚本桩不适用于 windows")
	}

	st, err := config.LoadFileForUpdate()
	if err != nil {
		t.Fatal(err)
	}
	m := New()
	defer m.Close()
	m.Load(st)
	if names := m.ToolNames(); len(names) != 2 {
		t.Fatalf("自动发现未接入桩插件: %v\n警告: %v\n发现项: %+v", names, m.Warnings(), discoverPlugins())
	}
	if c := m.Conns(); len(c) != 1 || c[0].Name != "grf-stub" || c[0].Mode != "stdio" {
		t.Fatalf("连接信息不符: %+v", c)
	}
}

func scriptQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// TestStubPlugin 以桩 MCP stdio 服务运行（经 -test.run 复执行触发）。
func TestStubPlugin(t *testing.T) {
	if os.Getenv(stubEnv) != "1" {
		return
	}
	runStub(os.Stdin, os.Stdout)
	os.Exit(0)
}

func runStub(r interface{ Read([]byte) (int, error) }, w interface{ Write([]byte) (int, error) }) {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := r.Read(tmp)
		buf = append(buf, tmp[:n]...)
		for {
			i := strings.IndexByte(string(buf), '\n')
			if i < 0 {
				break
			}
			line := append([]byte(nil), buf[:i]...)
			buf = append([]byte(nil), buf[i+1:]...)
			if len(line) > 0 {
				if resp := stubHandle(line); resp != nil {
					fmt.Fprintln(w, string(resp))
				}
			}
		}
		if err != nil {
			return
		}
	}
}

func stubHandle(line []byte) []byte {
	var req struct {
		ID     *int64          `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(line, &req); err != nil || req.ID == nil {
		return nil
	}
	resp := func(result any) []byte {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
		return b
	}
	switch req.Method {
	case "initialize":
		return resp(map[string]any{"protocolVersion": "2025-06-18",
			"serverInfo": map[string]any{"name": "stub-plugin", "version": "0.1.0"}})
	case "tools/list":
		return resp(map[string]any{"tools": []map[string]any{
			{"name": "stub_echo", "description": "回显", "inputSchema": map[string]any{"type": "object"}},
			{"name": "stub_get", "description": "只读取图", "inputSchema": map[string]any{"type": "object"},
				"annotations": map[string]any{"readOnlyHint": true}},
			{"name": "bash", "description": "与内置重名", "inputSchema": map[string]any{"type": "object"}},
		}})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		_ = json.Unmarshal(req.Params, &p)
		switch p.Name {
		case "stub_echo":
			var a struct {
				Msg string `json:"msg"`
			}
			_ = json.Unmarshal(p.Arguments, &a)
			return resp(map[string]any{"content": []map[string]any{{"type": "text", "text": "stub: " + a.Msg}}})
		case "stub_get":
			return resp(map[string]any{"content": []map[string]any{
				{"type": "image", "data": "aGk=", "mimeType": "image/png"},
			}})
		}
	}
	return nil
}
