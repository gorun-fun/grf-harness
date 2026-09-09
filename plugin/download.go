// download.go 官方插件下载：从发布服务器（与自更新同一台 nginx，目录布局
// {base}/{平台}/{版本}/plugins/grf-*}，由 CI publish job 产出）探测当前
// 平台可下载的插件并落到自动发现目录 ~/.grf/plugins/，重启 gh 即接入。
// 目录列表与下载替换复用 selfupdate 的通用件（ListDir/Install），基址与
// GRF_DOWNLOAD_BASE 覆盖亦同源。
package plugin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/selfupdate"
)

// PluginRelease 一个版本目录下可下载的插件集合。
type PluginRelease struct {
	Base    string   // 下载基址（结尾无斜杠）
	Dir     string   // 平台目录名（如 linux-arm64）
	Version string   // 版本号（去 v 前缀）
	Files   []string // 插件文件名（windows 含 .exe 后缀）
}

// URL 单个插件文件的下载地址；file 为空时返回 plugins/ 目录本身的列表页。
func (r *PluginRelease) URL(file string) string {
	return r.Base + "/" + r.Dir + "/" + r.Version + "/plugins/" + file
}

// ProbePlugins 探测当前平台的插件发布。version 非空时精确取该版本（无产物
// 报错，方便排查发布缺漏）；为空时优先取与主程序配套的版本（config.Version，
// 插件与主程序同 tag 发布），无产物再按 semver 降序取最新一个含 plugins/
// 目录的版本。返回 nil 表示服务器上当前平台没有插件产物（非错误）。
// base 传空用 selfupdate.Base()。
func ProbePlugins(ctx context.Context, base, version string) (*PluginRelease, error) {
	if base == "" {
		base = selfupdate.Base()
	}
	dir, ok := selfupdate.PlatformDir(runtime.GOOS, runtime.GOARCH)
	if !ok {
		return nil, fmt.Errorf("当前平台 %s/%s 没有发布产物", runtime.GOOS, runtime.GOARCH)
	}
	rel := &PluginRelease{Base: base, Dir: dir}

	if version != "" {
		rel.Version = strings.TrimPrefix(version, "v")
		files, err := selfupdate.ListDir(ctx, rel.URL(""))
		if errors.Is(err, selfupdate.ErrNotFound) {
			return nil, fmt.Errorf("版本 %s 没有插件产物", rel.Version)
		}
		if err != nil {
			return nil, err
		}
		if len(files) == 0 {
			return nil, fmt.Errorf("版本 %s 没有插件产物", rel.Version)
		}
		rel.Files = files
		return rel, nil
	}

	versions, err := selfupdate.ListDir(ctx, base+"/"+dir+"/")
	if errors.Is(err, selfupdate.ErrNotFound) {
		return nil, nil // 平台目录都没有：服务器无当前平台产物
	}
	if err != nil {
		return nil, err
	}
	sort.Slice(versions, func(i, j int) bool {
		return selfupdate.CompareVersions(versions[i], versions[j]) > 0
	})
	// 候选：主程序当前版本排最前（与主程序配套最稳），其余按新到旧
	candidates := versions
	if v := config.Version; v != "" && v != "dev" {
		candidates = append([]string{v}, versions...)
	}
	for _, v := range candidates {
		files, err := selfupdate.ListDir(ctx, base+"/"+dir+"/"+v+"/plugins/")
		if err != nil || len(files) == 0 {
			continue // 单个版本异常/无产物不挡后续版本
		}
		rel.Version, rel.Files = v, files
		return rel, nil
	}
	return nil, nil
}

// Download 把插件下载到自动发现目录 PluginDir()（已存在则原子覆盖）。
// names 为空下载全部；名称匹配宽松——"browser"、"grf-browser"、
// "grf-browser.exe" 视为同一个。progress 可为 nil（file 为远端文件名）。
// 返回落盘路径清单（含部分成功：出错时已完成的仍在其中）。
func (r *PluginRelease) Download(ctx context.Context, names []string, progress func(file string, done, total int64)) ([]string, error) {
	files, err := r.pick(names)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(PluginDir(), 0o755); err != nil {
		return nil, err
	}
	if progress == nil {
		progress = func(string, int64, int64) {}
	}
	var saved []string
	for _, f := range files {
		dst := filepath.Join(PluginDir(), f)
		wrap := func(done, total int64) { progress(f, done, total) }
		if err := selfupdate.Install(ctx, r.URL(f), dst, wrap); err != nil {
			return saved, fmt.Errorf("下载 %s 失败: %w", f, err)
		}
		saved = append(saved, dst)
	}
	return saved, nil
}

// pick 按用户参数挑选待下载文件（空 = 全部），未知名称报错并列出可用项。
func (r *PluginRelease) pick(names []string) ([]string, error) {
	if len(names) == 0 {
		return append([]string(nil), r.Files...), nil
	}
	var out []string
	for _, want := range names {
		found := ""
		for _, f := range r.Files {
			if normPluginName(f) == normPluginName(want) {
				found = f
				break
			}
		}
		if found == "" {
			return nil, fmt.Errorf("没有名为 %q 的插件（可下载: %s）", want, strings.Join(r.Files, "、"))
		}
		out = append(out, found)
	}
	return out, nil
}

// normPluginName 名称归一：去 grf- 前缀与 .exe 后缀，宽松匹配用户输入。
func normPluginName(s string) string {
	s = strings.TrimPrefix(s, "grf-")
	return strings.TrimSuffix(s, ".exe")
}
