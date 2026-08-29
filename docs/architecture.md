# grf-harness 架构总览

按功能线条组织的仓库全景。对接嵌入方看 [integration.md](integration.md)（Go 库）与
[server.md](server.md)（HTTP）；工作区规则看根目录 AGENTS.md。

## 1. 功能线条（四层）

```
┌─ 接入表面（并列，各自薄装配，全部直接组装 agent.Build）─────────────┐
│  main(CLI, package main)                                             │
│    ├─ tui/      全屏聊天 + 模型配置页（CLI 的交互形态）              │
│    ├─ app/      库门面：app.New + 单任务直出 + 非 TTY REPL + TaskHost │
│    ├─ server/   HTTP Responses API（ORS 兼容，gh server）            │
│    └─ weixin/   微信 ClawBot 消息渠道（gh connect weixin 守护进程）  │
├─ 核心层 ────────────────────────────────────────────────────────────┤
│  agent/       回合循环（turn.go）+ 暂停/恢复协议（pause.go）+        │
│               子代理树 Manager + goal 状态机 + workflow/ralph 编排   │
├─ 能力层 ────────────────────────────────────────────────────────────┤
│  tools/       26 个工具实现 + Registry（spec/tools.json 内嵌）       │
│  js/          迷你 JS 解释器（仅 workflow 使用，受控子集）           │
│  llm/         OpenAI 兼容客户端（SSE/多模态）+ 正文回退工具调用解析   │
├─ 基础层 ────────────────────────────────────────────────────────────┤
│  term/        终端底层（raw/输入解析/CJK 宽度/整帧渲染）+ 文本助手    │
│  config/      ~/.grf/settings.json（env > 文件 > 默认）              │
│  store/       .grf/sessions 会话持久化                               │
└──────────────────────────────────────────────────────────────────────┘
```

依赖规则：自上而下单向、无环。能力/基础层的兄弟依赖仅 `tools→llm`（工具
定义的 wire 形状）、`store→llm`（消息线格式）。`term` 是最底层的文本度量
与终端库，任何上层（含 agent）都可引用其纯文本助手（`term.FirstLine` 等）。

## 2. 包职责与文件线条

| 包 | 职责 | 关键文件 |
|---|---|---|
| `main`（根目录） | CLI 分发与子命令 | `main.go`（分发）、`chat.go`、`settings_cmd.go`、`connect.go`、`server_cmd.go`、`daemon_unix/other.go`（平台） |
| `tui` | 全屏聊天状态机 | `chat.go`（事件循环）、`hub.go`（流式汇聚）、`render.go`（组帧/统计）、`input.go`、`popups.go`、`host.go`、`modelpage.go`、`editor.go` |
| `app` | 库门面 | `app.go`（Options/New/会话门面）、`render.go`（直出渲染+RunTask）、`taskhost.go`、`repl.go` |
| `server` | ORS Responses API | `server.go`（路由/错误）、`create.go`（主端点）、`session.go`（会话池/盘上为真）、`bridge.go`（事件→SSE）、`dto.go`、`extras.go`、`compact.go`、`deferred.go` |
| `weixin` | 微信渠道 | `bot.go`（Run/登录/轮询）、`pool.go`（每用户 agent 池）、`mux.go`（分发/回合执行）、`login.go`、`media.go`、`ilink.go`、`host.go`、`state.go` |
| `agent` | 核心循环与编排 | `turn.go`（runTurn）、`pause.go`（暂停/恢复）、`run.go`（Run* 入口族）、`agent.go`（结构/访问器）、`history.go`、`notice.go`、`events.go`、`manager.go`、`goals.go`、`workflow.go`、`ralph.go`、`build.go`（装配）、`prompt.go` |
| `tools` | 工具与注册表 | `registry.go`、`spec/`（唯一定义来源）、`fs.go`/`glob.go`/`grep.go`/`fspolicy.go`、`bash.go`、`jobs.go`、`web.go`、`image.go`、`askuser.go`、`todo.go`、`skill.go`、`plan.go`、`host.go`、`artifacts.go` |
| `js` | JS 解释器 | `lexer.go`、`parser.go`、`interp.go`、`eval.go`、`value.go`、`builtins.go`、`namespace.go` |
| `llm` | 模型客户端 | `client.go`、`message.go`、`stats.go`、`toolcall_fallback.go`/`dsml.go`/`fallback_tags.go`/`stream_display.go`（正文回退解析一条线） |
| `term` | 终端与文本 | `term.go` + `tty_*`（平台）、`input.go`、`width.go`、`screen.go`、`lines.go` |
| `config` / `store` | 配置 / 会话存档 | 单文件包 |

