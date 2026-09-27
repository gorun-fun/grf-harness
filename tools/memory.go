// memory.go memory 工具：跨会话持久记忆（JSON 条目库，可增删改查）。
// 两级作用域：project（默认，终端交互的记忆主体）= 工作区
// .grf/memory.json，user = 用户主目录下的记忆文件（agent 装配期经
// config.Home() 求出）。条目以调用方选定的 key 标识（作用域内唯一、
// 可用于检索）；动作：list 全量列出（不带 scope 两级都列）、find 检索
// （key 精确命中优先，其次 key 子串、再 content 子串，均大小写不敏感）、
// add 新增（key 不可重复，重复走 update）、update 按 key 重写 content、
// delete 按 key 删除。单文件 4KB 硬上限（超限拒绝写入，引导删改精简）；
// 根会话/子代理/多会话共享同一文件，读改写经进程级互斥锁串行。
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"git.gorun.fun/share/grf-harness/config"
)

// MemoryFile 两级作用域共用的记忆文件名（JSON 条目数组）。
const MemoryFile = "memory.json"

// memoryMaxBytes 单个记忆文件的硬上限（超出即拒绝写入，引导删改精简）。
const memoryMaxBytes = 4 << 10

// memoryKeyMaxRunes key 的长度上限（rune 计）。
const memoryKeyMaxRunes = 64

// memoryMu 记忆文件读改写的进程级互斥锁。
var memoryMu sync.Mutex

// MemoryDeps memory 工具的两级落点路径（agent 装配期给出绝对路径）。
type MemoryDeps struct {
	ProjectPath string // 工作区 .grf/memory.json
	UserPath    string // ~/.grf/memory.json（config.Home() 下）
}

// memoryEntry 一条记忆：key 由调用方选定（作用域内唯一），updated 记
// 最后写入日期。
type memoryEntry struct {
	Key     string `json:"key"`
	Content string `json:"content"`
	Updated string `json:"updated,omitempty"`
}

// path 按作用域取落点路径。
func (d MemoryDeps) path(scope string) (string, error) {
	switch scope {
	case "project":
		return d.ProjectPath, nil
	case "user":
		return d.UserPath, nil
	}
	return "", fmt.Errorf("invalid scope %q (use project|user)", scope)
}

