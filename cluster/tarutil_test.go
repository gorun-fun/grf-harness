// tarutil_test.go 打包/解包单元测试：往返一致性、路径穿越/绝对路径/
// 符号链接条目拒绝、超限、去重、目录递归。
package cluster

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.gorun.fun/share/grf-harness/tools"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestPackUnpackRoundtrip(t *testing.T) {
	root := writeTree(t, map[string]string{
		"a.txt":          "alpha",
		"sub/b.txt":      "beta",
		"sub/deep/c.txt": "gamma",
		"ignored.txt":    "not packed",
	})
	var buf bytes.Buffer
	files, err := PackPaths(nil, root, []string{"a.txt", "sub"}, &buf)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a.txt", "sub/b.txt", "sub/deep/c.txt"}
	if strings.Join(files, ",") != strings.Join(want, ",") {
		t.Fatalf("清单 %v，want %v", files, want)
	}
	dest := t.TempDir()
	n, err := UnpackTo(&buf, dest)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("解包 %d 文件，want 3", n)
	}
	for name, content := range map[string]string{"a.txt": "alpha", "sub/b.txt": "beta", "sub/deep/c.txt": "gamma"} {
		b, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil || string(b) != content {
			t.Fatalf("%s 读回不符：%q %v", name, b, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dest, "ignored.txt")); !os.IsNotExist(err) {
		t.Fatal("未收录文件不应出现在解包目录")
	}
}

func TestPackPathsDedupe(t *testing.T) {
	root := writeTree(t, map[string]string{"a.txt": "x", "sub/b.txt": "y"})
	var buf bytes.Buffer
	files, err := PackPaths(nil, root, []string{".", "a.txt", "sub", "sub/"}, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0] != "a.txt" {
		t.Fatalf("去重后应 2 个文件：%v", files)
	}
}

func TestPackPathsReadPolicy(t *testing.T) {
	root := writeTree(t, map[string]string{"in.txt": "ok"})
	outside := writeTree(t, map[string]string{"secret.txt": "s3cret"})
	fsp := tools.NewFsPolicy(root, false, true, nil) // read-workspace：读边界开
	var buf bytes.Buffer
	if _, err := PackPaths(fsp, root, []string{"in.txt"}, &buf); err != nil {
		t.Fatalf("区内文件应可打包：%v", err)
	}
	if _, err := PackPaths(fsp, root, []string{filepath.Join(outside, "secret.txt")}, &buf); err == nil {
		t.Fatal("区外路径应被 CheckRead 拒绝")
	}
}

func TestUnpackRejectsTraversal(t *testing.T) {
	build := func(entries []tar.Header) *bytes.Buffer {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		for _, h := range entries {
			h.Size = 0
			if err := tw.WriteHeader(&h); err != nil {
				t.Fatal(err)
			}
		}
		tw.Close()
		gz.Close()
		return &buf
	}
	dest := t.TempDir()
	cases := map[string]tar.Header{
		"absolute":      {Typeflag: tar.TypeReg, Name: "/etc/pwned"},
		"dotdot":        {Typeflag: tar.TypeReg, Name: "../escape.txt"},
		"dotdot-nested": {Typeflag: tar.TypeReg, Name: "a/../../escape.txt"},
		"backslash":     {Typeflag: tar.TypeReg, Name: `a\..\escape.txt`},
		"symlink":       {Typeflag: tar.TypeSymlink, Name: "link", Linkname: "/etc/passwd"},
		"hardlink":      {Typeflag: tar.TypeLink, Name: "hard", Linkname: "/etc/passwd"},
	}
	for name, hdr := range cases {
		if _, err := UnpackTo(build([]tar.Header{hdr}), dest); err == nil {
			t.Fatalf("%s 条目应被拒绝", name)
		}
	}
	if _, err := os.Stat(filepath.Join(dest, "escape.txt")); !os.IsNotExist(err) {
		t.Fatal("穿越文件不应落地")
	}
}

func TestUnpackEmptyAndGarbage(t *testing.T) {
	dest := t.TempDir()
	// 非 gzip 流
	if _, err := UnpackTo(bytes.NewReader([]byte("not gzip")), dest); err == nil {
		t.Fatal("垃圾流应报错")
	}
	// 只有目录条目、无常规文件
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "only-dir"})
	tw.Close()
	gz.Close()
	if _, err := UnpackTo(&buf, dest); err == nil {
		t.Fatal("无常规文件应报错")
	}
}

func TestUnpackSizeCap(t *testing.T) {
	// 头声明超 unpackMaxBytes 的单文件（流被 tar.Reader 截断前按头拒）
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "bomb.txt", Size: unpackMaxBytes + 1})
	tw.Close()
	gz.Close()
	if _, err := UnpackTo(&buf, t.TempDir()); err == nil {
		t.Fatal("超总量应拒绝")
	}
}
