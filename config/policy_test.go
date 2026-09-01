// policy_test.go 统一规则串 ParsePolicy/NormalizePolicy 解析测试。
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParsePolicy(t *testing.T) {
	cases := []struct {
		in         string
		writeLimit bool
		readLimit  bool
	}{
		{"", true, true},                                // 空值安全默认全开
		{"off", false, false},                           // 全关
		{"write-workspace", true, false},                // 只开写侧
		{"read-workspace", false, true},                 // 只开读侧
		{"write-workspace|read-workspace", true, true},  // 全开
		{"read-workspace | write-workspace", true, true}, // 顺序与空格无关
		{"garbage", true, false},                        // 非法 → 回退 write-workspace
		{"write-workspace|garbage", true, false},        // 含合法 token 按写的来
		{"off|read-workspace", false, true},             // off 混写：合法 token 生效
	}
	for _, c := range cases {
		w, r := ParsePolicy(c.in)
		if w != c.writeLimit || r != c.readLimit {
			t.Fatalf("ParsePolicy(%q) = (%v,%v), want (%v,%v)", c.in, w, r, c.writeLimit, c.readLimit)
		}
	}
}

func TestNormalizePolicy(t *testing.T) {
	cases := map[string]string{
		"":                               "write-workspace|read-workspace",
		"write-workspace|read-workspace": "write-workspace|read-workspace",
		"write-workspace":                "write-workspace",
		"read-workspace":                 "read-workspace",
		"off":                            "off",
		"garbage":                        "write-workspace",               // 非法 → 回退写侧
		" write-workspace ":              "write-workspace",
	}
	for in, want := range cases {
		if got := NormalizePolicy(in); got != want {
			t.Fatalf("NormalizePolicy(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestLoadFilePolicyRewritten 加载时解析出错的 policy 被回退并回写文件。
func TestLoadFilePolicyRewritten(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GRF_HOME", home)
	path := filepath.Join(home, "settings.json")
	os.WriteFile(path, []byte(`{"policy":"garbage"}`), 0o600)

	s, err := LoadFileForUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if s.Policy != "write-workspace" {
		t.Fatalf("invalid policy should fall back to write-workspace, got %q", s.Policy)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"write-workspace"`) || strings.Contains(string(data), "garbage") {
		t.Fatalf("settings.json should be rewritten, got: %s", data)
	}
}
