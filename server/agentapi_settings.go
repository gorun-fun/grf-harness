// agentapi_settings.go — /v1/agent/* 共用的 settings.json 原始读写与
// 路径/可写探测：以通用 map 整读整写,未知键原样保留(其余域的 handlers
// 都经这里落盘)。路径与原子落盘复用 config 包公共件,勿再各写一份。
package server

import (
	"encoding/json"
	"os"
	"path/filepath"

	"git.gorun.fun/share/grf-harness/config"
)

// readSettingsRaw 读 settings.json 为通用 map(损坏按空文档处理);不存在
// 返回空 map。其余未知键靠它原样保留。
func readSettingsRaw() (map[string]json.RawMessage, error) {
	all := map[string]json.RawMessage{}
	raw, err := os.ReadFile(config.Path())
	if err != nil {
		if os.IsNotExist(err) {
			return all, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(raw, &all); err != nil {
		return map[string]json.RawMessage{}, nil // 损坏按空文档,由本次写入修复
	}
	return all, nil
}

// writeSettingsRaw 写回 settings.json(保留未知键;0600,原子替换)。
func writeSettingsRaw(all map[string]json.RawMessage) error {
	path := config.Path()
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(path, append(data, '\n'), 0o600)
}

// settingsWritable 目标可写探测(文件或目录;不存在时建临时探针)。
func settingsWritable(path string) bool {
	if f, err := os.OpenFile(path, os.O_WRONLY, 0o600); err == nil {
		f.Close()
		return true
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	f, err := os.CreateTemp(dir, ".grf-write-test")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	_ = os.Remove(name)
	return true
}
