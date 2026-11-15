// sync.go 工作区同步档位（settings cluster.sync）：seed/git/full 共用
// M2 的打包-推送-解包通道，档位只决定"派发时打包什么"。seed = 模型经
// seed_files 显式点名；git/full = 未显式点名时自动打包（git 跟踪文件 /
// 全工作区，跳过 .git），让远端节点天然拿到本机上下文、产物回传闭环，
// 模型无需逐次点名。
package cluster

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// SyncMode 归一化同步档位：返回 ""（none）| seed | git | full；未知取值
// 按 none 处理（保守：不打包，任务自包含）。
func SyncMode(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "seed":
		return "seed"
	case "git":
		return "git"
	case "full":
		return "full"
	}
	return ""
}

// GitTrackedFiles 工作区 git 跟踪文件清单（相对工作区，含未提交改动的工作
// 树文件，不含未跟踪/忽略文件）。非 git 仓库/无 git 可执行时报错（调用方
// 转成派发失败——sync=git 是显式契约，静默降级会让远端跑错上下文）。
func GitTrackedFiles(ws string) ([]string, error) {
	cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "git", "-C", ws, "ls-files", "-z").Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("cluster sync git: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("cluster sync git: %w (is the workspace a git repository?)", err)
	}
	var files []string
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("cluster sync git: no tracked files under %q", ws)
	}
	return files, nil
}

// AllWorkspaceFiles 全工作区常规文件清单（相对工作区；跳过 .git 目录——
// 对象库动辄成百 MB 且对远端无意义）。上限与打包侧一致，超限由 PackPaths
// 终检报错。
func AllWorkspaceFiles(ws string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(ws, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, ok := relInside(ws, path)
		if !ok {
			return nil // 越出根（符号链接等）：跳过，WalkDir 本身不跟链接
		}
		if d.IsDir() {
			if rel == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("cluster sync full: %w", err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("cluster sync full: workspace %q has no regular files", ws)
	}
	return files, nil
}
