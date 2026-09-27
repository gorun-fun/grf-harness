// update_cmd.go gh update 子命令：探测下载服务器上的最新版本并自更新。
// 探测逻辑在 selfupdate 包（与 grf-web 下载页同一套目录约定）。
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/selfupdate"
)

// runUpdateCmd gh update [-y]：检查更新，确认后下载替换当前可执行文件。
func runUpdateCmd(args []string) {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	yes := fs.Bool("y", false, "跳过确认直接更新")
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "用法: gh update [-y]")
		os.Exit(2)
	}

	cur := config.Version
	if cur == "dev" {
		fatal(fmt.Errorf("本地构建（dev）没有版本号，无法比对更新；请用 ./run.sh 构建或从下载页安装正式版"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	rel, err := selfupdate.Check(ctx, "")
	cancel()
	if err != nil {
		fatal(fmt.Errorf("检查更新失败: %w", err))
	}
	if rel == nil || selfupdate.CompareVersions(rel.Version, cur) <= 0 {
		fmt.Printf("已是最新版本 %s\n", cur)
		return
	}

	fmt.Printf("发现新版本: %s → %s\n", cur, rel.Version)
	if !*yes && !confirmUpdate() {
		fmt.Println("已取消更新")
		return
	}

	exe, err := selfupdate.CurrentExe()
	if err != nil {
		fatal(fmt.Errorf("定位当前可执行文件: %w", err))
	}
	fmt.Println() // 确认行收尾，进度 \r 覆盖单独一行
	if err := selfupdate.Install(context.Background(), rel.URL, exe, printProgress); err != nil {
		fatal(fmt.Errorf("更新失败: %w", err))
	}
	fmt.Printf("\n已更新到 %s（重启 gh 生效）\n", rel.Version)
}

// confirmUpdate 终端读一行确认；空/其他输入一律不更新。
func confirmUpdate() bool {
	fmt.Print("是否下载并更新? [y/N] ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return false // EOF（管道/脚本）视为拒绝
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes", "是":
		return true
	}
	return false
}

// printProgress 单行覆盖式下载进度（自更新，无文件名前缀）。
func printProgress(done, total int64) { downloadProgress("", done, total) }

// downloadProgress 单行覆盖式下载进度（file 非空时作前缀；total 未知时
// 只显示已下载量）。gh update 与 gh plugins download 共用。
func downloadProgress(file string, done, total int64) {
	pre := ""
	if file != "" {
		pre = file + " "
	}
	switch {
	case total > 0:
		fmt.Printf("\r%s下载中 %.1f/%.1f MB   ", pre, float64(done)/(1<<20), float64(total)/(1<<20))
	default:
		fmt.Printf("\r%s下载中 %.1f MB   ", pre, float64(done)/(1<<20))
	}
}
