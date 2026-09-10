// discover.go 插件自动发现与配置项归一：~/.grf/plugins 下的可执行文件
// 每个视为一个本地 stdio 插件（项目级 .grf/plugins 不自动执行，避免
// 仓库内二进制未经用户确认即运行）。
package plugin

import (
	"os"
	"path/filepath"
	"strings"

	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/tools"
)

// PluginDir 插件自动发现目录（GRF_HOME 可重定位）。
func PluginDir() string { return filepath.Join(config.Home(), "plugins") }

// discoverPlugins 扫描发现目录：常规文件 + 可执行位 + 非隐藏 + 非文档
// （README 等）。目录不存在返回空。
func discoverPlugins() []config.Plugin {
	dir := PluginDir()
	items, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []config.Plugin
	for _, it := range items {
		name := it.Name()
		if it.IsDir() || strings.HasPrefix(name, ".") || name == "README.md" || strings.HasSuffix(name, ".md") {
			continue
		}
		full := filepath.Join(dir, name)
		info, err := it.Info()
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		out = append(out, config.Plugin{Name: name, Command: []string{full}})
	}
	return out
}

// specHasTool 名字是否与内置 28 个工具重名。
func specHasTool(name string) bool {
	_, ok := tools.SpecDef(name)
	return ok
}
