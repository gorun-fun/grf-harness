// memory_test.go memory 工具行为测试：JSON 条目库 list/find/add/update/
// delete、自选唯一 key、find 三级检索（key 精确→key 子串→content 子串）、
// 两级作用域、默认 project、损坏文件报错与 4KB 上限。
package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func newMemoryHarness(t *testing.T) (*Registry, MemoryDeps) {
	t.Helper()
	dir := t.TempDir()
	deps := MemoryDeps{
		ProjectPath: filepath.Join(dir, "ws", ".grf", "memory.json"),
		UserPath:    filepath.Join(dir, "home", "memory.json"),
	}
	r := NewRegistry()
	RegisterMemory(r, deps)
	return r, deps
}

// readEntries 解析库文件内容。
func readEntries(t *testing.T, path string) []memoryEntry {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var entries []memoryEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("invalid store %s: %v", path, err)
	}
	return entries
}

func TestMemoryLifecycle(t *testing.T) {
	r, deps := newMemoryHarness(t)
	ctx := context.Background()

	// 初始两级皆空
	res := r.Execute(ctx, "memory", mustJSON(t, map[string]any{"action": "list"}))
	if res.Text != "(both memory scopes are empty)" {
		t.Fatalf("empty list: %s", res.Text)
	}

	// add 缺省作用域 = project（终端交互的记忆主体是工作区），key 自选
	res = r.Execute(ctx, "memory", mustJSON(t, map[string]any{"action": "add", "key": "构建命令", "content": "构建用 ./build.sh"}))
	if !strings.Contains(res.Text, "added [构建命令] to project memory") || res.Err != "" {
		t.Fatalf("add: %s (err=%q)", res.Text, res.Err)
	}
	entries := readEntries(t, deps.ProjectPath)
	if len(entries) != 1 || entries[0].Key != "构建命令" || entries[0].Content != "构建用 ./build.sh" || entries[0].Updated == "" {
		t.Fatalf("store after add: %+v", entries)
	}
	r.Execute(ctx, "memory", mustJSON(t, map[string]any{"action": "add", "key": "依赖约束", "content": "主模块零第三方依赖"}))

	// key 重复：拒绝并引导 update
	res = r.Execute(ctx, "memory", mustJSON(t, map[string]any{"action": "add", "key": "构建命令", "content": "另一条"}))
	if res.Err == "" || !strings.Contains(res.Text, "already exists") {
		t.Fatalf("duplicate key should fail: %s", res.Text)
	}

	// add 到 user 作用域落到另一文件；key 空间按作用域独立
	res = r.Execute(ctx, "memory", mustJSON(t, map[string]any{"action": "add", "scope": "user", "key": "构建命令", "content": "用户级同 key 不冲突"}))
	if !strings.Contains(res.Text, "added [构建命令] to user memory") || res.Err != "" {
		t.Fatalf("add user: %s (err=%q)", res.Text, res.Err)
	}
	if _, err := os.Stat(deps.UserPath); err != nil {
		t.Fatal(err)
	}

	// list 不带 scope：两级小节都在，条目带 key 前缀展示
	res = r.Execute(ctx, "memory", mustJSON(t, map[string]any{"action": "list"}))
	for _, want := range []string{"## project memory", "## user memory", "[构建命令]", "构建用 ./build.sh"} {
		if !strings.Contains(res.Text, want) {
			t.Fatalf("list both missing %q: %s", want, res.Text)
		}
	}

	// list 指定 scope：只回该级
	res = r.Execute(ctx, "memory", mustJSON(t, map[string]any{"action": "list", "scope": "user"}))
	if !strings.Contains(res.Text, "## user memory") || strings.Contains(res.Text, "## project memory") {
		t.Fatalf("list single scope: %s", res.Text)
	}

	// update 按 key 整条重写 content（key 不变）
	res = r.Execute(ctx, "memory", mustJSON(t, map[string]any{"action": "update", "key": "构建命令", "content": "构建统一走 ./build.sh（vet+build）"}))
	if !strings.Contains(res.Text, "updated [构建命令] in project memory") || res.Err != "" {
		t.Fatalf("update: %s (err=%q)", res.Text, res.Err)
	}
	entries = readEntries(t, deps.ProjectPath)
	if len(entries) != 2 || entries[0].Key != "构建命令" || entries[0].Content != "构建统一走 ./build.sh（vet+build）" {
		t.Fatalf("store after update: %+v", entries)
	}

	// delete 按 key 删除；删到 0 条存空数组，list 回 (empty)
	res = r.Execute(ctx, "memory", mustJSON(t, map[string]any{"action": "delete", "key": "依赖约束"}))
	if !strings.Contains(res.Text, "deleted [依赖约束] from project memory (1 entries left)") || res.Err != "" {
		t.Fatalf("delete: %s (err=%q)", res.Text, res.Err)
	}
	res = r.Execute(ctx, "memory", mustJSON(t, map[string]any{"action": "delete", "key": "构建命令"}))
	if !strings.Contains(res.Text, "0 entries left") {
		t.Fatalf("delete last: %s", res.Text)
	}
	if entries := readEntries(t, deps.ProjectPath); len(entries) != 0 {
		t.Fatalf("store should be empty: %+v", entries)
	}
	res = r.Execute(ctx, "memory", mustJSON(t, map[string]any{"action": "list", "scope": "project"}))
	if !strings.Contains(res.Text, "(empty)") {
		t.Fatalf("list empty store: %s", res.Text)
	}
}

