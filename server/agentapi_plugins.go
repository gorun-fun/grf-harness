// agentapi_plugins.go — /v1/agent/plugins 域:settings.json plugins 键的
// 显式列表读写与自动发现目录枚举。
package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/plugin"
)

// handleAgentPluginsGet GET /v1/agent/plugins。
func (s *Server) handleAgentPluginsGet(w http.ResponseWriter, r *http.Request) {
	path := config.Path()
	dir := filepath.Join(config.Home(), "plugins")
	out := pluginsDTO{
		Path: path, Dir: dir,
		Plugins: []pluginDTO{}, Discovered: []string{},
		Writable: settingsWritable(path),
	}
	all, err := readSettingsRaw()
	if err != nil {
		writeError(w, 500, errServer, err.Error(), "")
		return
	}
	if v, ok := all["plugins"]; ok {
		out.Explicit = true
		list := []ghPlugin{}
		_ = json.Unmarshal(v, &list)
		for _, p := range list {
			if p.Command == nil {
				p.Command = []string{}
			}
			out.Plugins = append(out.Plugins, pluginDTO{
				Name: p.Name, Command: p.Command, URL: p.URL, Timeout: p.Timeout,
				HasTok: strings.TrimSpace(p.Token) != "",
			})
		}
	}
	if ents, err := os.ReadDir(dir); err == nil {
		for _, e := range ents {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") || strings.HasSuffix(e.Name(), ".md") || plugin.IsStaleOldName(e.Name()) {
				continue
			}
			if info, err := e.Info(); err == nil && plugin.IsExecutableFile(e.Name(), info.Mode()) {
				out.Discovered = append(out.Discovered, e.Name())
			}
		}
		sort.Strings(out.Discovered)
	}
	writeJSON(w, 200, out)
}

// handleAgentPluginsPut PUT /v1/agent/plugins:explicit=false 删 plugins 键
// (回到自动发现);true 写显式列表。token 空 = 按同名保留现值。
func (s *Server) handleAgentPluginsPut(w http.ResponseWriter, r *http.Request) {
	var in pluginsDTO
	if !decodeJSON(w, r, &in, bodyLimitSmall) {
		return
	}
	all, err := readSettingsRaw()
	if err != nil {
		writeError(w, 500, errServer, err.Error(), "")
		return
	}
	if !in.Explicit {
		delete(all, "plugins")
	} else {
		cur := []ghPlugin{}
		if v, ok := all["plugins"]; ok {
			_ = json.Unmarshal(v, &cur)
		}
		curTok := func(name string) string {
			for _, p := range cur {
				if strings.TrimSpace(p.Name) == strings.TrimSpace(name) {
					return strings.TrimSpace(p.Token)
				}
			}
			return ""
		}
		list := make([]ghPlugin, 0, len(in.Plugins))
		for _, p := range in.Plugins {
			if strings.TrimSpace(p.Name) == "" && strings.TrimSpace(p.URL) == "" && len(p.Command) == 0 {
				continue
			}
			cmd := make([]string, 0, len(p.Command))
			for _, c := range p.Command {
				if v := strings.TrimSpace(c); v != "" {
					cmd = append(cmd, v)
				}
			}
			tok := strings.TrimSpace(p.Token)
			if tok == "" {
				tok = curTok(p.Name)
			}
			list = append(list, ghPlugin{
				Name: strings.TrimSpace(p.Name), Command: cmd,
				URL: strings.TrimSpace(p.URL), Token: tok, Timeout: p.Timeout,
			})
		}
		raw, err := json.Marshal(list)
		if err != nil {
			writeError(w, 500, errServer, err.Error(), "")
			return
		}
		all["plugins"] = raw
	}
	if err := writeSettingsRaw(all); err != nil {
		writeError(w, 500, errServer, err.Error(), "")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
