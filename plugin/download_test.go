// download_test.go 官方插件下载测试：httptest 模拟 nginx autoindex 发布
// 服务器（{base}/{平台}/{版本}/plugins/ 布局），覆盖探测的版本选择、
// 名称宽松匹配与落盘（GRF_HOME 重定位到临时目录，不碰真实 ~/.grf）。
package plugin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"git.gorun.fun/share/grf-harness/selfupdate"
)

// newReleaseServer 模拟发布服务器。releases: 版本 → 插件文件名清单；
// 文件内容为 "fake-<版本>-<文件名>"。
func newReleaseServer(t *testing.T, releases map[string][]string) *httptest.Server {
	t.Helper()
	dir, ok := selfupdate.PlatformDir(runtime.GOOS, runtime.GOARCH)
	if !ok {
		t.Skip("当前平台无发布目录")
	}
	mux := http.NewServeMux()
	root := func(w http.ResponseWriter, entries ...string) {
		var b strings.Builder
		b.WriteString("<html><body>\n")
		for _, e := range entries {
			b.WriteString(fmt.Sprintf("<a href=\"%s\">%s</a>\n", e, e))
		}
		b.WriteString("</body></html>\n")
		fmt.Fprint(w, b.String())
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch p {
		case "/":
			root(w, dir)
		case "/" + dir + "/":
			versions := make([]string, 0, len(releases))
			for v := range releases {
				versions = append(versions, v)
			}
			sort.Strings(versions)
			root(w, versions...)
		default:
			// 形如 {dir}/{ver}/plugins/（目录列表，尾斜杠）或
			// {dir}/{ver}/plugins/{file}（文件体）
			rest := strings.TrimPrefix(p, "/"+dir+"/")
			parts := strings.Split(rest, "/")
			if len(parts) == 3 && parts[1] == "plugins" {
				ver, file := parts[0], parts[2]
				files, ok := releases[ver]
				if !ok {
					http.NotFound(w, r)
					return
				}
				if file == "" { // 目录列表页（尾斜杠）
					root(w, files...)
					return
				}
				for _, f := range files {
					if f == file {
						fmt.Fprintf(w, "fake-%s-%s", ver, file)
						return
					}
				}
			}
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestProbePluginsExplicitVersion(t *testing.T) {
	srv := newReleaseServer(t, map[string][]string{
		"0.1.0": {"grf-browser", "grf-computer"},
	})
	rel, err := ProbePlugins(context.Background(), srv.URL, "v0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != "0.1.0" {
		t.Errorf("Version = %q, want 0.1.0", rel.Version)
	}
	if want := "grf-browser、grf-computer"; strings.Join(rel.Files, "、") != want {
		t.Errorf("Files = %v, want [%s]", rel.Files, want)
	}
	if got := rel.URL("grf-browser"); got != srv.URL+"/"+rel.Dir+"/0.1.0/plugins/grf-browser" {
		t.Errorf("URL = %q", got)
	}

	// 指定不存在的版本：报错而非静默回落
	if _, err := ProbePlugins(context.Background(), srv.URL, "9.9.9"); err == nil {
		t.Error("指定版本无产物应报错")
	}
}

func TestProbePluginsLatest(t *testing.T) {
	srv := newReleaseServer(t, map[string][]string{
		"0.1.0":     {"grf-browser"},
		"0.2.0-rc.1": {"grf-browser"},
		"0.3.0":     nil, // 空目录：无产物，应跳过
	})
	rel, err := ProbePlugins(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if rel == nil {
		t.Fatal("期望探测到版本，得到 nil")
	}
	// 0.3.0 目录存在但无文件 → 取 0.2.0-rc.1
	if rel.Version != "0.2.0-rc.1" {
		t.Errorf("Version = %q, want 0.2.0-rc.1", rel.Version)
	}
}

func TestProbePluginsNone(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	rel, err := ProbePlugins(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if rel != nil {
		t.Errorf("服务器无产物期望 nil，得到 %+v", rel)
	}
}

func TestDownload(t *testing.T) {
	srv := newReleaseServer(t, map[string][]string{
		"0.1.0": {"grf-browser", "grf-computer"},
	})
	home := t.TempDir()
	t.Setenv("GRF_HOME", home)

	rel, err := ProbePlugins(context.Background(), srv.URL, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}

	// 指定名称 + 宽松匹配（省略 grf- 前缀）
	saved, err := rel.Download(context.Background(), []string{"computer"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 || filepath.Base(saved[0]) != "grf-computer" {
		t.Fatalf("saved = %v, want 仅 grf-computer", saved)
	}
	data, err := os.ReadFile(filepath.Join(home, "plugins", "grf-computer"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "fake-0.1.0-grf-computer" {
		t.Errorf("内容 = %q", data)
	}
	st, err := os.Stat(filepath.Join(home, "plugins", "grf-computer"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o111 == 0 {
		t.Error("落盘文件应保留可执行位")
	}

	// 空名称 = 全部；再次下载同名为覆盖更新
	saved, err = rel.Download(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 2 {
		t.Errorf("全部下载应 2 个文件，实际 %d", len(saved))
	}
	// 临时文件不残留
	ents, _ := os.ReadDir(filepath.Join(home, "plugins"))
	if len(ents) != 2 {
		t.Errorf("插件目录应只剩 2 个文件，实际 %d", len(ents))
	}

	// 未知名称：报错并列出可用项
	if _, err := rel.Download(context.Background(), []string{"nope"}, nil); err == nil {
		t.Error("未知插件名应报错")
	}
}
