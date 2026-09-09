// Package selfupdate 自更新：从下载服务器的 nginx 目录列表探测当前平台的
// 最新版本，并下载替换正在运行的可执行文件。探测逻辑与 grf-web 的
// lib/downloads.ts 保持同一套约定：目录布局 {base}/{平台}/{版本}/gh(.exe)，
// 版本按 semver 风格降序取第一个含产物文件的版本。
//
// 目录列表（ListDir/ErrNotFound）与下载替换（Install）是通用件，官方插件
// 下载（plugin 包的 gh plugins download）复用同一套服务器约定。
//
// 下载基址默认 https://download.gorun.fun/grf-harness，可用环境变量
// GRF_DOWNLOAD_BASE 覆盖（私有部署/调试）。
package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// DefaultBase 下载服务器基址（与 CI 发布目录 /data/download/grf-harness 对应）。
const DefaultBase = "https://download.gorun.fun/grf-harness"

// httpClient 探测与下载共用：不设整体 Timeout，超时全由请求级 ctx 控制
// （探测短超时、下载大文件长超时，二者差异大不适合共用 client.Timeout）。
var httpClient = &http.Client{}

// ErrNotFound 目录 404 的哨兵：调用方视为"无产物"而非网络故障
// （插件下载等复用方据此区分"该版本没有产物"与"服务器不可达"）。
var ErrNotFound = errors.New("404 not found")

var dirHrefRe = regexp.MustCompile(`<a href="([^"]+)"`)

