# grf-harness（gh）工作区说明

零依赖终端 agent harness：纯 Go 标准库重构自 `~/AiProjects/gh`（bubbletea/glamour 版）。
25 个工具的**定义（名称/描述/参数 Schema）内嵌于 `tools/spec/tools.json`**（唯一来源），
一个不能少。改动工具相关代码前先读 spec 与对接文档。

文档分工：`docs/architecture.md`（架构总览：功能线条分层、关键机制、并发
规则）、`docs/integration.md`（Go 库对接：go get 安装、事件、多会话）、
`docs/server.md`（HTTP Responses API 接入，ORS 兼容）。

## 构建与测试

```sh
./build.sh                # vet + build（产物 ./gh，跳过测试；macOS 会重做 adhoc 签名）
./build.sh macos|linux|linux-arm|windows|all   # 交叉编译，产物在 dist/
go build -o gh .
go test ./...             # 无需网络与 API key（agent 回环用 httptest 模拟 LLM）
go test -race ./agent ./tools ./term ./js ./llm ./server
```

注意：本机 `~/.grf/settings.json` 已有可用的真实模型端点，直接 `./gh "任务"`
即可做端到端冒烟；全屏 UI 冒烟可用 python3 pty.fork 驱动（打字→截图式解析
ANSI→验证帧），历史会话输出落在 `.grf/sessions/`。

## 硬性约束

- **零第三方依赖**：`go.mod` 只允许 `module git.gorun.fun/share/grf-harness` + go 版本行，不新增 require（模块路径与公开仓库地址一致，`go get` 安装见 docs/integration.md §1）。
  终端能力用自研 `term` 包，workflow 的 JS 用自研 `js` 包解释器。
- **无 `internal/` 目录**：所有包必须可被其他 Go 应用导入（模块路径 `git.gorun.fun/share/grf-harness`）。
- **工具定义唯一来源**：`tools/spec/tools.json`（go:embed 内嵌；docs/ 下的
  开发参考原稿已移除，spec 即唯一事实）。`tools.Registry.Register` 会拒绝
  spec 里不存在的名字。改工具行为不改 spec 文本。嵌入方扩展走 `RegisterCustom`（自带定义、
  不得与内置 25 个重名），内置工具不借道覆盖；ORS 客户端执行工具走 `RegisterClient`
  （模型调用时转入暂停协议，见 agent/pause.go）。
- 回复**不做 markdown 渲染**，只显示原始文本（需求如此，勿加渲染）。
- **Git 远端分工**：origin = 私有主库 `https://git.gorun.fun/ai/grf-harnees.git`
  （推送一律走这里；"harnees"是仓库实名，勿"纠正"）；`https://git.gorun.fun/share/grf-harness`
  是对外**公开镜像（模块安装地址），只读——禁止对它直接 push 或任何写操作**。

## 分层（功能线条，依赖自上而下、无环）

```
接入表面（并列，各自薄装配，全部直接组装 agent.Build）
  main(package main) ── tui（全屏）/ app（库门面）/ server（HTTP ORS）/ weixin（微信渠道）
核心层    agent（回合循环 turn.go + 暂停/恢复 pause.go + 子代理/goal/workflow/ralph）
能力层    tools（25 工具+注册表）/ js（workflow 解释器）/ llm（传输+回退解析）
基础层    term / config / store
```

- `agent`：核心回合循环（`turn.go` 的 `runTurn` 是唯一循环实现，`run.go` 的
  Run/RunAs/RunWithImages 与 `RunTurn/RunTurnParts/Resume` 都是其薄委托）、
  goal 状态机、Manager（子代理树）、ralph、workflow 接线、系统提示词。
  工具与 harness 在此组装（`agent/build.go`，四表面共用的唯一装配事实）。
- `tools`：25 个工具的实现与注册表；orchestration 类工具（subagent/goal/
  workflow/ralph）的处理器注册在 `agent` 包，纯工具（fs/bash/jobs/web/
  image/交互类）在 `tools` 包。
