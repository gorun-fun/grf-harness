# grf-harness（gh）工作区说明

零依赖终端 agent harness：纯 Go 标准库重构自 `~/AiProjects/gh`（bubbletea/glamour 版）。
28 个工具的**定义（名称/描述/参数 Schema）内嵌于 `tools/spec/tools.json`**（唯一来源），
一个不能少。改动工具相关代码前先读 spec 与对接文档。

文档分工（**每篇都有中英双版本：`xxx.md` 中文为默认 + `xxx.en.md` 英文，
第 3 行语言切换（当前语言加粗）；修改任一版本必须同步另一版本——这是硬约束**）：
`docs/usage.md`（使用指南：三形态、快捷键、指令、工具速查）、
`docs/configuration.md`（配置参考：settings.json 全字段、policy、环境变量）、
`docs/security.md`（安全模型：威胁边界、各渠道默认、检查清单）、
`docs/architecture.md`（架构总览：功能线条分层、关键机制、并发
规则）、`docs/integration.md`（Go 库对接：go get 安装、事件、多会话）、
`docs/server.md`（HTTP Responses API 接入，ORS 兼容）、
`docs/plugins.md`（MCP 插件：grf-browser/grf-computer、安装配置、
协议子集与安全模型）、`docs/plugin-dev.md`（插件开发规范：运行时
契约、工具准入、SDK 与裸写、测试、构建分发）。README 同理
（README.md + README.en.md，面向人类读者的入口：目录导航 + 快速开始，
细节一律下沉 docs）。

## 构建与测试

```sh
./run.sh                # vet + build（产物 ./gh，跳过测试；macOS 会重做 adhoc 签名）
./run.sh macos|macos-arm64|linux|linux-arm64|windows|windows-arm64|all   # 交叉编译，产物在 dist/
./run.sh all            # 6 平台全量；CI（.gitlab-ci.yml，仅 tag 触发：test 门禁 →
                          # build → publish）用它构建后发布到
                          # 发布机 ${DEPLOY_DIR}（CI Variables）/{平台}/{tag去v}/gh(.exe)，
                          # 随产物生成 SHA256SUMS 清单（selfupdate.Install 下载校验）；
                          # 版本号经 ldflags 注入 config.Version（CI 取 tag、本地取 git describe，去 v 前缀）；
                          # 交叉编译产物 linux/windows 经 upx 压缩（UPX=0 禁用；darwin 与 windows/arm64
                          # 跳过，同 plugins/build.sh 规则）
./plugins/build.sh        # 插件独立构建（独立模块，产物 plugins/bin/；install 安装到
                          # ~/.grf/plugins/ 自动发现；all 交叉编译 6 平台 → plugins/dist/，
                          # linux/windows 产物经 upx 压缩——darwin 与 windows/arm64 跳过）
                          # CI 同 tag 一并发布到 /data/download/grf-harness/{平台}/{版本}/plugins/
                          # （文件名去平台后缀），gh plugins download 按此布局探测下载
go build -o gh .
go test ./...             # 无需网络与 API key（agent 回环用 httptest 模拟 LLM；插件 go test 下自动空转）
go test -race ./agent ./tools ./term ./js ./llm ./server
```

注意：真实模型端点配置在 `~/.grf/settings.json` 的 providers（GRF_HOME 可
重定位），默认模型可用时直接 `./gh "任务"` 即可做端到端冒烟；全屏 UI 冒烟可用
python3 pty.fork 驱动（打字→截图式解析 ANSI→验证帧），历史会话输出落在
`.grf/sessions/`。

## 硬性约束

- **零第三方依赖**：主模块 `go.mod` 只允许 `module git.gorun.fun/share/grf-harness` + go 版本行，不新增 require（模块路径与公开仓库地址一致，`go get` 安装见 docs/integration.md §1）。
  终端能力用自研 `term` 包，workflow 的 JS 用自研 `js` 包解释器。
  **唯一例外**：`plugins/` 是独立 Go 模块（独立 go.mod，不参与主构建），
  允许第三方依赖（当前仅 chromedp 及其传递依赖）。