## 3. 关键机制

### 3.1 回合循环（agent/turn.go）

`runTurn` 是唯一的工具循环实现：并入 user 输入（或恢复处置）→ LLM 请求 →
逐个执行工具调用 → 结果回传 → 循环，直至产生最终回复或命中暂停点。所有
入口（`Run/RunAs/RunWithImages`、`RunTurn/RunTurnParts/Resume`、
`RunAutoGoal`）都是它的薄委托。MaxSteps 兜底循环上限；输出被 token 上限
截断时不结束回合、注入通知提醒模型重发。

### 3.2 暂停/恢复协议（agent/pause.go）

为跨请求应答交互（gh server）设计。**暂停状态完全落在持久化历史里**：
工具循环中途返回，历史尾部保留未闭合的 `tool_call`（OpenAI 线格式的合法
中间态），其参数即交互请求原文——因此进程重启后仍可恢复。三类触发：

- `function`：客户端执行工具（`Registry.RegisterClient`，ORS function 工具）
- `question`：deferred 宿主下的 ask_user 提问
- `tool_approval`：deferred 宿主 + 审批模式下的工具调用

恢复 = `Resume(ctx, []ResumeInput)` 把每个未闭合调用的处置（结果/答案/
批准与否）注入历史后续跑。`Run*` 系列遇暂停返回 `*PausedError`（不处理
暂停，接入方应改用 RunTurn/Resume）。判定暂停态只需看历史
（`HasPausedCalls`）。

### 3.3 装配收敛：agent.Build 是唯一装配事实

四个宿主表面都直接 `agent.Build(agent.Options{...})`（或经 `app.New` 薄
封装）拿到 `*agent.Harness`：根 agent、子代理工厂（Manager）、goal/todo/
FsPolicy/JobManager/Store/技能目录一次就位，26 个工具按根/子代理规则注册
（goal 三件套、ralph、ask_user 仅根会话）。表面之间不互相依赖——`server`
不 import `app`，`weixin` 不 import `app`，只有 main 把它们接成 CLI。
多会话 = 多个 Harness 实例（见 integration.md §9、server 的会话池、
weixin 的 agentPool）。

### 3.4 观察策略与沙箱升级（tools/fspolicy.go、bash.go）

- `fs_policy=observe`（默认）：write/edit 覆写**已存在**文件前需本会话
  read 过（os.Stat 区分新建）。
- `sandbox=workspace-write`（macOS sandbox-exec）：越界写返回
  `[sandbox: file access denied]` 标记，一次性升级重试协议
  （`sandbox_permissions` + `justification` → `Host.ApproveEscalation`）。
- 路径解析：文件工具的相对路径相对**工作区**，不是进程 CWD。

### 3.5 事件流

三条互不冲突的回调链（integration.md §5 有字段表）：

- `OnToolEvent`（结构化）：按 CallID 推进 running → done/error/denied，
  全量输入输出；子代理事件回流根。
- `OnEvent`（行式）：tool/result/notice/todos/goal/plan/phase。
- `StreamHooks`：正文/思考逐 token。

各表面将其映射到自己的媒介：TUI → 内容块与统计条；app → stdout 直出；
server → SSE 事件（bridge.go）；weixin → 控制台日志 + 消息回复。

### 3.6 并发规则（各表面的单线程纪律）

| 表面 | 规则 |
|---|---|
| tui | 所有状态只在事件循环 goroutine 改；后台回调经 `c.events` 通道投递（onEvent/eventMsg） |
| agent | `history` 一律走 `a.mu`；异步通知 `EnqueueNotice`（空闲触发 OnIdleNotice 续跑），只在 LLM 请求前 drain（tool 结果必须紧跟 assistant，OpenAI 协议要求） |
| server | 每会话单 turn（beginTurn/endTurn），并发请求 409 不排队 |
| weixin | 每用户一个 goroutine（userMux），同一用户串行、不同用户并发；agentPool 加锁 |

### 3.7 工具定义唯一来源

26 个内置工具的定义（名称/描述/参数 Schema）内嵌于
`tools/spec/tools.json`（go:embed），`Registry.Register` 拒绝 spec 外的
名字；嵌入方扩展走 `RegisterCustom`（自带定义、不得与内置重名），ORS
客户端工具走 `RegisterClient`。改工具行为不改 spec 文本。

## 4. 平台支持

termios ioctl 按 build tags 分 darwin/linux（`term/tty_*.go`），其他平台
回退非交互；守护进程脱离会话与进程探测在 `daemon_unix.go` /
`daemon_other.go`（package main）。交叉编译见 `./build.sh`（产物 `dist/`）。
