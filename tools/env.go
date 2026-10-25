// env.go 子进程环境构造：bash 工具与 MCP 插件共用同一套"剔除疑似密钥"
// 规则（mcp 包有同规则副本，见 mcp/stdio.go）。模型可以令子进程打印
// 环境（env / printenv），父进程环境里的 LLM 供应商密钥（OPENAI_API_KEY
// 等）与工具链自身秘密（GRF_TURN_WEBHOOK_SECRET 签名密钥等）不应顺带
// 暴露；被剔除变量确有需要的工作流，在命令内自行 export 即可。
package tools

import (
	"os"
	"regexp"
	"strings"
)

// secretEnvRe 疑似密钥变量名的宽松匹配（大小写不敏感）：API key / token /
// secret / password / credential 命名族。
var secretEnvRe = regexp.MustCompile(`(?i)(api[_-]?key|apikey|secret|token|password|passwd|credential)`)

// secretishEnv 变量名是否疑似密钥。
func secretishEnv(name string) bool {
	return secretEnvRe.MatchString(name)
}

// ChildEnv 构造子进程环境：父进程环境剔除疑似密钥变量后，追加 extra
// （harness 显式注入的事实变量，如 DSH_* 工作区事实）。
func ChildEnv(extra []string) []string {
	env := os.Environ()
	out := make([]string, 0, len(env)+len(extra))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 && secretishEnv(kv[:i]) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, extra...)
}
