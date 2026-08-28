# grf-harness（gh）

零依赖的终端 agent harness：**纯 Go 标准库实现**（无任何第三方依赖），
完整实现 25 个工具（定义内嵌于 `tools/spec/tools.json`，一个不少），OpenAI 兼容接口驱动，
默认全屏交互（终端宽度变化自动整帧重排重绘），亦可作为 Go 模块被其他应用
嵌入使用，或经 `gh server` 以 OpenAI Responses API 形状供非 Go 宿主接入。

## 特性

- **零依赖**：终端 raw 模式、备用屏幕、输入解析（按键/粘贴/SGR 鼠标）、
  CJK 宽度软换行、SSE 流式解析、迷你 JavaScript 解释器（workflow 用）
  全部原生实现
- **25 个工具**：定义内嵌自 `tools/spec/tools.json`（工具定义唯一来源），
  与 spec 严格一一对齐——ask_user_question / bash / create_goal / edit /
  exit_plan_mode / get_goal / glob / grep / interrupt_agent / job_kill /
  job_list / job_output / list_agents / ralph / read / read_image /
  send_message / skill / subagent / subagent_fork / todo_write / update_goal /
  web_search / workflow / write
- **全屏交互**（默认）：备用屏幕 + 底部输入框 + 内容滚动区；SIGWINCH /
  尺寸变化时按新宽度重新软换行整帧重绘；思考进度行、正文原文直出
  （不渲染 markdown，只显示原文）、连续工具事件合并折叠（Ctrl+O/单击展开）、
  todo/goal 状态面板、斜杠指令提示、模型/会话弹窗、ask 表单、计划审批、
  鼠标滚轮/单击/拖选复制（OSC52）
- **单任务模式**：`gh "<任务描述>"` 直接流式输出结果后退出
- **子代理系统**：subagent / subagent_fork（后台默认 + 通知注入）、
  send_message 续聊、interrupt_agent 取消、list_agents 树状列表
- **goal 自动续跑**：create/get/update_goal 严格 revision 校验、
  blocked 最少 3 轮、自动续跑直到 complete/blocked/上限
- **workflow**：文档规定的 JS 脚本编排（agent/pipeline/parallel/phase/
  log + args + 顶层 await + schema 校验），脚本由内置迷你 JS 解释器执行
- **ralph**：新鲜代理迭代循环（COMPLETE/BLOCKER 结构化报告）
- **bash 沙箱**：`workspace-write` 模式下用 macOS sandbox-exec 限制写入
  工作区；拒绝后按"一次性重试"协议升级（sandbox_permissions +
  justification + 用户审批）
- **会话持久化**：`.grf/sessions/` 会话记录，`-resume` / `/resume` 恢复
- **Responses API 服务**：`gh server` 以 ORS 兼容形状暴露 agent 能力
  （SSE 流式、client function tools、跨重启的暂停/恢复），见 `docs/server.md`
- **微信渠道**：`gh connect weixin` 拉起守护进程，微信消息由本地 agent
  回答（每用户独立会话）

## 用法

```sh
go build -o gh .      # 或 ./build.sh（vet + build；交叉编译见 build.sh macos|linux|linux-arm|windows|all → dist/）

gh                     # 全屏交互（默认模式）
gh -resume             # 恢复上一次会话
gh -plan               # 以计划模式启动（exit_plan_mode 呈交计划）
gh "统计当前目录 go 文件行数"   # 单任务模式：直接输出结果后退出
gh model               # 模型配置页（多提供商 / 自动检测模型）
gh model set <p/m>     # 脚本化设置默认模型
gh model list          # 列出已配置 提供商/模型
gh config              # 配置摘要
gh config prompt [set|reset]
gh config steps [n|reset]
gh config sandbox [off|workspace-write]
gh config fspolicy [observe|off]
gh connect weixin      # 连接微信 ClawBot（扫码后转后台守护进程；-workspace 指定工作区）
gh disconnect weixin   # 停止微信守护进程
gh server              # 启动 Responses API 服务（ORS 兼容，默认 127.0.0.1:7400；
                       #   -addr 监听地址 / -data 数据目录 / -token 令牌）
gh version
```

非 TTY（管道/重定向）自动回退纯文本行模式 REPL。

### 全屏快捷键