- **无 `internal/` 目录**：所有包必须可被其他 Go 应用导入（模块路径 `git.gorun.fun/share/grf-harness`）。
- **工具定义唯一来源**：`tools/spec/tools.json`（go:embed 内嵌；docs/ 下的
  开发参考原稿已移除，spec 即唯一事实）。`tools.Registry.Register` 会拒绝
  spec 里不存在的名字。改工具行为不改 spec 文本。嵌入方扩展走 `RegisterCustom`（自带定义、
  不得与内置 28 个重名），内置工具不借道覆盖；ORS 客户端执行工具走 `RegisterClient`
  （模型调用时转入暂停协议，见 agent/pause.go）。
- 回复**不做 markdown 渲染**，只显示原始文本（需求如此，勿加渲染）。
- **Git 远端分工**：origin = 私有主库（地址见本机 `git remote -v`，不入库文；
  推送一律走这里）；`https://git.gorun.fun/share/grf-harness`
  是对外**公开镜像（模块安装地址），只读——禁止对它直接 push 或任何写操作**。

## 分层（功能线条，依赖自上而下、无环）

```
接入表面（并列，各自薄装配，全部直接组装 agent.Build）
  main(package main) ── tui（全屏）/ app（库门面）/ server（HTTP ORS）/ weixin（微信渠道）
核心层    agent（回合循环 turn.go + 暂停/恢复 pause.go + 子代理/goal/workflow/ralph）
能力层    tools（28 工具+注册表）/ js（workflow 解释器）/ llm（传输+回退解析）
          mcp（MCP 客户端：stdio + Streamable HTTP）/ plugin（MCP 插件管理器 +
          download.go 官方插件下载）
基础层    term / config / store / selfupdate（自更新：目录探测 + semver 比较 +
          替换自身；ListDir/Install 通用件供 plugin 下载复用，基址
          https://download.gorun.fun/grf-harness，GRF_DOWNLOAD_BASE 覆盖）
插件      plugins/（独立 Go 模块，独立构建：grf-browser + grf-computer，
          内部 internal/mcpserver 是插件侧 MCP 服务端 SDK）
```

- 根目录 `main`（package main）子命令按功能线条分文件：`chat.go`（默认三形态：
  全屏 TTY / 非 TTY 行模式 REPL / 单任务直出）、`settings_cmd.go`（model/config/
  version 脚本化读写）、`server_cmd.go`（gh server）、`plugins_cmd.go`（gh plugins
  插件体检 + `gh plugins download` 官方插件下载）、`update_cmd.go`（gh update
  自更新，探测逻辑在 `selfupdate` 包；全屏交互启动时后台探测、有更新经事件循环
  提示）；connect/daemon 见 weixin 条目。
- `agent`：核心回合循环（`turn.go` 的 `runTurn` 是唯一循环实现，`run.go` 的
  Run/RunAs/RunWithImages 与 `RunTurn/RunTurnParts/Resume` 都是其薄委托）、
  goal 状态机、Manager（子代理树）、ralph、workflow 接线、系统提示词。
  工具与 harness 在此组装（`agent/build.go`，四表面共用的唯一装配事实）。
- `tools`：28 个工具的实现与注册表；orchestration 类工具（subagent/goal/
  workflow/ralph）的处理器注册在 `agent` 包，纯工具（fs/bash/jobs/web/
  image/交互类/记忆）在 `tools` 包。memory 跨会话记忆是 JSON 条目库
  （增删改查），两级落点：project（默认）= 工作区 `.grf/memory.json`，
  user = `~/.grf/memory.json`（`agent/build.go` 的 `memoryDeps` 装配）。
- `app`：库入口（`app.New(app.Options{...})`）+ 会话门面 + 单任务直出渲染 +
  非 TTY REPL + TaskHost。
- `tui`：全屏聊天（`chat.go` 事件循环 / `hub.go` 流式汇聚 / `render.go` 组帧）
  与模型配置页。
- `server`：Responses API 接入层（ORS 兼容；会话盘上为真、SSE 事件桥、
  client function tools、暂停应答链式续交、compact），`gh server` 子命令。
  客户端工具同名重注册幂等（ORS 客户端每请求重发 tools 数组是常态，
  `registry.go` RegisterClient 覆盖式；与内置重名/请求内重名在 `applyTools`
  提前 400）；`parseRespID` 校验 sid 为 16 位 hex（sid 拼磁盘路径，防穿越）；
  会话 meta/快照落盘 0600；HTTP 层设 ReadTimeout/IdleTimeout（不设
  WriteTimeout——SSE 长写不可预估）。
