// commands.go 斜杠指令：grf-harness 的会话级行为（与终端 REPL / TUI 的
// 指令同构），在回合执行前拦截。纯本地指令（/plan /goal /compact /plugin）
// 直接执行并以 assistant 消息回显结果，不经模型；任务型指令（/init /skill）
// 把输入改写为 harness 定义的任务文本后照常跑回合（由模型带工具完成）。
// 前端输入框的补全菜单只是这条通道的 UI，语义全部在此实现。
package server

import (
	"context"
	"fmt"
	"strings"
	"time"

	"git.gorun.fun/share/grf-harness/agent"
	"git.gorun.fun/share/grf-harness/llm"
	"git.gorun.fun/share/grf-harness/plugin"
)

// slashCommands 支持的指令名（不含参数语义；未列出的 /xxx 按普通消息
// 走模型，不做拦截）。
var slashCommands = []string{"/init", "/plan", "/compact", "/goal", "/skill", "/plugin"}

// isSlashCommand 首词是否命中指令表（多行输入只看首行首词）。
func isSlashCommand(text string) bool {
	first := text
	if i := strings.IndexAny(first, "\n"); i >= 0 {
		first = first[:i]
	}
	name := strings.Fields(strings.TrimSpace(first))
	if len(name) == 0 {
		return false
	}
	for _, c := range slashCommands {
		if name[0] == c {
			return true
		}
	}
	return false
}

// runSlashCommand 执行一条指令。返回值：
//
//	out     回显文本（handled=true 时以此直接完成回合，不经模型）
//	rewrite 任务型指令改写后的输入（handled=false 时以它继续正常回合）
//	handled 指令是否已完全处理
//	err     用法错误（映射 400，链尾不动）
func (s *Server) runSlashCommand(ctx context.Context, sess *session, text string) (out string, rewrite string, handled bool, err error) {
	first := text
	if i := strings.IndexAny(first, "\n"); i >= 0 {
		first = first[:i]
	}
	fields := strings.Fields(strings.TrimSpace(first))
	name := fields[0]
	arg := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), first))

	switch name {
	case "/plan":
		var on bool
		switch strings.ToLower(arg) {
		case "", "on", "1", "true":
			on = true
		case "off", "0", "false":
			on = false
		default:
			return "", "", false, fmt.Errorf("用法: /plan [on|off]（缺省开启；再次 /plan off 退出）")
		}
		sess.root.SetPlanMode(on)
		if on {
			return "计划模式已开启：模型将先呈计划（exit_plan_mode），批准后再实施。", "", true, nil
		}
		return "计划模式已退出。", "", true, nil

	case "/goal":
		if arg == "" {
			if g := sess.h.Goals.Marshal(); g != "" {
				return "当前 goal：\n" + g, "", true, nil
			}
			return "（当前无 goal）\n设定: /goal <目标描述>", "", true, nil
		}
		g, cerr := sess.h.Goals.Create(arg, 0)
		if cerr != nil {
			return "设定失败：" + cerr.Error() + "\n（已有活动 goal 时先让模型 update_goal complete，或用 /goal 查看）", "", true, nil
		}
		return fmt.Sprintf("goal 已设定并进入自动推进（round_limit=%d）：\n目标：%s\n\n模型会在每轮结束后自动续跑，直至完成/阻塞/达到轮次上限。",
			g.RoundLimit, g.Objective), "", true, nil

	case "/compact":
		msgs, cerr := s.compactInPlace(ctx, sess)
		if cerr != nil {
			return "压缩失败：" + cerr.Error(), "", true, nil
		}
		return fmt.Sprintf("上下文已压缩：%d 条历史消息 → 摘要（后续回合以摘要为基底继续）。", msgs), "", true, nil

	case "/plugin":
		if arg == "" {
			return "", "", false, fmt.Errorf("用法: /plugin <名称>（官方下载服务器校验后自动安装，名称可省略 grf- 前缀）")
		}
		out, cerr := s.installPlugin(ctx, arg)
		if cerr != nil {
			return "插件安装失败：" + cerr.Error(), "", true, nil
		}
		return out, "", true, nil

	case "/init":
		return "", "/init 任务：扫描当前工作区，理解项目结构与技术栈，生成/更新 AGENTS.md——" +
			"包含项目概述、常用命令、目录结构与分层、以及协作时必须遵守的硬性约束。" +
			"已存在 AGENTS.md 时在原有基础上修订。完成后简述改动。", false, nil

	case "/skill":
		if arg == "" {
			return "", "", false, fmt.Errorf("用法: /skill <一句话用途描述>（将在工作区创建技能并自动生效）")
		}
		return "", "/skill 任务：创建一个新技能，用途描述：" + arg + "\n" +
			"要求：\n" +
			"1) 由描述拟定一个简短的 kebab-case 名称（如 export-csv）。\n" +
			"2) 在工作区 .grf/skills/<名称>/SKILL.md 写入技能文件：YAML frontmatter" +
			"（name、description=上述用途描述）+ 正文写清触发时机与操作步骤。\n" +
			"3) 验证：确认文件已写入、frontmatter 可解析、同名技能不存在冲突。\n" +
			"4) 完成后报告技能名与文件路径。技能目录随会话加载，无需其他安装步骤。", false, nil
	}
	return "", "", false, fmt.Errorf("未知指令 %s", name)
}

// compactInPlace 就地压缩会话：把当前历史交给模型总结，随后以摘要重建
// 历史基底（总结生成本体与 /v1/responses/compact 端点同源：agent 包的
// BuildTranscript/SummarizeTranscript；区别是端点返回新链输入，这里直接
// 原地替换、链尾不变）。
func (s *Server) compactInPlace(ctx context.Context, sess *session) (int, error) {
	model := sess.root.Model()
	if model == "" {
		return 0, fmt.Errorf("会话尚无模型（先发一条消息或指定 model）")
	}
	transcript, n := agent.BuildTranscript(sess.root.History())
	if n == 0 {
		return 0, fmt.Errorf("会话尚无可压缩的历史")
	}
	client, err := s.clientFactory()
	if err != nil {
		return 0, fmt.Errorf("构建客户端: %w", err)
	}
	summary, err := agent.SummarizeTranscript(ctx, client, transcript)
	if err != nil {
		return 0, err
	}
	sess.root.RestoreFull([]llm.Message{{
		Role:    "user",
		Content: "以下是此前对话的压缩摘要（原始历史已被压缩替换）：\n\n" + summary,
	}}, sess.root.SystemText())
	return n, nil
}

// installPlugin 从官方下载服务器按名称安装插件：先探测列表校验存在
// （精确名优先，子串匹配需唯一），再下载到自动发现目录；重启 gh 后接入。
func (s *Server) installPlugin(ctx context.Context, query string) (string, error) {
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	rel, err := plugin.ProbePlugins(pctx, "", "")
	cancel()
	if err != nil {
		return "", fmt.Errorf("探测下载服务器: %w", err)
	}
	if rel == nil {
		return "", fmt.Errorf("下载服务器上没有当前平台的插件产物")
	}
	// 名称匹配与 gh plugins download 同一语义（plugin.PluginRelease.Resolve：
	// 归一精确 + 唯一子串）。
	hits, err := rel.Resolve(query)
	if err != nil {
		return "", fmt.Errorf("下载服务器（版本 %s）: %w", rel.Version, err)
	}
	saved, err := rel.Download(ctx, hits, nil)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("已安装: %s（版本 %s）\n重启 gh server 后接入，可用 gh plugins 体检连接状态。", strings.Join(saved, "、"), rel.Version), nil
}
