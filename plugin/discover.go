// discover.go 插件自动发现与配置项归一：~/.grf/plugins 下的可执行文件
// 每个视为一个本地 stdio 插件（项目级 .grf/plugins 不自动执行，避免
// 仓库内二进制未经用户确认即运行）。
package plugin

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/tools"
)

// PluginDir 插件自动发现目录（GRF_HOME 可重定位）。
func PluginDir() string { return filepath.Join(config.Home(), "plugins") }

// discoverPlugins 扫描发现目录：常规可执行文件 + 非隐藏 + 非文档
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
		if it.IsDir() || strings.HasPrefix(name, ".") || name == "README.md" || strings.HasSuffix(name, ".md") || IsStaleOldName(name) {
			continue
		}
		full := filepath.Join(dir, name)
		info, err := it.Info()
		if err != nil || !IsExecutableFile(name, info.Mode()) {
			continue
		}
		out = append(out, config.Plugin{Name: name, Command: []string{full}})
	}
	return out
}

// IsExecutableFile 判断文件是否视为可执行插件。Windows 没有 POSIX
// 可执行位：Go 把普通文件统一报成 0666（只读 0444），os.Chmod 也只切换
// 只读属性，按位过滤恒为空——改按可执行后缀判定（.exe/.bat/.cmd/.com/
// .ps1）。goder 的 internal/desktop/agentconfig.go 有同规则副本，两边
// 必须同步改。
func IsExecutableFile(name string, mode os.FileMode) bool {
	if runtime.GOOS == "windows" {
		switch strings.ToLower(filepath.Ext(name)) {
		case ".exe", ".bat", ".cmd", ".com", ".ps1":
			return true
		}
		return false
	}
	return mode.IsRegular() && mode.Perm()&0o111 != 0
}

// IsStaleOldName 更新落盘挪出的旧副本名（x.old / x.old.2…）：仍被占用
// 没清掉时会短暂留在目录里，不应被当作插件。
func IsStaleOldName(name string) bool {
	return strings.HasSuffix(name, ".old") || strings.Contains(name, ".old.")
}

// specHasTool 名字是否与内置 28 个工具重名。
func specHasTool(name string) bool {
	_, ok := tools.SpecDef(name)
	return ok
}
