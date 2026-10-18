// taskhost.go 任务模式交互宿主：提问打印到终端、从 stdin 读一行作答；
// 沙箱升级/计划审批按 AutoApprove（或 TTY 下 y/n 询问）。
package app

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"git.gorun.fun/share/grf-harness/term"
	"git.gorun.fun/share/grf-harness/tools"
)

// TaskHost stdin/stdout 交互宿主。
//
// 用 NewTaskHost 构造（自动探测 stdout 是否 TTY）。直接构造零值时视为
// 非 TTY：AskUser 在 stdin 可交互时仍可用，三类审批一律按 AutoApprove
// 处理（false 即静默拒绝）。
type TaskHost struct {
	AutoApprove bool
	ttyOut      bool
}

// NewTaskHost 构建 stdin/stdout 交互宿主，按当前 stdout 是否 TTY 决定
// 审批是否可交互询问。
func NewTaskHost(autoApprove bool) *TaskHost {
	return &TaskHost{AutoApprove: autoApprove, ttyOut: term.IsTTY(int(os.Stdout.Fd()))}
}

func (h *TaskHost) AskUser(ctx context.Context, qs []tools.Question) (map[string]any, error) {
	if !term.IsTTY(int(os.Stdin.Fd())) {
		return nil, fmt.Errorf("stdin is not interactive")
	}
	answers := map[string]any{}
	rd := bufio.NewReader(os.Stdin)
	for _, q := range qs {
		fmt.Println()
		if q.Header != "" {
			fmt.Printf("【%s】\n", q.Header)
		}
		fmt.Println(q.Question)
		if len(q.Options) == 0 {
			fmt.Print("> ")
			line, err := rd.ReadString('\n')
			if err != nil && line == "" {
				return nil, fmt.Errorf("user input ended")
			}
			answers[q.ID] = strings.TrimSpace(line)
			continue
		}
		for i, opt := range q.Options {
			desc := ""
			if opt.Description != "" {
				desc = " — " + opt.Description
			}
			fmt.Printf("  %d. %s%s\n", i+1, opt.Label, desc)
		}
		if q.MultiSelect {
			fmt.Print("多选（逗号分隔序号，可附自定义文字）> ")
		} else {
			fmt.Print("序号或自定义文字 > ")
		}
		line, err := rd.ReadString('\n')
		if err != nil && line == "" {
			return nil, fmt.Errorf("user input ended")
		}
		answers[q.ID] = optionAnswer(q, line)
	}
	return answers, nil
}

// optionAnswer 把选项题的一行输入解析成答案：合法序号映射为选项标签，
// 其余非空内容视作用户自定义文字（多选时两者可混用），空输入默认第一项。
// 序号映射与多选拆分用 tools 公共件（OptionByIndex/SplitOptionParts）。
func optionAnswer(q tools.Question, line string) any {
	if !q.MultiSelect {
		t := strings.TrimSpace(line)
		if l := tools.OptionByIndex(q.Options, t); l != "" {
			return l
		}
		if t != "" {
			return t
		}
		return q.Options[0].Label
	}
	var labels []string
	hasNum := false
	for _, f := range tools.SplitOptionParts(line) {
		if l := tools.OptionByIndex(q.Options, f); l != "" {
			labels = append(labels, l)
			hasNum = true
		} else if t := strings.TrimSpace(f); t != "" {
			labels = append(labels, t)
		}
	}
	if len(labels) == 0 {
		return []string{q.Options[0].Label}
	}
	if !hasNum {
		// 纯自定义回复：整行原样作一条答案，避免被空白拆散
		return []string{strings.TrimSpace(line)}
	}
	return labels
}

func (h *TaskHost) ApproveEscalation(ctx context.Context, tool, key, mode, justification string) tools.EscalationDecision {
	if h.AutoApprove || !h.ttyOut || !term.IsTTY(int(os.Stdin.Fd())) {
		return tools.EscalationDecision{Approved: h.AutoApprove}
	}
	hint := "\n批准? [y]仅此一次  [N]拒绝: "
	if tool == "write" || tool == "edit" {
		hint = "\n批准? [y]仅此一次  [d]批准并授权目录 " + filepath.Dir(key) + "  [N]拒绝: "
	}
	fmt.Printf("\n⚠ 沙箱升级请求 [%s → %s]\n资源: %s\n理由: %s%s", tool, mode, key, justification, hint)
	rd := bufio.NewReader(os.Stdin)
	line, _ := rd.ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	switch {
	case line == "y" || line == "yes":
		return tools.EscalationDecision{Approved: true}
	case (line == "d" || line == "dir") && (tool == "write" || tool == "edit"):
		return tools.EscalationDecision{Approved: true, GrantDir: filepath.Dir(key)}
	default:
		return tools.EscalationDecision{}
	}
}

func (h *TaskHost) ApproveTool(ctx context.Context, name, args string) tools.ApproveDecision {
	if h.AutoApprove || !h.ttyOut || !term.IsTTY(int(os.Stdin.Fd())) {
		return tools.ApproveDecision{Approved: h.AutoApprove}
	}
	fmt.Printf("\n⚠ 工具调用审批 [%s]\n参数: %s\n", name, args)
	fmt.Print("批准? [y/N]（其他输入作为反馈拒绝并回给模型）> ")
	rd := bufio.NewReader(os.Stdin)
	line, _ := rd.ReadString('\n')
	line = strings.TrimSpace(line)
	if strings.EqualFold(line, "y") || strings.EqualFold(line, "yes") {
		return tools.ApproveDecision{Approved: true}
	}
	if line == "" {
		return tools.ApproveDecision{}
	}
	return tools.ApproveDecision{Feedback: line}
}

func (h *TaskHost) ApprovePlan(ctx context.Context, plan string) (bool, string) {
	if h.AutoApprove || !h.ttyOut || !term.IsTTY(int(os.Stdin.Fd())) {
		if h.AutoApprove {
			return true, ""
		}
		return false, ""
	}
	fmt.Println("\n【计划审批】")
	fmt.Println(plan)
	fmt.Println("  1. 批准，按计划执行 — 退出计划模式并开始执行")
	fmt.Println("  2. 继续计划 — 不批准，重新提交")
	fmt.Print("序号，或其他输入作为反馈拒绝 > ")
	rd := bufio.NewReader(os.Stdin)
	line, _ := rd.ReadString('\n')
	line = strings.TrimSpace(line)
	switch {
	case line == "1" || strings.EqualFold(line, "y") || strings.EqualFold(line, "yes"):
		return true, ""
	case line == "" || line == "2":
		return false, "（用户未批准且无反馈）"
	default:
		return false, line
	}
}

func (h *TaskHost) NotifyEvent(kind, detail string) {}
