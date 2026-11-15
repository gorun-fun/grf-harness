// cluster_files.go 集群文件传输端点与接线（M2）：POST /cluster/files/obj
// （共享对象接收，guardCluster 双令牌）、种子解包（newSession 在建
// harness 前调用）、落定产物回传（execute 尾部、快照持久化之前）。
// push-only——对象只进接收方 ~/.grf/cluster-share/，ref 即本地句柄，
// 不存在拉取端点（NAT 友好，攻击面减半）。
package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"git.gorun.fun/share/grf-harness/cluster"
)

// handleClusterObjPost POST /cluster/files/obj：流式接收一个 tar.gz 共享
// 对象（限额/SHA256/过期清扫由 ShareStore 承担），返回分配的 ref。
func (s *Server) handleClusterObjPost(w http.ResponseWriter, r *http.Request) {
	if s.cluster == nil {
		writeError(w, 404, errNotFound, "cluster is not enabled on this server", "")
		return
	}
	meta, err := s.cluster.Share().AddFromReader(r.Body)
	if err != nil {
		status := 400
		if strings.Contains(err.Error(), "exceeds") {
			status = 413
		}
		writeError(w, status, errInvalid, err.Error(), "input")
		return
	}
	writeJSON(w, 200, cluster.ObjAck{ObjMeta: meta})
}

// errSeedBad 种子解包失败（客户端指定的 seed_ref 无效或内容非法）——
// 建会话失败按 400（客户端输入错误）而非 500 回给派发方。
var errSeedBad = errors.New("invalid seed_ref")

// unpackSeed 种子解包：ref 指向本节点共享区的 tar.gz，解到会话工作区。
// newSession 在建 harness 前调用；失败即建会话失败（POST /v1/responses
// 直接 400 回指挥官，路由层把错误转成派发失败）。
func (s *Server) unpackSeed(seedRef, ws string) error {
	if s.cluster == nil {
		return fmt.Errorf("%w: cluster is not enabled on this node", errSeedBad)
	}
	f, _, err := s.cluster.Share().Open(seedRef)
	if err != nil {
		return fmt.Errorf("%w %q: %v", errSeedBad, seedRef, err)
	}
	defer f.Close()
	if _, err := cluster.UnpackTo(f, ws); err != nil {
		return fmt.Errorf("%w: %v", errSeedBad, err)
	}
	return nil
}

// collectArtifacts 落定产物回传（M2）：会话带 return_files 且回合
// completed 时，把工作区匹配产物打包推回指挥官共享区，清单写进
// grf_artifacts（随落定快照持久化，指挥官路由层轮询读取后解包并经落定
// 通知告知路径）。推送失败不致败回合——清单记 error，通知如实转达；
// 回合未完成（失败/取消/暂停）不回传。
func (s *Server) collectArtifacts(sess *session, base *ResponseResource) {
	if len(sess.meta.ReturnFiles) == 0 || base.Status != statusCompleted {
		return
	}
	if s.clusterCfg == nil || s.clusterCfg.CommanderURL == "" {
		// 纯静态表节点（无 call-home 配置）没有回传通道：如实声明
		base.GrfArtifacts = []cluster.ArtifactMeta{{
			Error: "artifact return is unavailable: this node has no commander_url configured"}}
		return
	}
	ws := sess.meta.Workspace
	var paths []string
	for _, pat := range sess.meta.ReturnFiles {
		full := pat
		if !filepath.IsAbs(full) {
			full = filepath.Join(ws, pat)
		}
		ms, err := filepath.Glob(full)
		if err != nil {
			continue // 坏模式按无匹配处理，不阻断其余模式
		}
		paths = append(paths, ms...)
	}
	var buf bytes.Buffer
	files, err := cluster.PackPaths(nil, ws, paths, &buf)
	am := cluster.ArtifactMeta{Files: files}
	if err != nil {
		am = cluster.ArtifactMeta{Error: err.Error()}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		c := cluster.NewNodeClient(s.clusterCfg.CommanderURL, s.clusterCfg.Token)
		ack, perr := c.PushObject(ctx, &buf)
		if perr != nil {
			am = cluster.ArtifactMeta{Error: "push artifacts to commander: " + perr.Error()}
		} else {
			am.Ref, am.SHA256 = ack.Ref, ack.SHA256
		}
	}
	base.GrfArtifacts = []cluster.ArtifactMeta{am}
}