| 按键 | 功能 |
|---|---|
| `↑` / `↓` | 输入历史回溯（~/.grf/history）；指令提示弹出时选择 |
| `←` `→` `Home` `End`，`Ctrl+A/E/K/U/W` | 行编辑（UTF-8 感知） |
| `Enter` | 提交（指令提示弹出时先补全） |
| `Tab` | 补全指令 / 切换鼠标上报 |
| `PgUp` / `PgDn` / 滚轮 | 内容区翻页/滚动 |
| `Ctrl+O`、鼠标单击 | 展开/折叠思考摘要与工具调用合并块 |
| `F2` 或 `/plan` | 进入/退出计划模式 |
| `Ctrl+C` | 中断当前执行（空闲时退出） |
| `Ctrl+D` | 空输入时退出 |
| 鼠标拖选 | 按行选择上方内容，松开 OSC52 复制 |

指令：`/help` `/model` `/resume` `/plan` `/goal` `/save` `/exit`。

## 配置

`~/.grf/settings.json`（`GRF_HOME` 可重定位）：

```json
{
  "providers": {
    "deepseek": {
      "base_url": "https://api.deepseek.com/v1",
      "api_key": "sk-xxx",
      "models": ["deepseek-chat", "deepseek-reasoner"]
    }
  },
  "default": "deepseek/deepseek-chat",
  "max_steps": 0,
  "sandbox": "off",
  "fs_policy": "observe",
  "auto_approve": false
}
```

环境变量：`OPENAI_BASE_URL` / `OPENAI_API_KEY` / `GRF_MODEL` 临时覆盖；
`GRF_MOUSE=0/1` 鼠标开关。

- `sandbox: off | workspace-write` — 后者用 sandbox-exec 限制 bash 写入
  越界（macOS；无 sandbox-exec 时仅保留升级审批协议）
- `fs_policy: observe` — write/edit 覆写已存在文件前需本会话 read 过
  （文档默认的 fs-observation-policy）
- `auto_approve` — 非交互（单任务/嵌入）模式下自动批准沙箱升级与计划

## 接入 grf 的三条路径

| 路径 | 适用 | 文档 |
|---|---|---|
| **Go 库嵌入**（`app.New` 门面，25 工具/事件/多会话全可编程） | Go 应用 | `docs/integration.md` |
| **HTTP Responses API**（`gh server`，ORS 兼容；任意语言经 OpenAI SDK 指向即可） | 非 Go 宿主 / 结构化集成 | `docs/server.md` |
| **CLI 子进程**（`./gh "任务"` 单任务直出，解析 stdout） | 脚本 / CI / 轻量调用 | integration.md §10 |

架构总览（功能线条分层、暂停/恢复协议、并发规则等）见
**`docs/architecture.md`**。

### Go 库嵌入速览

模块路径 `git.gorun.fun/share/grf-harness`（与公开仓库地址一致，可直接
`go get`）；所有包均可公开导入（无 internal/）：

```sh
GOPRIVATE=git.gorun.fun go get git.gorun.fun/share/grf-harness@v0.0.1-alpha.2
```

```go
import (
    "git.gorun.fun/share/grf-harness/app"    // 应用门面（库入口）
    "git.gorun.fun/share/grf-harness/agent"  // 回合循环 / 暂停恢复 / goals / 子代理 / workflow
    "git.gorun.fun/share/grf-harness/tools"  // 25 个工具与注册表
    "git.gorun.fun/share/grf-harness/llm"    // OpenAI 兼容客户端（SSE / tool calls / 多模态）
    "git.gorun.fun/share/grf-harness/store"  // 会话持久化
    "git.gorun.fun/share/grf-harness/config" // ~/.grf/settings.json
    "git.gorun.fun/share/grf-harness/term"   // 终端底层（raw/输入解析/宽度换行/全屏渲染）
    "git.gorun.fun/share/grf-harness/tui"    // 全屏聊天 / 模型配置页
    "git.gorun.fun/share/grf-harness/js"     // 迷你 JS 解释器
)

a, err := app.New(app.Options{
    Workspace: "/path/to/ws",
    Host:      myHost, // 实现 tools.Host：AskUser / 审批 / 事件
})
defer a.Close()
err = a.RunTask(ctx, "做点什么")          // 单次执行
// 或嵌入自己的 UI：a.Root() 返回 *agent.Agent，可挂流式回调
```

