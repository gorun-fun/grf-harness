// artifacts.go 工具超长结果的落盘位置（glob/grep/bash 的完整输出），
// 按会话建临时目录，路径报告给模型。
package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
)

type Artifacts struct {
	dir string
	seq atomic.Int64
}

// NewArtifacts 创建会话产物目录（os.TempDir 下）。
func NewArtifacts() (*Artifacts, error) {
	dir, err := os.MkdirTemp("", "grf-artifacts-*")
	if err != nil {
		return nil, err
	}
	return &Artifacts{dir: dir}, nil
}

// Save 写入一段文本，返回文件路径。
func (a *Artifacts) Save(prefix, content string) (string, error) {
	if a == nil {
		return "", fmt.Errorf("artifacts store unavailable")
	}
	name := fmt.Sprintf("%s-%d.txt", prefix, a.seq.Add(1))
	path := filepath.Join(a.dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// Cleanup 删除产物目录（会话结束时调用，尽力而为）。
func (a *Artifacts) Cleanup() {
	if a != nil && a.dir != "" {
		_ = os.RemoveAll(a.dir)
	}
}

// Dir 产物目录路径。
func (a *Artifacts) Dir() string { return a.dir }
