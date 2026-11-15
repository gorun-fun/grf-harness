// share_test.go 共享对象存储单元测试：往返、SHA256、限额、TTL 清扫、
// 句柄不可猜测。
package cluster

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestShareStoreRoundtrip(t *testing.T) {
	dir := t.TempDir()
	s := newShareStore(dir, time.Hour)
	payload := []byte("hello cluster share")
	meta, err := s.AddFromReader(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.Ref) != 16 {
		t.Fatalf("ref 应 16 hex，得到 %q", meta.Ref)
	}
	sum := sha256.Sum256(payload)
	if meta.SHA256 != hex.EncodeToString(sum[:]) || meta.Size != int64(len(payload)) {
		t.Fatalf("摘要/大小不符：%+v", meta)
	}
	// 落盘 0600、文件名 = ref
	fi, err := os.Stat(filepath.Join(dir, meta.Ref+".tgz"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("对象文件权限 %v，want 0600", fi.Mode().Perm())
	}
	// Open 读取内容一致
	f, _, err := s.Open(meta.Ref)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got := make([]byte, len(payload))
	if _, err := f.Read(got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("读回内容不一致")
	}
}

func TestShareStoreNotFound(t *testing.T) {
	s := newShareStore(t.TempDir(), time.Hour)
	if _, _, err := s.Open("deadbeef00000000"); err != ErrObjNotFound {
		t.Fatalf("未知句柄应 ErrObjNotFound，得到 %v", err)
	}
}

func TestShareStoreOverLimit(t *testing.T) {
	s := newShareStore(t.TempDir(), time.Hour)
	// LimitReader 在 store 内部按 ShareMaxBytes+1 截断，构造超限流（稀疏
	// 生成器，不真分配内存）
	big := &zeroReader{n: ShareMaxBytes + 1}
	if _, err := s.AddFromReader(big); err == nil || !bytes.Contains([]byte(err.Error()), []byte("exceeds")) {
		t.Fatalf("超限应报 exceeds，得到 %v", err)
	}
	// 失败不建索引、不留半包
	if _, _, err := s.Open("0000000000000000"); err != ErrObjNotFound {
		t.Fatalf("失败后不应有对象")
	}
}

func TestShareStoreTTL(t *testing.T) {
	s := newShareStore(t.TempDir(), 50*time.Millisecond)
	meta, err := s.AddFromReader(bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Open(meta.Ref); err != nil {
		t.Fatalf("TTL 内应可读：%v", err)
	}
	time.Sleep(80 * time.Millisecond)
	if _, _, err := s.Open(meta.Ref); err != ErrObjNotFound {
		t.Fatalf("过期应清扫，得到 %v", err)
	}
	// 盘面也清掉
	if _, err := os.Stat(filepath.Join(s.dir, meta.Ref+".tgz")); !os.IsNotExist(err) {
		t.Fatalf("过期对象文件应删除")
	}
}

// zeroReader 产出 n 个 'a'（超限测试用，免真分配）。
type zeroReader struct{ n int64 }

func (z *zeroReader) Read(p []byte) (int, error) {
	if z.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > z.n {
		p = p[:z.n]
	}
	for i := range p {
		p[i] = 'a'
	}
	z.n -= int64(len(p))
	return len(p), nil
}

// newFakeObjServer /cluster/files/obj 接收端桩：校验 Bearer、记录收到的
// 字节、回固定 ack。
func newFakeObjServer(t *testing.T, got *[]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cluster/files/obj" || r.Method != http.MethodPost {
			http.Error(w, "not found", 404)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", 401)
			return
		}
		data, _ := io.ReadAll(r.Body)
		*got = data
		_, _ = w.Write([]byte(`{"ref":"0123456789abcdef","sha256":"abc","size":13}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// PushObject 流式上传 + 响应解析（httptest 接收端）。
func TestPushObject(t *testing.T) {
	var got []byte
	srv := newFakeObjServer(t, &got)
	c := NewNodeClient(srv.URL, "tok")
	meta, err := c.PushObject(t.Context(), bytes.NewReader([]byte("payload-bytes")))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "payload-bytes" {
		t.Fatalf("接收端内容不符：%q", got)
	}
	if meta.Ref != "0123456789abcdef" {
		t.Fatalf("ref 解析不符：%+v", meta)
	}
}