- `weixin`：微信 ClawBot 消息渠道（`gh connect weixin` 拉起守护进程，日志在
  `~/.grf/weixin.log` 0600；消息由本地 agent 回答）。**渠道安全默认**：发送者
  白名单（settings.json `weixin.allow_senders`，空 = 拒绝所有并回执指引，未授权
  回执按发送者 60s 节流）；工作区默认 `~/.grf/weixin/ws`（不再默认整个 home）；
  策略默认 `write-workspace|read-workspace`（`weixin.policy` 覆盖，全局 AutoApprove
  不带入）；审批（逐工具/升级/计划）经微信问答转发（`host.go` 的 Approve* 实现，
  超时按拒绝）；媒体下载钉域 `*.weixin.qq.com`（`media.go` checkCDNURL，防 SSRF）；
  守护进程拉起用 pid 文件 O_EXCL 互斥（`connect.go` claimPidFile）。守护进程生命
  周期管理也在本包：`connect.go` 的 `Connect`/`Disconnect` + 平台文件
  `daemon_unix.go`/`daemon_other.go`（根目录 main.go 只做子命令分发）。
- `mcp` + `plugin`：MCP 插件接入（docs/plugins.md）。`mcp` 是零依赖客户端
  （`stdio.go` spawn 子进程行分隔 JSON-RPC / `http.go` Streamable HTTP，
  仅 initialize/tools 子集，通知一律丢弃；stdio stderr 环形截留用于诊断）。
  `plugin.Manager` 负责配置解析（`settings.json` 的 `plugins`；键缺省时
  自动发现 `~/.grf/plugins` 下可执行文件）+ 建连枚举 + 经
  `RegisterInto` 把工具注册进各 agent 注册表（`RegisterCustom`，底层连接
  **进程级共享**——`plugin.Shared`，gh server 每会话 harness 复用同一批
  插件连接；`testing.Testing()` 下自动空转保证测试无副作用）。连接失败只
  降级（警告追加进系统提示词），不致命。
- `plugins/`：独立 Go 模块的插件二进制（grf-browser=chromedp 浏览器操作、
  grf-computer=平台命令适配的电脑操作；`plugins/internal/mcpserver` 是插件
  侧 SDK，一份工具代码同时支持 stdio 与 `serve --addr` HTTP 双模式）。
  工具名自带前缀命名空间、不得与内置 28 个重名；`readOnlyHint` 决定计划
  模式是否放行（未标注的插件工具在 `agent/turn.go` 的 `planBlocked` 里按
  改动类拦截，集合经 `Agent.planExtra` 下发）。

## 关键约定与坑

- **同批工具分发的 dsh 语义**（`turn.go` 的 `dispatchBatch`）：连续并行安全
  工具（`parallelSafeTools` 只读集）入有界池并发（`MaxParallelTools` 默认 8），
  其余独占串行并构成屏障；结果并入历史的完成顺序与模型顺序无关，请求快照由
  `canonicalizeToolResults` 按发起顺序重排闭合——改分组逻辑时勿破坏该不变式。
  循环层横切件：结果裁剪网 `pruneResultText`（>8192 保头 4096+尾 1024）、
  回合取消未开始调用的 aborted-before-dispatch 合成结果、崩溃残留未闭合调用
  的 outcome-unknown 占位、取消流的已交付前缀入历史（interrupted 标记）、
  连续相同调用 3/5/8 次递进提醒。前台 bash 超时晋升后台 job
  （`bash.go` promoteOnTimeout；晋升后命令归 job 所有，不随回合取消死）。
  运行中插话（`steer.go`）：`Agent.Steer` 在步骤边界以普通 user 消息并入
  （注入点必须保持"tool 结果已闭合"约束）；TUI 运行中回车即插话（回合
  结束由 finishStream 的 DrainSteer 转下一回合），weixin 运行中纯文本
  Steer、媒体/命令回队。
