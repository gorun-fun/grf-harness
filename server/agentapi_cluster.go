// agentapi_cluster.go — /v1/agent/cluster：集群配置段的读写（goder 等
// 宿主设置页的配置通道；桌面直写 settings.json 之外的 web 路径）。
// 落盘经 readSettingsRaw/writeSettingsRaw（未知键原样保留）。
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"git.gorun.fun/share/grf-harness/config"
)

// clusterCfgDTO GET 响应：配置路径与可写探测 + 集群段原文。
type clusterCfgDTO struct {
	Path     string         `json:"path"`
	Writable bool           `json:"writable"`
	Cluster  *config.Cluster `json:"cluster"`
}

// handleAgentClusterGet GET /v1/agent/cluster。
func (s *Server) handleAgentClusterGet(w http.ResponseWriter, r *http.Request) {
	path := config.Path()
	out := clusterCfgDTO{Path: path, Writable: settingsWritable(path)}
	all, err := readSettingsRaw()
	if err != nil {
		writeError(w, 500, errServer, err.Error(), "")
		return
	}
	if v, ok := all["cluster"]; ok {
		var c config.Cluster
		if json.Unmarshal(v, &c) == nil {
			out.Cluster = &c
		}
	}
	writeJSON(w, 200, out)
}

// handleAgentClusterPut PUT /v1/agent/cluster：整体替换集群段。校验
// role/sync/discover 枚举与 policy 归一，节点表条目去空白；enabled
// 显式写 false 亦落盘（API 关闭集群的正规途径）。
func (s *Server) handleAgentClusterPut(w http.ResponseWriter, r *http.Request) {
	var in config.Cluster
	if !decodeJSON(w, r, &in, bodyLimitSmall) {
		return
	}
	if err := normalizeClusterCfg(&in); err != nil {
		writeError(w, 400, errInvalid, err.Error(), "cluster")
		return
	}
	all, err := readSettingsRaw()
	if err != nil {
		writeError(w, 500, errServer, err.Error(), "")
		return
	}
	raw, err := json.Marshal(&in)
	if err != nil {
		writeError(w, 500, errServer, err.Error(), "")
		return
	}
	all["cluster"] = raw
	if err := writeSettingsRaw(all); err != nil {
		writeError(w, 500, errServer, err.Error(), "")
		return
	}
	writeJSON(w, 200, clusterCfgDTO{Path: config.Path(), Writable: true, Cluster: &in})
}

// normalizeClusterCfg 集群段校验与归一（就地修改）。
func normalizeClusterCfg(c *config.Cluster) error {
	switch strings.ToLower(strings.TrimSpace(c.Role)) {
	case "", "node":
		c.Role = "node"
	case "commander":
		c.Role = "commander"
	default:
		return apiErr(errInvalid, fmt.Sprintf("role must be node or commander (got %q)", c.Role))
	}
	switch strings.ToLower(strings.TrimSpace(c.Sync)) {
	case "":
	case "none", "seed", "git", "full":
		c.Sync = strings.ToLower(strings.TrimSpace(c.Sync))
	default:
		return apiErr(errInvalid, fmt.Sprintf("sync must be none, seed, git or full (got %q)", c.Sync))
	}
	switch strings.ToLower(strings.TrimSpace(c.Discover)) {
	case "":
	case "off", "lan":
		c.Discover = strings.ToLower(strings.TrimSpace(c.Discover))
	default:
		return apiErr(errInvalid, fmt.Sprintf("discover must be off or lan (got %q)", c.Discover))
	}
	if c.Policy != "" {
		c.Policy = config.NormalizePolicy(c.Policy)
	}
	var nodes []config.ClusterNode
	for _, n := range c.Nodes {
		n.Addr = strings.TrimSpace(n.Addr)
		if n.Addr == "" {
			continue
		}
		n.Name = strings.TrimSpace(n.Name)
		nodes = append(nodes, n)
	}
	c.Nodes = nodes
	c.Name = strings.TrimSpace(c.Name)
	c.CommanderURL = strings.TrimSpace(c.CommanderURL)
	return nil
}