// RegisterMemory 注册 memory 工具。
func RegisterMemory(r *Registry, deps MemoryDeps) {
	r.Register("memory", func(ctx context.Context, args string) (Result, error) {
		var a struct {
			Action  string `json:"action"`
			Scope   string `json:"scope"`
			Key     string `json:"key"`
			Content string `json:"content"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return Result{}, err
		}
		if a.Action == "" {
			return Result{}, errors.New("action is required (list|find|add|update|delete)")
		}
		// 写类与 find 动作缺省 project（终端交互的记忆主体是工作区）；
		// list 不带 scope = 两级都读，由 listMemory 按原始值区分
		scope := a.Scope
		if scope == "" {
			scope = "project"
		}
		memoryMu.Lock()
		defer memoryMu.Unlock()
		switch a.Action {
		case "list":
			return listMemory(deps, a.Scope)
		case "find":
			term := strings.TrimSpace(a.Key)
			if term == "" {
				return Result{}, errors.New("key is required for find (a search term: exact key first, then substrings of keys or contents)")
			}
			path, err := deps.path(scope)
			if err != nil {
				return Result{}, err
			}
			entries, err := loadMemory(path, scope)
			if err != nil {
				return Result{}, err
			}
			return findMemory(entries, scope, path, term)
		case "add":
			key, err := validMemoryKey(a.Key)
			if err != nil {
				return Result{}, err
			}
			content := strings.TrimSpace(a.Content)
			if content == "" {
				return Result{}, errors.New("content is required for add (one concise entry)")
			}
			path, err := deps.path(scope)
			if err != nil {
				return Result{}, err
			}
			entries, err := loadMemory(path, scope)
			if err != nil {
				return Result{}, err
			}
			if indexOfKey(entries, key) >= 0 {
				return Result{}, fmt.Errorf("key %q already exists in %s memory; use update to rewrite it", key, scope)
			}
			entries = append(entries, memoryEntry{Key: key, Content: content, Updated: memoryDate()})
			n, err := saveMemory(path, scope, entries)
			if err != nil {
				return Result{}, err
			}
			return TextResult(fmt.Sprintf("added [%s] to %s memory (%d entries, %d bytes)", key, scope, len(entries), n)), nil
		case "update":
			if strings.TrimSpace(a.Content) == "" {
				return Result{}, errors.New("content is required for update")
			}
			key, err := validMemoryKey(a.Key)
			if err != nil {
				return Result{}, err
			}
			path, err := deps.path(scope)
			if err != nil {
				return Result{}, err
			}
			entries, err := loadMemory(path, scope)
			if err != nil {
				return Result{}, err
			}
			i := indexOfKey(entries, key)
			if i < 0 {
				return Result{}, fmt.Errorf("unknown key %q in %s memory; list it first", key, scope)
			}
			entries[i].Content = strings.TrimSpace(a.Content)
			entries[i].Updated = memoryDate()
			n, err := saveMemory(path, scope, entries)
			if err != nil {
				return Result{}, err
			}
			return TextResult(fmt.Sprintf("updated [%s] in %s memory (%d entries, %d bytes)", key, scope, len(entries), n)), nil
		case "delete":
			key, err := validMemoryKey(a.Key)
			if err != nil {
				return Result{}, err
			}
			path, err := deps.path(scope)
			if err != nil {
				return Result{}, err
			}
			entries, err := loadMemory(path, scope)
			if err != nil {
				return Result{}, err
			}
			i := indexOfKey(entries, key)
			if i < 0 {
				return Result{}, fmt.Errorf("unknown key %q in %s memory; list it first", key, scope)
			}
			entries = append(entries[:i], entries[i+1:]...)
			if _, err := saveMemory(path, scope, entries); err != nil {
				return Result{}, err
			}
			return TextResult(fmt.Sprintf("deleted [%s] from %s memory (%d entries left)", key, scope, len(entries))), nil
		}
		return Result{}, fmt.Errorf("invalid action %q (use list|find|add|update|delete)", a.Action)
	})
}

// memSection list 输出的一个小节（作用域 + 落点）。
type memSection struct {
	label string
	path  string
}

// listMemory 列出记忆：rawScope 为空时两级都读（各带小节标题与条目数），
// 否则只读该级。手工膨胀超上限的库截取前 20 条展示并注明。
func listMemory(deps MemoryDeps, rawScope string) (Result, error) {
	var sections []memSection
	if rawScope == "" {
		sections = []memSection{{"project", deps.ProjectPath}, {"user", deps.UserPath}}
	} else {
		path, err := deps.path(rawScope)
		if err != nil {
			return Result{}, err
		}
		sections = []memSection{{rawScope, path}}
	}
	var b strings.Builder
	empty := true
	for _, s := range sections {
		entries, err := loadMemory(s.path, s.label)
		if err != nil {
			return Result{}, err
		}
		if len(entries) == 0 {
			fmt.Fprintf(&b, "## %s memory (%s)\n(empty)\n\n", s.label, s.path)
			continue
		}
		empty = false
		fmt.Fprintf(&b, "## %s memory (%s, %d entries)\n", s.label, s.path, len(entries))
		shown := entries
		note := ""
		if data, err := json.Marshal(entries); err == nil && len(data) > memoryMaxBytes {
			const maxShown = 20
			if len(entries) > maxShown {
				shown = entries[:maxShown]
			}
			note = fmt.Sprintf("\n[truncated: store exceeds the %d-byte cap, showing first %d of %d entries — update or delete to slim it]",
				memoryMaxBytes, len(shown), len(entries))
		}
		for _, e := range shown {
			writeMemoryEntry(&b, e)
		}
		b.WriteString(note + "\n\n")
	}
	if empty && rawScope == "" {
		return TextResult("(both memory scopes are empty)"), nil
	}
	return TextResult(strings.TrimRight(b.String(), "\n")), nil
}

// findMemory 检索：key 精确命中（大小写不敏感）优先，其次 key 含检索词、
// 再 content 含检索词；无命中回提示文本（检索落空不是错误）。
func findMemory(entries []memoryEntry, scope, path, term string) (Result, error) {
	lower := strings.ToLower(term)
	var exact, inKey, inContent []memoryEntry
	for _, e := range entries {
		k := strings.ToLower(e.Key)
		switch {
		case k == lower:
			exact = append(exact, e)
		case strings.Contains(k, lower):
			inKey = append(inKey, e)
		case strings.Contains(strings.ToLower(e.Content), lower):
			inContent = append(inContent, e)
		}
	}
	matches := len(exact) + len(inKey) + len(inContent)
	if matches == 0 {
		return TextResult(fmt.Sprintf("no entries matching %q in %s memory (%s)", term, scope, path)), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## %s memory (%s, %d/%d entries match %q)\n", scope, path, matches, len(entries), term)
	for _, e := range exact {
		writeMemoryEntry(&b, e)
	}
	for _, e := range inKey {
		writeMemoryEntry(&b, e)
	}
	for _, e := range inContent {
		fmt.Fprintf(&b, "- [%s] %s: %s (matched in content)\n", e.Key, e.Updated, e.Content)
	}
	return TextResult(strings.TrimRight(b.String(), "\n")), nil
}

// writeMemoryEntry 输出一条条目（列表与 find 共用的行格式）。
func writeMemoryEntry(b *strings.Builder, e memoryEntry) {
	fmt.Fprintf(b, "- [%s] %s: %s\n", e.Key, e.Updated, e.Content)
}

// loadMemory 读库：文件不存在或为空返回 nil；JSON 损坏报错（不静默清空）。
func loadMemory(path, scope string) ([]memoryEntry, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s memory: %w", scope, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, nil
	}
	var entries []memoryEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("%s memory file is corrupted (invalid JSON): %v — ask the user to fix or remove the file", scope, err)
	}
	return entries, nil
}

// saveMemory 落盘（缩进 JSON + 尾换行），返回写入字节数；超上限拒绝。
func saveMemory(path, scope string, entries []memoryEntry) (int, error) {
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("encode %s memory: %w", scope, err)
	}
	data = append(data, '\n')
	if len(data) > memoryMaxBytes {
		return 0, fmt.Errorf("%s memory would exceed the %d-byte cap (%d bytes, %d entries); view it, then update or delete stale entries instead",
			scope, memoryMaxBytes, len(data), len(entries))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, fmt.Errorf("create memory dir: %w", err)
	}
	if err := config.WriteFileAtomic(path, data, 0o644); err != nil {
		return 0, fmt.Errorf("write %s memory: %w", scope, err)
	}
	return len(data), nil
}

// validMemoryKey 校验并归一 key：去空白、非空、单行、≤64 个 rune。
func validMemoryKey(raw string) (string, error) {
	key := strings.TrimSpace(raw)
	switch {
	case key == "":
		return "", errors.New("key is required (a short unique topic string)")
	case strings.ContainsAny(key, "\n\r"):
		return "", errors.New("key must be a single line")
	case utf8.RuneCountInString(key) > memoryKeyMaxRunes:
		return "", fmt.Errorf("key too long (max %d characters)", memoryKeyMaxRunes)
	}
	return key, nil
}

// indexOfKey 按 key 精确定位条目下标（未找到 -1）。
func indexOfKey(entries []memoryEntry, key string) int {
	for i, e := range entries {
		if e.Key == key {
			return i
		}
	}
	return -1
}

// memoryDate 条目的最后写入日期（本地时区，日期粒度足够判断新鲜度）。
func memoryDate() string { return time.Now().Format("2006-01-02") }
