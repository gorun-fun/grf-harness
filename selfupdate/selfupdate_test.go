// selfupdate_test.go 版本比较表驱动 + httptest 模拟下载服务器验证探测与安装。
package selfupdate

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	// 序关系与 grf-web/lib/downloads.ts 一致（数字标识符 < 字母；标识符少的小）
	ordered := []string{
		"0.0.1-1",
		"0.0.1-alpha",
		"0.0.1-alpha.1",
		"0.0.1-alpha.2",
		"0.0.1-alpha.6",
		"0.0.1-rc.1",
		"0.0.1",
		"0.1.0",
		"0.1.1",
		"0.2.0",
		"1.0.0",
	}
	for i, a := range ordered {
		if got := CompareVersions(a, a); got != 0 {
			t.Errorf("CompareVersions(%q, %q) = %d, want 0", a, a, got)
		}
		for _, b := range ordered[i+1:] {
			if got := CompareVersions(a, b); got != -1 {
				t.Errorf("CompareVersions(%q, %q) = %d, want -1", a, b, got)
			}
			if got := CompareVersions(b, a); got != 1 {
				t.Errorf("CompareVersions(%q, %q) = %d, want 1", b, a, got)
			}
		}
	}
	// 长度不齐的主版本段按 0 补齐
	if got := CompareVersions("1.0", "1.0.0"); got != 0 {
		t.Errorf("CompareVersions(1.0, 1.0.0) = %d, want 0", got)
	}
	if got := CompareVersions("1.0.1", "1"); got != 1 {
		t.Errorf("CompareVersions(1.0.1, 1) = %d, want 1", got)
	}
}

func TestPlatformDir(t *testing.T) {
	cases := []struct {
		goos, goarch, dir string
		ok                bool
	}{
		{"darwin", "arm64", "macos-arm64", true},
		{"darwin", "amd64", "macos", true},
		{"linux", "amd64", "linux", true},
		{"linux", "arm64", "linux-arm64", true},
		{"windows", "amd64", "windows", true},
		{"windows", "arm64", "windows-arm64", true},
		{"darwin", "386", "", false},
		{"freebsd", "amd64", "", false},
	}
	for _, c := range cases {
		dir, ok := PlatformDir(c.goos, c.goarch)
		if dir != c.dir || ok != c.ok {
			t.Errorf("PlatformDir(%s, %s) = %q,%v want %q,%v", c.goos, c.goarch, dir, ok, c.dir, c.ok)
		}
	}
	if _, ok := PlatformDir(runtime.GOOS, runtime.GOARCH); !ok {
		t.Logf("当前平台 %s/%s 无发布目录（合法，CI 仅发六平台）", runtime.GOOS, runtime.GOARCH)
	}
}

// page 生成 nginx autoindex 风格的目录页。
func page(entries ...string) string {
	var b strings.Builder
	b.WriteString("<html><body>\n")
	for _, e := range entries {
		b.WriteString(fmt.Sprintf("<a href=\"%s/\">%s/</a>\n", e, e))
	}
	b.WriteString("</body></html>\n")
	return b.String()
}

// filePage 目录页混合文件与目录（文件条目无尾斜杠）。
func filePage(files ...string) string {
	var b strings.Builder
	b.WriteString("<html><body>\n")
	for _, f := range files {
		b.WriteString(fmt.Sprintf("<a href=\"%s\">%s</a>\n", f, f))
	}
	b.WriteString("</body></html>\n")
	return b.String()
}

// newFileServer 模拟下载服务器：paths 为 路径→响应体。
func newFileServer(t *testing.T, paths map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := paths[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, body)
	}))
}

func TestCheck(t *testing.T) {
	dir, ok := PlatformDir(runtime.GOOS, runtime.GOARCH)
	if !ok {
		t.Skip("当前平台无发布目录")
	}
	binary := BinaryName(runtime.GOOS)
	srv := newFileServer(t, map[string]string{
		"/":                   page("macos", "macos-arm64", "linux", "linux-arm64", "windows", "windows-arm64"),
		"/" + dir + "/":       page("0.1.0", "0.2.0", "0.3.0"),
		"/" + dir + "/0.1.0/": filePage(binary),
		"/" + dir + "/0.2.0/": filePage(binary),
	})
	defer srv.Close()

	// 最高版本 0.3.0 目录 404（未在 paths 里）：跳过后应回落到 0.2.0
	rel, err := Check(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if rel == nil {
		t.Fatal("期望探测到版本，得到 nil")
	}
	if rel.Version != "0.2.0" {
		t.Errorf("Version = %q, want 0.2.0", rel.Version)
	}
	if want := srv.URL + "/" + dir + "/0.2.0/" + binary; rel.URL != want {
		t.Errorf("URL = %q, want %q", rel.URL, want)
	}
}

func TestCheckNoRelease(t *testing.T) {
	// 平台目录存在但没有版本
	dir, ok := PlatformDir(runtime.GOOS, runtime.GOARCH)
	if !ok {
		t.Skip("当前平台无发布目录")
	}
	srv := newFileServer(t, map[string]string{
		"/":             page(dir),
		"/" + dir + "/": filePage("README"),
	})
	defer srv.Close()

	rel, err := Check(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if rel != nil {
		t.Errorf("无产物时期望 nil，得到 %+v", rel)
	}
}

func TestCheckUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // 立即关闭制造连接失败

	if _, err := Check(context.Background(), url); err == nil {
		t.Fatal("服务器不可达时应返回错误")
	}
}

func TestInstall(t *testing.T) {
	payload := "fake-binary-content"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, payload)
	}))
	defer srv.Close()

	dir := t.TempDir()
	exePath := filepath.Join(dir, "gh")
	if err := os.WriteFile(exePath, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}

	var calls int
	err := Install(context.Background(), srv.URL, exePath, func(done, total int64) { calls++ })
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != payload {
		t.Errorf("替换后内容 = %q, want %q", data, payload)
	}
	st, err := os.Stat(exePath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o755 {
		t.Errorf("权限 = %v, want 0755（沿用旧文件）", st.Mode().Perm())
	}
	if calls == 0 {
		t.Error("progress 未被回调")
	}
	// 临时文件不残留
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Errorf("目录应只剩 1 个文件，实际 %d 个", len(ents))
	}
}

func TestInstallHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	if err := Install(context.Background(), srv.URL, filepath.Join(t.TempDir(), "gh"), nil); err == nil {
		t.Fatal("HTTP 404 应报错")
	}
}
