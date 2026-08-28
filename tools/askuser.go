// askuser.go ask_user_question 工具：向最终用户提问（Host.AskUser）。
// 根会话注册可交互版本；子代理注册拒绝版（不可达最终用户）。deferred
// 宿主（tools.DeferredHost）下本工具转入回合暂停协议（见 agent/pause.go），
// 不会真正执行到这里。
package tools

import (
	"context"
	"fmt"
	"strings"
)

// RegisterAskUser 注册 ask_user_question。host 为宿主 getter（TUI 会在
// agent 构建后才接管交互，运行期取当前宿主而非注册时的快照）。
func RegisterAskUser(r *Registry, host func() Host) {
	r.Register("ask_user_question", func(ctx context.Context, args string) (Result, error) {
		var a struct {
			Questions []Question `json:"questions"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return Result{}, err
		}
		if len(a.Questions) == 0 {
			return Result{}, fmt.Errorf("questions array is required")
		}
		if len(a.Questions) > 4 {
			a.Questions = a.Questions[:4]
		}
		for i, q := range a.Questions {
			if strings.TrimSpace(q.ID) == "" || strings.TrimSpace(q.Question) == "" {
				return Result{}, fmt.Errorf("question %d: id and question are both required", i)
			}
			if len(q.Options) > 4 {
				a.Questions[i].Options = q.Options[:4]
			}
		}
		h := host()
		if h == nil {
			return Result{}, fmt.Errorf("no interactive user is available in this session; proceed without asking and state assumptions")
		}
		answers, err := h.AskUser(ctx, a.Questions)
		if err != nil {
			return Result{}, fmt.Errorf("the user could not answer: %v", err)
		}
		out, err := MarshalAnswers(answers)
		if err != nil {
			return Result{}, err
		}
		return TextResult(out), nil
	})
}

// RegisterAskUserDenied 子代理版本：明确拒绝（子代理无法触达最终用户）。
func RegisterAskUserDenied(r *Registry) {
	r.Register("ask_user_question", func(ctx context.Context, args string) (Result, error) {
		return Result{}, fmt.Errorf("ask_user_question is only available to the agent talking directly with the human; as a subagent, proceed without asking and state assumptions in your result")
	})
}