`tools.Host` 是嵌入方的交互接口：`AskUser`（ask_user_question 表单）、
`ApproveEscalation`（沙箱升级审批）、`ApprovePlan`（计划审批）、
`NotifyEvent`（事件流）。

## 工具说明（25 个，定义见 tools/spec/tools.json）

| 类别 | 工具 |
|---|---|
| 文件 | `read`（行号/分页）、`write`、`edit`（唯一匹配/replace_all）、`glob`（mtime 序/100 条内联）、`grep`（250 条内联/include 过滤）、`read_image`（PNG/JPEG/GIF 解码缩放，WebP 透传） |
| 执行 | `bash`（workdir/timeout/exit code 标记/长输出截尾落盘/沙箱与升级协议）、`job_output` / `job_kill` / `job_list`（后台作业，流式增量读取） |
| 网络 | `web_search`（1-4 查询合并，DDG 摘要 + 来源列表） |
| 交互 | `ask_user_question`（表单）、`exit_plan_mode`（计划审批）、`todo_write`（面板展示）、`skill`（SKILL.md 目录） |
| 目标 | `create_goal` / `get_goal` / `update_goal`（revision 校验、自动续跑） |
| 编排 | `subagent` / `subagent_fork`（后台默认+落定通知）、`send_message` / `interrupt_agent` / `list_agents`、`workflow`（JS 脚本 + 并发/总量上限 + schema 校验）、`ralph`（新鲜代理循环） |

## 结构

```
main.go + chat.go + settings_cmd.go + connect.go + server_cmd.go
                      CLI 分发与子命令（package main；
                      daemon_unix/daemon_other.go 为守护进程平台文件）
term/                 终端底层：raw 模式、尺寸/SIGWINCH、输入事件解析、
                      ANSI 宽度与软换行、备用屏幕整帧渲染、行选取助手
llm/                  OpenAI 兼容客户端（SSE 流式、tool_calls、图像 parts）
                      + 正文回退工具调用解析（<tool_call> XML / DSML）
config/               ~/.grf/settings.json
store/                会话持久化（.grf/sessions）
tools/                工具注册表 + 25 个工具实现；spec/tools.json 为
                      工具定义唯一来源（go:embed 内嵌）
js/                   迷你 JavaScript 解释器（workflow 脚本执行）
agent/                回合循环（turn.go）与暂停/恢复协议（pause.go）、
                      Run* 入口族、子代理管理器、goal 状态机、ralph、
                      workflow 接线、装配（build.go）、系统提示词
app/                  库门面（app.New）、会话管理、单任务直出渲染、
                      非 TTY REPL、TaskHost
tui/                  全屏聊天（事件循环/流式汇聚/组帧渲染分文件：ask
                      表单、计划审批、弹窗、鼠标、resize 重绘）、模型配置页
server/               Responses API 接入层（ORS 兼容：会话池、SSE 事件
                      桥、客户端工具、暂停应答、compact）
weixin/               微信 ClawBot 渠道（每用户 agent 池、消息分发、
                      登录与媒体处理）
docs/                 architecture.md（架构总览）、integration.md（Go 库
                      对接）、server.md（HTTP 接入）
dist/                 交叉编译产物（build.sh，gitignore）
```

## 开发

```sh
./build.sh            # vet + build（产物 ./gh，跳过测试；macOS 重做 adhoc 签名）
./build.sh all        # 交叉编译 macos/linux/linux-arm/windows → dist/
go test ./...         # 全部测试（无需网络与 API key，agent 回环用 httptest）
go test -race ./agent ./tools ./term ./js ./llm ./server
```

工作区约定（分层规则、坑清单）见 `AGENTS.md`；架构说明见
`docs/architecture.md`。

## 与原 gh 项目的关系

本仓库是对 `~/AiProjects/gh`（bubbletea + glamour 版）的重构：
- 去除全部第三方依赖（bubbletea/glamour/ansi → 自研 term 包）
- 工具链从 gh 的 7 个（read_file/write_file/edit_file/delete_file/
  list_dir/run_command/http_request + 记忆三件套）替换为新定的 25 个工具规范
  （现内嵌于 tools/spec/tools.json），agent 核心按其文档语义重写（通知/子代理/goal/
  workflow/ralph/沙箱等）
- markdown 不再渲染（原 glamour），按需求显示原始 md 文本
- 保留：多提供商配置、会话持久化/恢复、输入历史、模型配置页