func TestMemoryFind(t *testing.T) {
	r, _ := newMemoryHarness(t)
	ctx := context.Background()
	add := func(key, content string) {
		if res := r.Execute(ctx, "memory", mustJSON(t, map[string]any{"action": "add", "key": key, "content": content})); res.Err != "" {
			t.Fatalf("add %s: %s", key, res.Text)
		}
	}
	add("build-cmd", "构建用 ./build.sh")
	add("build-cache", "构建产物缓存目录 dist/")
	add("code-style", "注释一律用中文")

	// key 精确命中（大小写不敏感）：只回该条
	res := r.Execute(ctx, "memory", mustJSON(t, map[string]any{"action": "find", "key": "BUILD-CMD"}))
	if !strings.Contains(res.Text, "build-cmd") || strings.Contains(res.Text, "build-cache") || strings.Contains(res.Text, "code-style") {
		t.Fatalf("find exact: %s", res.Text)
	}

	// key 子串命中：build 同时命中 build-cmd 与 build-cache
	res = r.Execute(ctx, "memory", mustJSON(t, map[string]any{"action": "find", "key": "build"}))
	if !strings.Contains(res.Text, "build-cmd") || !strings.Contains(res.Text, "build-cache") || strings.Contains(res.Text, "code-style") {
		t.Fatalf("find key substring: %s", res.Text)
	}

	// content 子串命中：带 (matched in content) 标注
	res = r.Execute(ctx, "memory", mustJSON(t, map[string]any{"action": "find", "key": "./build.sh"}))
	if !strings.Contains(res.Text, "build-cmd") || !strings.Contains(res.Text, "(matched in content)") {
		t.Fatalf("find content substring: %s", res.Text)
	}

	// 无命中：提示文本，不算错误
	res = r.Execute(ctx, "memory", mustJSON(t, map[string]any{"action": "find", "key": "不存在的词"}))
	if res.Err != "" || !strings.Contains(res.Text, "no entries matching") {
		t.Fatalf("find miss: %s (err=%q)", res.Text, res.Err)
	}
}

