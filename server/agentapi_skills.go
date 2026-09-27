// agentapi_skills.go — /v1/agent/skills 域:GRF_HOME/skills/<name>/SKILL.md
// 的列表/读/写/删与启用禁用(改名 SKILL.md.disabled)。
package server

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func skillsDir() string {
	return filepath.Join(configHome(), "skills")
}

// handleAgentSkillsGet GET /v1/agent/skills。
func (s *Server) handleAgentSkillsGet(w http.ResponseWriter, r *http.Request) {
	dir := skillsDir()
	out := skillsDTO{Path: dir, Skills: []skillDTO{}, Writable: settingsWritable(dir)}
	ents, err := os.ReadDir(dir)
	if err != nil {
		writeJSON(w, 200, out) // 目录不存在 = 空列表
		return
	}
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		md := filepath.Join(dir, e.Name(), "SKILL.md")
		disabled := false
		if _, err := os.Stat(md); err != nil {
			if _, err2 := os.Stat(filepath.Join(dir, e.Name(), "SKILL.md.disabled")); err2 != nil {
				continue
			}
			md = filepath.Join(dir, e.Name(), "SKILL.md.disabled")
			disabled = true
		}
		raw, err := os.ReadFile(md)
		if err != nil {
			continue
		}
		out.Skills = append(out.Skills, skillDTO{
			Name:        e.Name(),
			Description: skillDescription(string(raw)),
			Path:        md,
			Disabled:    disabled,
		})
	}
	sort.Slice(out.Skills, func(i, j int) bool { return out.Skills[i].Name < out.Skills[j].Name })
	writeJSON(w, 200, out)
}

// handleAgentSkillGet GET /v1/agent/skills/{name}:技能全文。
func (s *Server) handleAgentSkillGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := validSkillName(name); err != nil {
		writeError(w, 400, errInvalid, err.Error(), "name")
		return
	}
	raw, err := os.ReadFile(filepath.Join(skillsDir(), name, "SKILL.md"))
	if err != nil {
		if os.IsNotExist(err) {
			// 已禁用的技能也可读(编辑不必先启用)
			raw, err = os.ReadFile(filepath.Join(skillsDir(), name, "SKILL.md.disabled"))
		}
		if err != nil {
			writeError(w, 404, errNotFound, "技能不存在: "+name, "")
			return
		}
	}
	writeJSON(w, 200, map[string]string{"content": string(raw)})
}

// handleAgentSkillPut PUT /v1/agent/skills/{name}:新建/覆盖 SKILL.md。
func (s *Server) handleAgentSkillPut(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := validSkillName(name); err != nil {
		writeError(w, 400, errInvalid, err.Error(), "name")
		return
	}
	var in struct {
		Content string `json:"content"`
	}
	if !decodeJSON(w, r, &in, bodyLimitSmall) {
		return
	}
	dir := filepath.Join(skillsDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeError(w, 500, errServer, err.Error(), "")
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(in.Content), 0o600); err != nil {
		writeError(w, 500, errServer, err.Error(), "")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleAgentSkillDelete DELETE /v1/agent/skills/{name}。
func (s *Server) handleAgentSkillDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := validSkillName(name); err != nil {
		writeError(w, 400, errInvalid, err.Error(), "name")
		return
	}
	dir := filepath.Join(skillsDir(), name)
	if _, err := os.Stat(dir); err != nil {
		writeError(w, 404, errNotFound, "技能不存在: "+name, "")
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		writeError(w, 500, errServer, err.Error(), "")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleAgentSkillEnabled POST /v1/agent/skills/{name}/enabled {"enabled":bool}:
// gh 只认 SKILL.md,禁用即改名 SKILL.md.disabled(内容保留),启用改回。
// 启用/禁用两支互为镜像,折叠为 (from,to) 改名对统一处理。
func (s *Server) handleAgentSkillEnabled(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := validSkillName(name); err != nil {
		writeError(w, 400, errInvalid, err.Error(), "name")
		return
	}
	var in struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &in, bodyLimitSmall) {
		return
	}
	dir := filepath.Join(skillsDir(), name)
	on := filepath.Join(dir, "SKILL.md")
	off := filepath.Join(dir, "SKILL.md.disabled")
	from, to := off, on
	if !in.Enabled {
		from, to = on, off
	}
	if _, err := os.Stat(from); err != nil {
		if _, err2 := os.Stat(to); err2 == nil {
			writeJSON(w, 200, map[string]any{"ok": true}) // 已是目标状态
			return
		}
		writeError(w, 404, errNotFound, "技能不存在: "+name, "")
		return
	}
	if err := os.Rename(from, to); err != nil {
		writeError(w, 500, errServer, err.Error(), "")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// validSkillName 技能名即目录名:非空且不含路径分隔符/向上引用。
func validSkillName(name string) error {
	if name == "" || strings.ContainsRune(name, '/') || strings.ContainsRune(name, '\\') || name == "." || name == ".." {
		return fmt.Errorf("非法技能名: %s", name)
	}
	return nil
}

// skillDescription 提取技能描述:优先 YAML frontmatter 的 description:,
// 否则取正文首个非标题非空行(截断到 80 字)。
func skillDescription(md string) string {
	lines := strings.Split(md, "\n")
	inFront := false
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "---" && !inFront {
			inFront = true
			continue
		}
		if inFront {
			if t == "---" || t == "..." {
				inFront = false
			} else if v, ok := strings.CutPrefix(t, "description:"); ok {
				if s := strings.Trim(strings.TrimSpace(v), `"'`); s != "" {
					return truncateRunes(s, 80)
				}
			}
			continue
		}
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		return truncateRunes(t, 80)
	}
	return ""
}
