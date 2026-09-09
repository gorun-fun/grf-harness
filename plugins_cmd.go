// plugins_cmd.go gh plugins：插件体检——列出配置/自动发现的 MCP 插件、
// 连接状态与工具清单。独立实例直连（不走进程单例），输出后立即关闭。
// 子命令 download 从官方下载服务器拉取插件二进制（探测逻辑在
// plugin/download.go，目录约定与 selfupdate 同源）。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/plugin"
	"git.gorun.fun/share/grf-harness/selfupdate"
)

func runPluginsCmd(args []string) {
	if len(args) > 0 && args[0] == "download" {
		runPluginsDownloadCmd(args[1:])
		return
	}
	fs := flag.NewFlagSet("plugins", flag.ExitOnError)
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "未知参数: %v\n用法: gh plugins [download [名称...] [-version 版本]]\n", fs.Args())
		os.Exit(1)
	}
	st, err := config.LoadFileForUpdate()
	if err != nil {
		fatal(err)
	}
	cfgPath, _ := config.Path()
	m := plugin.New()
	defer m.Close()
	m.Load(st)

	conns := m.Conns()
	if len(conns) == 0 {
		fmt.Printf("没有插件。\n\n自动发现目录: %s（放入可执行文件即接入，gh plugins download 可从官方下载）\n或在 %s 配置 \"plugins\"（本地 command / 远程 url，见 docs/plugins.md）\n",
			plugin.PluginDir(), cfgPath)
		return
	}
	fmt.Printf("插件 %d 个（自动发现目录: %s）：\n", len(conns), plugin.PluginDir())
	byPlugin := map[string][]string{}
	for name, info := range m.Tools() {
		byPlugin[info.Plugin] = append(byPlugin[info.Plugin], name)
	}
	for _, c := range conns {
		status := "已连接"
		if err := c.DeadErr(); err != nil {
			status = "不可用"
		}
		fmt.Printf("\n[%s] %s  %s: %s\n", status, c.Name, c.Mode, c.Where)
		if err := c.DeadErr(); err != nil {
			fmt.Printf("    错误: %v\n", err)
		}
		if si := c.ServerInfo(); si.Name != "" {
			fmt.Printf("    服务: %s %s\n", si.Name, si.Version)
		}
		names := byPlugin[c.Name]
		sort.Strings(names)
		fmt.Printf("    工具 %d 个:\n", len(names))
		for _, n := range names {
			if info, ok := m.Tools()[n]; ok && info.ReadOnly {
				fmt.Printf("    - %s（只读）\n", n)
			} else {
				fmt.Printf("    - %s\n", n)
			}
		}
	}
	if warns := m.Warnings(); len(warns) > 0 {
		fmt.Println("\n警告:")
		for _, w := range warns {
			fmt.Println("  - " + w)
		}
	}
}

// runPluginsDownloadCmd gh plugins download [名称...] [-version V]：探测
// 官方下载服务器上当前平台的插件（缺省优先与主程序同版本，其次最新），
// 下载到自动发现目录 ~/.grf/plugins/，重启 gh 生效。名称可省略 grf-
// 前缀；不指定名称则下载全部（标志可写在名称前后任意位置）。
func runPluginsDownloadCmd(args []string) {
	fs := flag.NewFlagSet("plugins download", flag.ExitOnError)
	version := fs.String("version", "", "插件版本（缺省优先与主程序同版本，其次最新）")
	_ = fs.Parse(reorderFlags(args, "version"))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	rel, err := plugin.ProbePlugins(ctx, "", *version)
	cancel()
	if err != nil {
		fatal(err)
	}
	if rel == nil {
		fmt.Printf("下载服务器（%s）上没有当前平台（%s/%s）的插件产物。\n", selfupdate.Base(), runtime.GOOS, runtime.GOARCH)
		return
	}
	fmt.Printf("插件版本 %s：%s\n", rel.Version, strings.Join(rel.Files, "、"))

	saved, err := rel.Download(context.Background(), fs.Args(), func(file string, done, total int64) {
		switch {
		case total > 0:
			fmt.Printf("\r%s 下载中 %.1f/%.1f MB   ", file, float64(done)/(1<<20), float64(total)/(1<<20))
		default:
			fmt.Printf("\r%s 下载中 %.1f MB   ", file, float64(done)/(1<<20))
		}
	})
	fmt.Println()
	if err != nil {
		fatal(err)
	}
	for _, p := range saved {
		fmt.Printf("已安装: %s\n", p)
	}
	if st, err := config.LoadFileForUpdate(); err == nil && st.Plugins != nil {
		fmt.Println("注意: settings.json 配置了 plugins 键（自动发现已关闭），下载的插件不会被接入；删除该键或将其加入配置。")
	}
	fmt.Println("重启 gh 生效；gh plugins 可体检连接状态。")
}

// reorderFlags 把指定标志挪到位置参数之前再交给 flag.Parse——标准库 flag
// 遇到首个位置参数即停止解析，而 "download browser -version v1" 是自然写法。
// 支持两种形态：-f 值 与 -f=值（单连字符/双连字符均可）。
func reorderFlags(args []string, flags ...string) []string {
	var head, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		matched := false
		for _, f := range flags {
			for _, dash := range []string{"-", "--"} {
				name := dash + f
				if a == name { // -f 值
					if i+1 < len(args) {
						head = append(head, a, args[i+1])
						i++
					} else {
						head = append(head, a)
					}
					matched = true
				} else if strings.HasPrefix(a, name+"=") { // -f=值
					head = append(head, a)
					matched = true
				}
				if matched {
					break
				}
			}
			if matched {
				break
			}
		}
		if !matched {
			rest = append(rest, a)
		}
	}
	return append(head, rest...)
}
