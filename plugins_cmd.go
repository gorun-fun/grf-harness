// plugins_cmd.go gh plugins：插件体检——列出配置/自动发现的 MCP 插件、
// 连接状态与工具清单。独立实例直连（不走进程单例），输出后立即关闭。
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"

	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/plugin"
)

func runPluginsCmd(args []string) {
	fs := flag.NewFlagSet("plugins", flag.ExitOnError)
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "未知参数: %v\n用法: gh plugins\n", fs.Args())
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
		fmt.Printf("没有插件。\n\n自动发现目录: %s（放入可执行文件即接入）\n或在 %s 配置 \"plugins\"（本地 command / 远程 url，见 docs/plugins.md）\n",
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
