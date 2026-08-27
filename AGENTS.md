# grf-harness（gh）工作区说明

零依赖终端 agent harness：纯 Go 标准库重构自 `~/AiProjects/gh`（bubbletea/glamour 版）。
25 个工具的**定义（名称/描述/参数 Schema）严格来自 `docs/tools.json`**，一个不能少；
系统提示词的指导结构镜像 `docs/prompt.md`。改动工具相关代码前先读这两个文档。

## 构建与测试

```sh
./build.sh                # vet + build（产物 ./gh，跳过测试；macOS 会重做 adhoc 签名）
go build -o gh .
go test ./...             # 无需网络与 API key（agent 回环用 httptest 模拟 LLM）
go test -race ./agent ./tools ./term ./js ./llm
```

注意：本机 `~/.grf/settings.json` 已有可用的真实模型端点，直接 `./gh "任务"`
即可做端到端冒烟；全屏 UI 冒烟可用 python3 pty.fork 驱动（打字→截图式解析
ANSI→验证帧），历史会话输出落在 `.grf/sessions/`。

## 硬性约束

- **零第三方依赖**：`go.mod` 只允许 `module grf` + go 版本行，不新增 require。
  终端能力用自研 `term` 包，workflow 的 JS 用自研 `js` 包解释器。
- **无 `internal/` 目录**：所有包必须可被其他 Go 应用导入（模块路径 `grf`）。
- **工具定义唯一来源**：`tools/spec/tools.json` 是 `docs/tools.json` 的嵌入副本，
  两份必须保持一致；`tools.Registry.Register` 会拒绝 spec 里不存在的名字。
  改工具行为不改 spec 文本。嵌入方扩展走 `RegisterCustom`（自带定义、
  不得与内置 25 个重名），内置工具不借道覆盖。
- 回复**不做 markdown 渲染**，只显示原始文本（需求如此，勿加渲染）。

## 分层（依赖方向自上而下）

```
main.go → tui → app → agent → {tools, js, llm, config, store}
                              term（底层，被 tui/app 引用）
```

- `agent`：核心循环（Run/RunAs）、goal 状态机、Manager（子代理树）、ralph、
  workflow 接线、系统提示词。工具在此组装（`agent/build.go`）。
- `tools`：25 个工具的实现与注册表；orchestration 类工具（subagent/goal/
  workflow/ralph）的处理器注册在 `agent` 包，纯工具（fs/bash/jobs/web/
  image/misc）在 `tools` 包。
- `app`：库入口（`app.New(app.Options{...})`）+ 单任务直出渲染 + 非 TTY REPL。
- `tui`：全屏聊天与模型配置页。
- `weixin`：微信 ClawBot 消息渠道（`gh connect weixin` 拉起守护进程，日志在
  `~/.grf/weixin.log`；消息由本地 agent 回答）。

## 关键约定与坑

- **TUI 单 goroutine 规则**：`tui/chat.go` 的所有状态（blocks/todos/popups）
  只能在事件循环 goroutine 上改。后台 goroutine（agent 回调/Host）必须经
  `c.events` 通道投递（见 `onEvent`/`eventMsg`），直接改状态会 data race。
- `term.Screen` 是整帧全量重绘（无 diff）；resize（SIGWINCH）→ 上层按新宽度
  重新软换行后整帧 Paint。PTY 初始 winsize 可能为 0，Screen 已内置 80x24 兜底。
- agent 历史并发：`history` 一律走 `a.mu`；异步通知用 `EnqueueNotice`（空闲时
  触发 `OnIdleNotice` 自动续跑），通知只在 LLM 请求前 drain（tool 结果必须紧跟
  assistant 消息，OpenAI 协议要求）。
- 路径解析：文件工具的相对路径相对**工作区**（`FsPolicy.resolve`），不是进程
  CWD。macOS 上 t.TempDir() 在 /var（符号链接到 /private/var），边界判断不做
  symlink 展开，测试时两边要用同一路径构造。
- 观察策略（fs_policy=observe，默认）：write/edit 覆写**已存在**文件前需本会话
  read 过；新建文件不受限（`CheckWrite` 里用 os.Stat 区分）。
- 沙箱：`sandbox=off` 为默认；`workspace-write` 仅在 macOS 用 sandbox-exec，
  拒绝标记 `[sandbox: file access denied ...]` + 一次性升级重试协议
  （sandbox_permissions+justification → Host.ApproveEscalation）。
- workflow 的 js 解释器是受控子集（无 Promise 全家桶/扩展运算符）；钩子误用
  抛错必须终止整个脚本（fail-loud，见 agent/workflow.go 的 panicWorkflow）。
- goal/ralph/ask_user 仅根会话可用；子代理注册时就不挂这三个入口
  （`registerTools(root bool)`）。
- 注释用中文，风格与现有代码一致（包级说明 + 函数用途注释）。

## 平台

termios ioctl 用 build tags 分 darwin（TIOCGETA/SETA）与 linux（TCGETS/SETS），
其他平台回退非交互（`term/tty_other.go`）。全屏交互要求 stdin+stdout 均为 TTY，
否则自动回退纯文本行模式。