- **会话压缩**（`compact.go`）：新回合入口估算 token 超阈值（默认 100k，
  settings 的 `compact_threshold` 覆盖/负值关闭）或 `/compact` 手动触发，
  历史收敛为 [system 原文, 摘要消息]；只在回合边界发生（暂停态拒绝），
  自动失败开放。总结生成本体（提示词 `CompactSystemPrompt`/transcript
  构造 `BuildTranscript`/输出规范化 `SummarizeTranscript`）收敛在 agent
  包导出件，server 的 compact 端点与 `/compact` 指令薄委托，无第二份。
- **TUI 单 goroutine 规则**：`tui/chat.go` 的所有状态（blocks/todos/popups）
  只能在事件循环 goroutine 上改。后台 goroutine（agent 回调/Host）必须经
  `c.events` 通道投递（见 `onEvent`/`eventMsg`），直接改状态会 data race。
- `term.Screen` 是整帧全量重绘（无 diff）；resize（unix SIGWINCH / Windows
  轮询，见 `term/resize_*.go`）→ 上层按新宽度重新软换行后整帧 Paint。PTY
  初始 winsize 可能为 0，Screen 已内置 80x24 兜底。
- agent 历史并发：`history` 一律走 `a.mu`；异步通知用 `EnqueueNotice`（空闲时
  触发 `OnIdleNotice` 自动续跑），通知只在 LLM 请求前 drain（tool 结果必须紧跟
  assistant 消息，OpenAI 协议要求）。
- **暂停/恢复协议**（`agent/pause.go`）：暂停 = 历史尾部保留未闭合 `tool_call`
  （OpenAI 线格式合法中间态），状态完全在持久化历史里、跨重启可恢复；`Run*`
  遇暂停点返回 `*PausedError`，跨请求应答必须走 `RunTurn/Resume`（gh server
  即此用法）。server 每会话单 turn，并发请求 409 不排队。
- 计划/审批模式（仅根会话；Tab 循环 auto→计划→审批，或 F2 / `/plan` / `-plan`）：
  计划模式 = 提示词软约束 + `turn.go` 硬拦截（write/edit/bash/ralph/workflow
  直接拒绝，子代理经 `planGuard` 跟随根），`exit_plan_mode` 批准后自动退出、
  被拒则带反馈留在计划模式（审批是 ask 选择框：TUI 复用 ask 表单"批准/继续
  计划"+✎ 反馈，TaskHost 行模式为编号选项，空输入一律不批准）；模型也可经
  `enter_plan_mode`（仅根注册）主动申请进入，经 `Host.AskUser` 征用户批准后
  切换、拒绝则带反馈直接执行；
  审批模式 = 每个工具执行前经 `Host.ApproveTool`
  （TUI 复用 ask 选项表单；server 转 `tool_approval` 暂停点）。
- 路径解析：文件工具的相对路径相对**工作区**（`FsPolicy.resolve`），不是进程
  CWD。边界判断按 `EvalSymlinks` 展开后的真实路径进行（工作区本身也归一，
  `FsPolicy.wsReal`，防区内链接指向外部的穿越），macOS 的 /var →
  /private/var 两侧都会被接受。
- 统一规则串 `policy`（竖线组合，原 sandbox+fs_policy 已合并）：
  `write-workspace`（写侧：bash 仅 macOS 用 sandbox-exec 限写工作区 +
  write/edit 覆写前须先 read，新建文件不受限）| `read-workspace`（读边界：文件工具 `FsPolicy.CheckRead` 硬拒绝、
  不接升级协议；bash 沙箱叠加读黑名单——deny `/Users` `/root` `/home`
  后 allow 工作区，因 macOS 默认拒绝读会 SIGABRT 只能黑名单）。空值=默认 `off`（全关）；解析出错回退 `write-workspace`（保写侧）；
  `off` 全关。解析在
  `config.ParsePolicy/NormalizePolicy`。bash 子进程环境剔除疑似密钥变量
  （`tools/env.go` 的 ChildEnv：`*API_KEY*`/`*TOKEN*`/`*SECRET*` 等命名族；
  mcp 插件子进程同规则，见 `mcp/stdio.go` 的 stripSecretEnv——两处同步改）。写边界（文件工具只写工作区）
  无条件生效不受 policy 控制；bash 越界拒绝标记 `[sandbox: file access
  denied ...]`；越权即问——写边界/bash 沙箱拒绝时 harness 直接挂起问
  `Host.ApproveEscalation` 返回 `EscalationDecision`（批准=当次放行，bash
  脱沙箱重跑同一命令；拒绝=终局，记录消费掉，模型带 sandbox_permissions
  重试也不会二次询问；write/edit 可选 GrantDir=授权目录，持久化到工作区
  `.grf/grants.json` 仅本工作区生效，该目录下后续写越过写边界不再询问——
  但 grants 文件自身禁止模型侧写入（`fspolicy.go` CheckWrite 硬拒，防自写
  授权）；随仓库分发的 grants 构建期披露进系统提示词（`GrantsNotice`），
  且 "/" 与家目录级条目拒载）。多租户用户隔离：Workspace=用户文件夹 +
  默认 policy + 禁 bash + Host.ApproveEscalation 恒 false。
  `set_policy` 工具（仅根会话注册，`tools/policy.go`）：模型只能拉起弹窗
  并附理由，不可传目标 policy——用户从预制选项（`write-workspace` 工作区
  访问 / `off` 完全访问，见 `tools.SetPolicyOptions`）亲自选择，经
  `Host.AskUser` 弹窗呈现；选定后仅本会话生效、不落盘（`Harness.applyPolicy`
  更新内存 Settings + 根 FsPolicy，且经 `Manager.SetPolicyAll` 全树同步到
  已存在的子代理（不留旧策略绕行通道）；bash 沙箱开关经 `FsPolicy.Limits`
  实时读取，立即生效）；未选或保持现状为拒绝，终局。