- `app`：库入口（`app.New(app.Options{...})`）+ 会话门面 + 单任务直出渲染 +
  非 TTY REPL + TaskHost。
- `tui`：全屏聊天（`chat.go` 事件循环 / `hub.go` 流式汇聚 / `render.go` 组帧）
  与模型配置页。
- `server`：Responses API 接入层（ORS 兼容；会话盘上为真、SSE 事件桥、
  client function tools、暂停应答链式续交、compact），`gh server` 子命令。
- `weixin`：微信 ClawBot 消息渠道（`gh connect weixin` 拉起守护进程，日志在
  `~/.grf/weixin.log`；消息由本地 agent 回答）。守护进程生命周期管理在根目录
  `connect.go` + 平台文件 `daemon_unix.go`/`daemon_other.go`（package main）。

## 关键约定与坑

- **TUI 单 goroutine 规则**：`tui/chat.go` 的所有状态（blocks/todos/popups）
  只能在事件循环 goroutine 上改。后台 goroutine（agent 回调/Host）必须经
  `c.events` 通道投递（见 `onEvent`/`eventMsg`），直接改状态会 data race。
- `term.Screen` 是整帧全量重绘（无 diff）；resize（SIGWINCH）→ 上层按新宽度
  重新软换行后整帧 Paint。PTY 初始 winsize 可能为 0，Screen 已内置 80x24 兜底。
- agent 历史并发：`history` 一律走 `a.mu`；异步通知用 `EnqueueNotice`（空闲时
  触发 `OnIdleNotice` 自动续跑），通知只在 LLM 请求前 drain（tool 结果必须紧跟
  assistant 消息，OpenAI 协议要求）。
- **暂停/恢复协议**（`agent/pause.go`）：暂停 = 历史尾部保留未闭合 `tool_call`
  （OpenAI 线格式合法中间态），状态完全在持久化历史里、跨重启可恢复；`Run*`
  遇暂停点返回 `*PausedError`，跨请求应答必须走 `RunTurn/Resume`（gh server
  即此用法）。server 每会话单 turn，并发请求 409 不排队。
- 路径解析：文件工具的相对路径相对**工作区**（`FsPolicy.resolve`），不是进程
  CWD。macOS 上 t.TempDir() 在 /var（符号链接到 /private/var），边界判断不做
  symlink 展开，测试时两边要用同一路径构造。
- 观察策略（fs_policy=observe，默认）：write/edit 覆写**已存在**文件前需本会话
  read 过；新建文件不受限（`CheckWrite` 里用 os.Stat 区分）。
- 沙箱：`sandbox=off` 为默认；`workspace-write` 仅在 macOS 用 sandbox-exec，
  拒绝标记 `[sandbox: file access denied ...]` + 一次性升级重试协议
  （sandbox_permissions+justification → Host.ApproveEscalation）。
- llm 有两条正文回退工具调用解析（`<tool_call>` XML 与 DeepSeek DSML），
  标记常量与变体归一正则统一在 `llm/fallback_tags.go`——三处（两个解析器 +
  stream_display 过滤）必须共用，勿再各写一套。
- workflow 的 js 解释器是受控子集（无 Promise 全家桶/扩展运算符）；钩子误用
  抛错必须终止整个脚本（fail-loud，见 agent/workflow.go 的 panicWorkflow）。
- goal/ralph/ask_user 仅根会话可用；子代理注册时就不挂这三个入口
  （`registerTools(root bool)`）。
- 首行摘要等纯文本助手统一用 `term.FirstLine/LastLine`（agent/app/tui/store
  已收敛），勿再各自实现。
- 注释用中文，风格与现有代码一致（包级说明 + 函数用途注释）。

## 平台

termios ioctl 用 build tags 分 darwin（TIOCGETA/SETA）与 linux（TCGETS/SETS），
其他平台回退非交互（`term/tty_other.go`）；守护进程平台差异在根目录
`daemon_unix.go`/`daemon_other.go`。全屏交互要求 stdin+stdout 均为 TTY，
否则自动回退纯文本行模式。
