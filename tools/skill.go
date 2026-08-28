// skill.go skill 工具与技能目录：workspace/.grf/skills 与 ~/.grf/skills
// 下的 <name>/SKILL.md（工作区优先）。
package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SkillCatalog 技能目录：workspace/.grf/skills 与 ~/.grf/skills 下的
// <name>/SKILL.md。
type SkillCatalog struct {
	entries map[string]string // name -> SKILL.md 全文
	order   []string
}

// LoadSkills 加载两级技能目录（工作区优先）。
func LoadSkills(workspace string) *SkillCatalog {
	c := &SkillCatalog{entries: map[string]string{}}
	home := userGrfHome()
	dirs := []string{filepath.Join(home, "skills"), filepath.Join(workspace, ".grf", "skills")}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			path := filepath.Join(dir, e.Name(), "SKILL.md")
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			name := e.Name()
			if _, ok := c.entries[name]; !ok {
				c.order = append(c.order, name)
			}
			c.entries[name] = string(data)
		}
	}
	sort.Strings(c.order)
	return c
}

// Names 可用技能名。
func (c *SkillCatalog) Names() []string { return append([]string(nil), c.order...) }

// Load 取技能全文。
func (c *SkillCatalog) Load(name string) (string, bool) {
	s, ok := c.entries[name]
	return s, ok
}

// Summary 首行摘要（system prompt 列表用）。
func (c *SkillCatalog) Summary(name string) string {
	s, _ := c.entries[name]
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "#")); t != "" {
			return clipRunes(t, 80)
		}
	}
	return ""
}

// RegisterSkill 注册 skill 工具。
func RegisterSkill(r *Registry, catalog *SkillCatalog) {
	r.Register("skill", func(ctx context.Context, args string) (Result, error) {
		var a struct {
			Name string `json:"name"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return Result{}, err
		}
		if catalog == nil || len(catalog.order) == 0 {
			return TextResult("no skills are available in this session"), nil
		}
		if a.Name == "" {
			return Result{}, fmt.Errorf("name is required; available skills: %v", catalog.Names())
		}
		content, ok := catalog.Load(a.Name)
		if !ok {
			return Result{}, fmt.Errorf("unknown skill %q; available skills: %v", a.Name, catalog.Names())
		}
		return TextResult(fmt.Sprintf("skill %q loaded; follow its instructions for the task at hand:\n\n%s", a.Name, content)), nil
	})
}

// userGrfHome 用户级配置目录（GRF_HOME 覆盖 ~/.grf）。
func userGrfHome() string {
	if v := os.Getenv("GRF_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".grf"
	}
	return filepath.Join(home, ".grf")
}
