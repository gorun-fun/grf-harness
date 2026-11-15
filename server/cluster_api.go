// cluster_api.go 集群控制面端点：/cluster/capabilities（能力清单）、
// /cluster/register + /cluster/heartbeat（call-home 注册与心跳）、
// /cluster/members（成员表快照）。鉴权走双令牌（server 自身 token 或
// 集群共享密钥 cluster.token）——节点持集群密钥即可注册，不必知晓
// 指挥官的 server token（那是 RCE 级权限）。任务面（派发/取消/应答）
// 不在此文件：完全复用既有 ORS 端点（见 cluster 包 NodeClient）。
package server

import (
	"crypto/subtle"
	"net/http"
	"runtime"
	"time"

	"git.gorun.fun/share/grf-harness/cluster"
	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/plugin"
	"git.gorun.fun/share/grf-harness/tools"
)

// EnableCluster 启用集群：注入路由层（指挥官侧任务派发）与集群配置
// （/cluster/* 端点注册与双令牌鉴权依据）。须在 Handler() 之前调用
// （路由表按启用状态装配）。幂等。
func (s *Server) EnableCluster(r *cluster.Router, cfg *config.Cluster) {
	s.cluster = r
	if cfg != nil {
		s.clusterCfg = cfg
	}
}

// ClusterRouter 已注入的集群路由层（nil = 未启用）。
func (s *Server) ClusterRouter() *cluster.Router { return s.cluster }

// bearerOK 常量时间校验 Bearer 令牌。
func bearerOK(auth, token string) bool {
	if token == "" {
		return false
	}
	want := "Bearer " + token
	return len(auth) == len(want) && subtle.ConstantTimeCompare([]byte(auth), []byte(want)) == 1
}

// guardCluster 集群端点鉴权：接受 server 自身 token 或集群共享密钥
// （cluster.token 非空时）。后者让 call-home 节点无需持有指挥官的
// RCE 级 server token 即可注册/心跳。
func (s *Server) guardCluster(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !bearerOK(auth, s.token) && !bearerOK(auth, s.clusterToken()) {
			writeError(w, 401, errInvalid, "missing or invalid bearer token", "")
			return
		}
		h(w, r)
	}
}

func (s *Server) clusterToken() string {
	if s.clusterCfg == nil {
		return ""
	}
	return s.clusterCfg.Token
}

// RunningTurns 当前在跑回合数（节点心跳上报的负载口径）。
func (s *Server) RunningTurns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, sess := range s.sessions {
		if sess.root.IsRunning() {
			n++
		}
	}
	return n
}

// handleClusterCaps GET /cluster/capabilities：本节点能力清单。
func (s *Server) handleClusterCaps(w http.ResponseWriter, r *http.Request) {
	caps, err := s.ClusterCaps()
	if err != nil {
		writeError(w, 500, errServer, "load cluster identity: "+err.Error(), "")
		return
	}
	writeJSON(w, 200, caps)
}

// ClusterCaps 构建本节点能力清单（/cluster/capabilities 响应与节点侧
// call-home 注册上报共用一份事实）。settings cluster.name 非空时覆盖
// 显示名（与注册/心跳的节点路由名对齐）。
func (s *Server) ClusterCaps() (*cluster.Capabilities, error) {
	id, err := cluster.LoadIdentity()
	if err != nil {
		return nil, err
	}
	caps := &cluster.Capabilities{
		NodeID:   id.ID,
		Name:     id.Name,
		Version:  config.Version,
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
		Tools:    tools.SpecNames(),
		TS:       time.Now().Unix(),
	}
	if s.clusterCfg != nil && s.clusterCfg.Name != "" {
		caps.Name = s.clusterCfg.Name
	}
	// 模型与插件按当前 settings 实况上报（不泄密钥：只报名单）
	if cfg, err := config.LoadFileForUpdate(); err == nil {
		for _, m := range cfg.ListModels() {
			caps.Models = append(caps.Models, m.Provider+"/"+m.Model)
		}
		if pm := plugin.Shared(cfg); pm != nil {
			owned := pm.Tools()
			for _, c := range pm.Conns() {
				n := 0
				for _, info := range owned {
					if info.Plugin == c.Name {
						n++
					}
				}
				caps.Plugins = append(caps.Plugins, cluster.PluginCaps{Name: c.Name, Tools: n})
			}
		}
	}
	return caps, nil
}

// handleClusterRegister POST /cluster/register：call-home 注册/复活
// （body 见 cluster.RegisterRequest）。凭集群共享密钥鉴权（guardCluster）。
func (s *Server) handleClusterRegister(w http.ResponseWriter, r *http.Request) {
	if s.cluster == nil {
		writeError(w, 404, errNotFound, "cluster is not enabled on this server", "")
		return
	}
	var req cluster.RegisterRequest
	if !decodeJSON(w, r, &req, bodyLimitSmall) {
		return
	}
	if req.NodeID == "" || req.Name == "" {
		writeError(w, 400, errInvalid, "node_id and name are required", "")
		return
	}
	s.cluster.Table().Register(cluster.Member{
		NodeID: req.NodeID,
		Name:   req.Name,
		Addr:   req.Addr,
		Caps:   req.Capabilities,
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleClusterHeartbeat POST /cluster/heartbeat：刷新成员在线状态与
// 负载。未知节点自动以最小条目入表（等价一次匿名注册）。
func (s *Server) handleClusterHeartbeat(w http.ResponseWriter, r *http.Request) {
	if s.cluster == nil {
		writeError(w, 404, errNotFound, "cluster is not enabled on this server", "")
		return
	}
	var req cluster.HeartbeatRequest
	if !decodeJSON(w, r, &req, bodyLimitSmall) {
		return
	}
	if req.NodeID == "" && req.Name == "" {
		writeError(w, 400, errInvalid, "node_id or name is required", "")
		return
	}
	s.cluster.Table().Heartbeat(req.NodeID, req.Name, req.Running)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleClusterMembers GET /cluster/members：成员表快照（指挥官权威）。
func (s *Server) handleClusterMembers(w http.ResponseWriter, r *http.Request) {
	if s.cluster == nil {
		writeError(w, 404, errNotFound, "cluster is not enabled on this server", "")
		return
	}
	writeJSON(w, 200, s.cluster.Table().Snapshot())
}
