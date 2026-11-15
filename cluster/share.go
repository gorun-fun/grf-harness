// share.go 集群共享对象存储（M2 文件传输的接收侧地基）：POST
// /cluster/files/obj 落盘的 tar.gz 对象 + 进程内索引。push-only 语义——
// 对象由发送方推到接收方的 ~/.grf/cluster-share/，ref 只是"对象已在接收
// 方本地"的句柄，不存在拉取端点。TTL 惰性清扫（写入/读取时顺带，无后台
// goroutine）。id 为 crypto/rand 16hex，索引按 id 精确查找，天然无路径
// 穿越面（文件名由存储生成，不含任何请求侧输入）。
package cluster

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"git.gorun.fun/share/grf-harness/config"
)

// 共享对象限额与生命周期。
const (
	ShareMaxBytes = 256 << 20 // 单对象上限（与打包侧一致；超额整个拒绝）
	shareTTL      = 30 * time.Minute
)

// ObjMeta 一个共享对象的元数据。
type ObjMeta struct {
	Ref      string `json:"ref"` // 16 hex 句柄（= 落盘文件名去后缀）
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
	Creation int64  `json:"creation"` // unix 毫秒（TTL 判定；毫秒粒度便于测试）
}

// ObjAck POST /cluster/files/obj 的响应体（接收方声明收到的对象）。
type ObjAck struct {
	ObjMeta
}

// ArtifactMeta 一个落定回合回传的产物清单（ResponseResource 的
// grf_artifacts 扩展字段元素；ORS §11——既有对象加字段用 grf_ 前缀）。
// 指挥官路由层读到后从本地共享区取对象解包，模型经落定通知感知路径。
type ArtifactMeta struct {
	Ref    string   `json:"ref"`              // 接收方（指挥官）共享区句柄
	SHA256 string   `json:"sha256,omitempty"` // 发送侧（节点）算得的摘要
	Files  []string `json:"files,omitempty"`  // 归档内文件（相对归档根，字典序）
	Error  string   `json:"error,omitempty"`  // 回传失败原因（此时无 ref）
}

// ShareStore 共享对象存储。并发安全；进程级单实例（挂在 Router 上，
// server / app 两条装配路径共用）。
type ShareStore struct {
	dir string
	ttl time.Duration

	mu   sync.Mutex
	objs map[string]ObjMeta // ref → 元数据
}

// NewShareStore 构建共享存储（目录 ~/.grf/cluster-share，GRF_HOME 可重定位）。
func NewShareStore() *ShareStore {
	return newShareStore(filepath.Join(config.Home(), "cluster-share"), shareTTL)
}

// newShareStore 指定目录与 TTL（测试用）。
func newShareStore(dir string, ttl time.Duration) *ShareStore {
	return &ShareStore{dir: dir, ttl: ttl, objs: map[string]ObjMeta{}}
}

// AddFromReader 流式接收一个对象：写临时文件（同时算 SHA256、超额即断）
// → 0600 → 原子改名入区 → 建索引。调用方拿到 ref 后即可经本地 Open 取用。
func (s *ShareStore) AddFromReader(r io.Reader) (ObjMeta, error) {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return ObjMeta{}, fmt.Errorf("cluster share: %w", err)
	}
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return ObjMeta{}, fmt.Errorf("cluster share: gen id: %w", err)
	}
	ref := hex.EncodeToString(buf)
	tmp, err := os.CreateTemp(s.dir, ".obj-*")
	if err != nil {
		return ObjMeta{}, fmt.Errorf("cluster share: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // 改名成功后为无害 no-op
	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, ShareMaxBytes+1))
	if err != nil {
		tmp.Close()
		return ObjMeta{}, fmt.Errorf("cluster share: receive: %w", err)
	}
	if size > ShareMaxBytes {
		tmp.Close()
		return ObjMeta{}, fmt.Errorf("cluster share: object exceeds %d bytes limit", ShareMaxBytes)
	}
	if err := tmp.Close(); err != nil {
		return ObjMeta{}, fmt.Errorf("cluster share: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return ObjMeta{}, fmt.Errorf("cluster share: %w", err)
	}
	final := filepath.Join(s.dir, ref+".tgz")
	if err := os.Rename(tmpName, final); err != nil {
		return ObjMeta{}, fmt.Errorf("cluster share: %w", err)
	}
	meta := ObjMeta{Ref: ref, SHA256: hex.EncodeToString(h.Sum(nil)), Size: size, Creation: time.Now().UnixMilli()}
	s.mu.Lock()
	s.objs[ref] = meta
	s.mu.Unlock()
	s.sweepLocked()
	return meta, nil
}

// ErrObjNotFound ref 不存在（或已过期清扫）。
var ErrObjNotFound = errors.New("cluster share: object not found")

// Open 按句柄打开对象（读取方在接收方进程内）。文件句柄由调用方关闭。
func (s *ShareStore) Open(ref string) (*os.File, ObjMeta, error) {
	s.sweepLocked()
	s.mu.Lock()
	meta, ok := s.objs[ref]
	s.mu.Unlock()
	if !ok {
		return nil, ObjMeta{}, ErrObjNotFound
	}
	f, err := os.Open(filepath.Join(s.dir, ref+".tgz"))
	if err != nil {
		// 索引与盘面脱节（进程重启 / 手工删除）：条目作废
		s.mu.Lock()
		delete(s.objs, ref)
		s.mu.Unlock()
		return nil, ObjMeta{}, ErrObjNotFound
	}
	return f, meta, nil
}

// sweepLocked 清扫过期对象（索引 + 盘面）。命名沿用仓库习惯（锁外调用的
// 公共清理路径；内部持锁仅护索引）。
func (s *ShareStore) sweepLocked() {
	cutoff := time.Now().Add(-s.ttl).UnixMilli()
	var expired []ObjMeta
	s.mu.Lock()
	for ref, m := range s.objs {
		if m.Creation < cutoff {
			expired = append(expired, m)
			delete(s.objs, ref)
		}
	}
	s.mu.Unlock()
	for _, m := range expired {
		_ = os.Remove(filepath.Join(s.dir, m.Ref+".tgz"))
	}
}
