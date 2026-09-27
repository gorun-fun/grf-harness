// atomic.go 原子落盘公共件：settings.json（含 api_key）、会话存档、
// memory.json 等 JSON 状态文件共用——直接 os.WriteFile 在写中途崩溃会留
// 半截文件，损坏的配置会令整个客户端失效。
package config

import (
	"os"
	"path/filepath"
)

// WriteFileAtomic 原子写文件：同目录临时文件写全量内容后 rename 替换
// （同分区 rename 原子；Windows 上 Go 的 rename 语义为替换已存在目标，
// 正被占用运行的可执行文件不适用本函数，见 selfupdate.Install 的两步换）。
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".new-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// 失败路径清理；成功后 tmpName 已被 rename 走，Remove 报错忽略
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