func TestMemoryValidation(t *testing.T) {
	r, _ := newMemoryHarness(t)
	ctx := context.Background()
	r.Execute(ctx, "memory", mustJSON(t, map[string]any{"action": "add", "key": "seed", "content": "种子"}))

	cases := []struct {
		name, args string
		wantErr    string
	}{
		{"invalid action", mustJSON(t, map[string]any{"action": "dump"}), `invalid action "dump"`},
		{"missing action", mustJSON(t, map[string]any{"scope": "project"}), "action is required"},
		{"invalid scope", mustJSON(t, map[string]any{"action": "add", "scope": "global", "key": "k", "content": "x"}), `invalid scope "global"`},
		{"add without key", mustJSON(t, map[string]any{"action": "add", "content": "x"}), "key is required"},
		{"add blank key", mustJSON(t, map[string]any{"action": "add", "key": "  ", "content": "x"}), "key is required"},
		{"add multiline key", mustJSON(t, map[string]any{"action": "add", "key": "a\nb", "content": "x"}), "single line"},
		{"add long key", mustJSON(t, map[string]any{"action": "add", "key": strings.Repeat("k", 65), "content": "x"}), "key too long"},
		{"duplicate key", mustJSON(t, map[string]any{"action": "add", "key": "seed", "content": "x"}), "already exists"},
		{"empty add", mustJSON(t, map[string]any{"action": "add", "key": "k", "content": "  "}), "content is required for add"},
		{"empty update", mustJSON(t, map[string]any{"action": "update", "key": "seed"}), "content is required for update"},
		{"update without key", mustJSON(t, map[string]any{"action": "update", "content": "x"}), "key is required"},
		{"update unknown key", mustJSON(t, map[string]any{"action": "update", "key": "m9", "content": "x"}), `unknown key "m9"`},
		{"delete without key", mustJSON(t, map[string]any{"action": "delete"}), "key is required"},
		{"delete unknown key", mustJSON(t, map[string]any{"action": "delete", "key": "m9"}), `unknown key "m9"`},
		{"find without key", mustJSON(t, map[string]any{"action": "find"}), "key is required for find"},
	}
	for _, c := range cases {
		res := r.Execute(ctx, "memory", c.args)
		if res.Err == "" || !strings.Contains(res.Text, c.wantErr) {
			t.Fatalf("%s: got %q (err=%q), want %q", c.name, res.Text, res.Err, c.wantErr)
		}
	}
}

func TestMemoryCorruptedStore(t *testing.T) {
	r, deps := newMemoryHarness(t)
	os.MkdirAll(filepath.Dir(deps.ProjectPath), 0o755)
	os.WriteFile(deps.ProjectPath, []byte("not json"), 0o644)
	ctx := context.Background()
	res := r.Execute(ctx, "memory", mustJSON(t, map[string]any{"action": "list", "scope": "project"}))
	if res.Err == "" || !strings.Contains(res.Text, "corrupted") {
		t.Fatalf("corrupted list should fail: %s", res.Text)
	}
	res = r.Execute(ctx, "memory", mustJSON(t, map[string]any{"action": "add", "key": "k", "content": "x"}))
	if res.Err == "" || !strings.Contains(res.Text, "corrupted") {
		t.Fatalf("corrupted add should fail: %s", res.Text)
	}
}

func TestMemorySizeCap(t *testing.T) {
	r, deps := newMemoryHarness(t)
	ctx := context.Background()

	// 反复 add 较长条目，最终应触顶被拒且文件不超过 4KB
	capped := false
	for i := 0; i < 40 && !capped; i++ {
		res := r.Execute(ctx, "memory", mustJSON(t, map[string]any{
			"action": "add", "key": "k" + strconv.Itoa(i), "content": strings.Repeat("x", 300),
		}))
		if res.Err != "" {
			if !strings.Contains(res.Text, "cap") {
				t.Fatalf("unexpected error: %s", res.Text)
			}
			capped = true
		}
	}
	if !capped {
		t.Fatal("40 x 300-byte entries should trip the 4KB cap")
	}
	data, _ := os.ReadFile(deps.ProjectPath)
	if len(data) > memoryMaxBytes {
		t.Fatalf("file should never exceed cap: %d bytes", len(data))
	}

	// 触顶后 list 截断展示（手工膨胀到超限）
	big := make([]memoryEntry, 0, 60)
	for i := 1; i <= 60; i++ {
		big = append(big, memoryEntry{Key: "k" + strconv.Itoa(i), Content: strings.Repeat("y", 200), Updated: "2026-09-10"})
	}
	os.WriteFile(deps.ProjectPath, mustJSONBytes(t, big), 0o644)
	res := r.Execute(ctx, "memory", mustJSON(t, map[string]any{"action": "list", "scope": "project"}))
	if !strings.Contains(res.Text, "[truncated:") {
		t.Fatalf("truncated list: %.200s", res.Text)
	}
}

func mustJSONBytes(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return data
}
