// Package cluster 集群模式：多设备上的 gh 实例组成"指挥官-节点"集群。
// 节点 = 开集群开关的 gh server（能力上报、接收派发任务）；指挥官 =
// role=commander 的 gh 实例，subagent 工具的派发经本包路由层分流——
// description 命中节点（@name 或能力标签）时任务经既有 ORS 端点
// （POST /v1/responses，background 模式）派往远端节点，落定经通知回流
// 父会话；远端的审批/提问经暂停协议（grf:input_required / grf:input_result）
// 直达指挥官本地 Host 应答，模型全程无感知。会话与工作区留在节点本地
// （分片即隔离，指挥官不代理文件操作）。配置见 config.Cluster；
// M1 通道：静态节点表（可路由）+ call-home 注册/心跳（成员可见）。
package cluster

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"git.gorun.fun/share/grf-harness/config"
)

// Identity 节点身份：首次启用集群时生成，落 ~/.grf/cluster.json（0600，
// 与 server token 同款落盘模式），跨重启稳定。
type Identity struct {
	ID        string `json:"id"`   // 16 位 hex（与会话 sid 同规格）
	Name      string `json:"name"` // 显示名 / 路由名；空回退 hostname
	CreatedAt int64  `json:"created_at"`
}

// identityPath 身份文件路径（config.Home，GRF_HOME 可重定位）。
func identityPath() string {
	return filepath.Join(config.Home(), "cluster.json")
}

// LoadIdentity 读取或生成本节点身份。生成失败（无家目录等）时退化为
// 进程内随机身份——注册/心跳仍可用，跨重启身份不稳（显示为陌生节点）。
func LoadIdentity() (Identity, error) {
	path := identityPath()
	if b, err := os.ReadFile(path); err == nil {
		var id Identity
		if err := json.Unmarshal(b, &id); err == nil && len(id.ID) == 16 {
			if id.Name == "" {
				id.Name = defaultNodeName()
			}
			return id, nil
		}
	}
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return Identity{ID: fmt.Sprintf("%016x", time.Now().UnixNano()), Name: defaultNodeName()}, nil
	}
	id := Identity{ID: hex.EncodeToString(buf), Name: defaultNodeName(), CreatedAt: time.Now().Unix()}
	data, err := json.MarshalIndent(id, "", "  ")
	if err == nil {
		_ = os.WriteFile(path, append(data, '\n'), 0o600)
	}
	return id, nil
}

// defaultNodeName 节点显示名：hostname 去域名前缀；取不到用 "node"。
func defaultNodeName() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "node"
	}
	for i := 0; i < len(h); i++ {
		if h[i] == '.' {
			return h[:i]
		}
	}
	return h
}

// NodeName 节点表条目的路由名：显式名 > addr 主机名。
func NodeName(n config.ClusterNode) string {
	if n.Name != "" {
		return n.Name
	}
	return HostOf(n.Addr)
}

// HostOf 从地址提取主机名（scheme/host:port 均可；解析失败原样返回）。
func HostOf(addr string) string {
	a := addr
	if i := strings.Index(a, "://"); i >= 0 && i+3 <= len(a) {
		a = a[i+3:]
	}
	if i := strings.IndexByte(a, '/'); i >= 0 {
		a = a[:i]
	}
	if h, _, err := net.SplitHostPort(a); err == nil {
		return h
	}
	return a
}
