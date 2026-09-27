package weixin

import (
	"encoding/json"
	"os"
	"path/filepath"

	"git.gorun.fun/share/grf-harness/config"
)

// State iLink 连接状态,落盘保存(含 token 与长轮询游标,勿泄露)。
// 保存位置:~/.grf/weixin_state.json(GRF_HOME 可重定位)。
type State struct {
	BotToken      string `json:"bot_token"`
	BaseURL       string `json:"baseurl"`
	IlinkBotID    string `json:"ilink_bot_id"`
	IlinkUserID   string `json:"ilink_user_id"`
	GetUpdatesBuf string `json:"get_updates_buf"`
}

func stateFile() string {
	return filepath.Join(config.Home(), "weixin_state.json")
}

func LoadState() *State {
	st := &State{}
	bs, err := os.ReadFile(stateFile())
	if err == nil {
		_ = json.Unmarshal(bs, st)
	}
	return st
}

func SaveState(st *State) error {
	if err := os.MkdirAll(config.Home(), 0o755); err != nil {
		return err
	}
	bs, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(stateFile(), bs, 0o600)
}

// HasSavedLogin 是否存在可复用的 ClawBot 登录态。
func HasSavedLogin() bool { return LoadState().BotToken != "" }

// ResetState 清空登录态(新建 ClawBot 用):丢弃旧 token 与游标。
func ResetState() error {
	return SaveState(&State{})
}