- llm 有两条正文回退工具调用解析（`<tool_call>` XML 与 DeepSeek DSML），
  标记常量与变体归一正则统一在 `llm/fallback_tags.go`——三处（两个解析器 +
  stream_display 过滤）必须共用，勿再各写一套。
- workflow 的 js 解释器是受控子集（无 Promise 全家桶/扩展运算符）；钩子误用
  抛错必须终止整个脚本（fail-loud，见 agent/workflow.go 的 panicWorkflow）。
  资源护栏（`js/interp.go`，脚本=模型输出=半受信）：嵌套深度上限（表达式与
  用户函数帧共用计数，防 Go 栈溢出 fatal）、步数预算 + 周期性 ctx 取消观察
  （防纯自旋卡死回合，workflow 工具入口走 `RunEvalCtx` 传回合 ctx）、解析器
  深度上限（`parser.go`）、字符串操作 16MB 上限（repeat/padEnd/拼接）；Future
  goroutine 的 panic 一律转错误（不 re-panic，进程级崩溃面封死）。
- goal/ralph/ask_user 仅根会话可用；子代理注册时就不挂这三个入口
  （`registerTools(root bool)`）。
- 首行摘要等纯文本助手统一用 `term.FirstLine/LastLine`（agent/app/tui/store
  已收敛），勿再各自实现。
- 注释用中文，风格与现有代码一致（包级说明 + 函数用途注释）。

## 平台

termios ioctl 用 build tags 分 darwin（TIOCGETA/SETA）与 linux（TCGETS/SETS），
Windows 用 Console API 提供同套能力（`term/tty_windows.go`：VT 输入模式把
方向键等翻译成 ANSI 序列喂给 input.go 既有解析器、VT 输出模式让备用屏幕等
转义序列生效；老 conhost 不支持 VT 时 OpenRaw 报错照旧回退行模式），其余
平台回退非交互（`term/tty_other.go`）。resize 监听平台化（`term/resize_*.go`）：
unix 走 SIGWINCH，Windows 无等价信号改轮询。守护进程平台差异在 `weixin` 包
（`weixin/daemon_unix.go`/`daemon_other.go`）。全屏交互要求 stdin+stdout 均为 TTY，
否则自动回退纯文本行模式。bash 工具的执行载体平台化（`tools/bash.go` 的
`resolveShell`）：windows 依次探测 Git Bash 常规安装位与 PATH 上非
System32 的 bash.exe（`System32\bash.exe` 是 WSL 启动器、命令跑在 Linux
侧，须跳过），均无退回 PowerShell（`-NoProfile` + UTF-8 输出编码 + 原生
退出码透传包装；`tools.ShellNote()` 经系统提示词"运行环境"段告知模型写
PowerShell 语法）。命令停止语义同平台化：unix 先 SIGINT 优雅退出、
windows `taskkill /T /F` 终止进程树（`tools/bash_stop_*.go`）。