// Base 下载基址：GRF_DOWNLOAD_BASE > 默认值。
func Base() string {
	if v := os.Getenv("GRF_DOWNLOAD_BASE"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return DefaultBase
}

// Release 探测到的可下载版本。
type Release struct {
	Version string
	URL     string
}

// PlatformDir goos/goarch 到发布目录名的映射（与 .gitlab-ci.yml 的六平台一致）。
func PlatformDir(goos, goarch string) (string, bool) {
	switch goos {
	case "darwin":
		switch goarch {
		case "arm64":
			return "macos-arm64", true
		case "amd64":
			return "macos", true
		}
	case "linux":
		switch goarch {
		case "arm64":
			return "linux-arm64", true
		case "amd64":
			return "linux", true
		}
	case "windows":
		switch goarch {
		case "arm64":
			return "windows-arm64", true
		case "amd64":
			return "windows", true
		}
	}
	return "", false
}

// BinaryName 当前平台的产物文件名。
func BinaryName(goos string) string {
	if goos == "windows" {
		return "gh.exe"
	}
	return "gh"
}

// Check 探测当前平台的最新可用版本。返回 nil 表示服务器上没有可用产物
// （目录缺失/404 等，非错误）；err 非 nil 为网络故障。base 传空用 Base()。
func Check(ctx context.Context, base string) (*Release, error) {
	if base == "" {
		base = Base()
	}
	dir, ok := PlatformDir(runtime.GOOS, runtime.GOARCH)
	if !ok {
		return nil, fmt.Errorf("当前平台 %s/%s 没有发布产物", runtime.GOOS, runtime.GOARCH)
	}
	root, err := ListDir(ctx, base+"/")
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !contains(root, dir) {
		return nil, nil
	}
	versions, err := ListDir(ctx, base+"/"+dir+"/")
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sort.Slice(versions, func(i, j int) bool {
		return CompareVersions(versions[i], versions[j]) > 0
	})
	binary := BinaryName(runtime.GOOS)
	for _, v := range versions {
		files, err := ListDir(ctx, base+"/"+dir+"/"+v+"/")
		if err != nil {
			continue // 单个版本目录异常不挡后续版本
		}
		if contains(files, binary) {
			return &Release{
				Version: v,
				URL:     base + "/" + dir + "/" + v + "/" + binary,
			}, nil
		}
	}
	return nil, nil
}

// ListDir 抓取 nginx autoindex 目录页，提取去尾斜杠后的条目名
// （过滤分页参数、绝对链接与上跳目录）。目录缺失返回 ErrNotFound。
func ListDir(ctx context.Context, url string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var entries []string
	for _, m := range dirHrefRe.FindAllStringSubmatch(string(data), -1) {
		e := strings.TrimSuffix(m[1], "/")
		if e == "" || e == ".." || strings.HasPrefix(e, "?") || strings.HasPrefix(e, "/") {
			continue
		}
		entries = append(entries, e)
	}
	return entries, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// CompareVersions semver 风格比较：a>b 返回 1、相等 0、否则 -1。
// 升序示例：1.0.0-alpha.2 < 1.0.0-alpha.6 < 1.0.0-rc.1 < 1.0.0 < 1.1.0。
func CompareVersions(a, b string) int {
	ca, pa := splitVersion(a)
	cb, pb := splitVersion(b)
	n := len(ca)
	if len(cb) > n {
		n = len(cb)
	}
	for i := 0; i < n; i++ {
		var x, y int
		if i < len(ca) {
			x = ca[i]
		}
		if i < len(cb) {
			y = cb[i]
		}
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	// 正式版 > 预发布
	if len(pa) == 0 && len(pb) > 0 {
		return 1
	}
	if len(pa) > 0 && len(pb) == 0 {
		return -1
	}
	m := len(pa)
	if len(pb) > m {
		m = len(pb)
	}
	for i := 0; i < m; i++ {
		if i >= len(pa) {
			return -1 // 标识符少的小：alpha < alpha.1
		}
		if i >= len(pb) {
			return 1
		}
		ta, tb := pa[i], pb[i]
		na, aNum := parseIdent(ta)
		nb, bNum := parseIdent(tb)
		switch {
		case aNum && bNum:
			if na != nb {
				if na > nb {
					return 1
				}
				return -1
			}
		case aNum != bNum:
			if aNum {
				return -1 // 数字标识符排序低于字母（semver 规则）
			}
			return 1
		case ta != tb:
			if ta > tb {
				return 1
			}
			return -1
		}
	}
	return 0
}

// splitVersion 拆出主版本数字段与预发布标识符段（"1.0.0-rc.1" → [1,0,0], ["rc","1"]）。
func splitVersion(v string) (core []int, pre []string) {
	coreStr, preStr := v, ""
	if i := strings.IndexByte(v, '-'); i >= 0 {
		coreStr, preStr = v[:i], v[i+1:]
	}
	for _, p := range strings.Split(coreStr, ".") {
		n, _ := strconv.Atoi(p) // 非数字段按 0 参与 core 比较
		core = append(core, n)
	}
	if preStr != "" {
		pre = strings.Split(preStr, ".")
	}
	return core, pre
}

// parseIdent 严格数字标识符（"6"→6,true；"alpha"、"6x"→0,false）。
func parseIdent(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

// CurrentExe 当前可执行文件的规范路径（解析符号链接）。
func CurrentExe() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		return real, nil
	}
	return exe, nil
}

// Install 下载 url 指向的新版本并替换 exePath（通常为 CurrentExe 的结果）。
// progress 可为 nil，周期性回报下载进度（done 字节，total 为 Content-Length，
// 服务器未提供时为 -1）。临时文件写在 exePath 同目录，rename 原子替换；
// Windows 上运行中的 exe 不能被覆盖、但可以被改名，故先挪旧再放新。
func Install(ctx context.Context, url, exePath string, progress func(done, total int64)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}

	// 权限沿用旧文件（兜底 0755），避免下载产物丢失可执行位
	mode := os.FileMode(0o755)
	if st, err := os.Stat(exePath); err == nil {
		mode = st.Mode().Perm()
	}

	// 同目录临时文件：与目标同分区，rename 才是原子操作
	tmp, err := os.CreateTemp(filepath.Dir(exePath), "."+filepath.Base(exePath)+".new-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// 失败路径清理；成功后 tmpName 已被 rename 走，Remove 报错忽略
	defer os.Remove(tmpName)

	total := resp.ContentLength
	var done int64
	buf := make([]byte, 64<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := tmp.Write(buf[:n]); werr != nil {
				tmp.Close()
				return werr
			}
			done += int64(n)
			if progress != nil {
				progress(done, total)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			tmp.Close()
			return rerr
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}

	if runtime.GOOS == "windows" {
		old := exePath + ".old"
		os.Remove(old) // 上次更新可能残留
		if err := os.Rename(exePath, old); err != nil {
			return err
		}
		if err := os.Rename(tmpName, exePath); err != nil {
			os.Rename(old, exePath) // 尽力回滚，保住旧版本可用
			return err
		}
		os.Remove(old) // 运行中的旧文件可能删不掉，留给下次
		return nil
	}
	return os.Rename(tmpName, exePath)
}
