# grf-harness（gh）

**简体中文** | [English](README.en.md)

零依赖的终端 AI agent harness：纯 Go 标准库实现（go.mod 无任何第三方依赖），
28 个内置工具，默认全屏终端交互，可作为 Go 模块嵌入其他应用，也能经
`gh server` 以 OpenAI Responses API 形状供任意语言宿主接入。
它在你的工作区里读代码、跑命令、改文件、拆子任务，直到把活干完。

![Go](https://img.shields.io/badge/Go-1.26-00ADD8) ![Dependencies](https://img.shields.io/badge/deps-0-success) ![Platforms](https://img.shields.io/badge/platforms-macos%20%7C%20linux%20%7C%20windows-9feaf9)

## 目录

| 文档 | 内容 |
|---|---|
| [使用指南](docs/usage.md) | 三种运行形态、全屏快捷键、斜杠指令、28 工具速查表 |
| [配置参考](docs/configuration.md) | `settings.json` 全字段、policy 规则串、环境变量一览 |
| [安全模型](docs/security.md) | 威胁边界、各渠道默认姿态、部署前必读的使用须知 |
| [Go 库嵌入](docs/integration.md) | `app.New` 门面、事件流、多会话管理 |
| [HTTP 接入](docs/server.md) | `gh server`：ORS 兼容的 Responses API 服务 |
| [MCP 插件](docs/plugins.md) | grf-browser / grf-computer，安装、配置与安全模型 |
| [插件开发](docs/plugin-dev.md) | mcpserver SDK、运行时契约、构建与分发 |
| [架构总览](docs/architecture.md) | 功能分层、回合循环、暂停/恢复协议、并发规则 |

## 核心特性

### 🖥 全屏终端体验

备用屏幕 + 底部输入框 + 内容滚动区；终端尺寸变化自动重排重绘；思考进度行、
正文原文直出（不渲染 markdown）、连续工具事件折叠展开（`Ctrl+O`）、
todo/goal 状态面板、斜杠指令提示、ask 表单与计划审批弹窗、鼠标滚轮与
拖选复制（OSC52）。非 TTY 环境（管道/CI）自动回退纯文本行模式。

### 🔧 28 个内置工具

文件读写编辑检索（`read` / `write` / `edit` / `glob` / `grep` / `read_image`）、
bash 执行与后台作业（`bash` / `job_*`）、跨会话记忆（`memory`，两级作用域）、
网络检索（`web_search`）、交互问答与计划模式（`ask_user_question` /
`enter/exit_plan_mode`）、目标管理（`create/get/update_goal`）、
子代理编排（`subagent` / `subagent_fork` / `send_message` / `interrupt_agent` /
`list_agents` / `workflow` / `ralph`）。定义全部内嵌于
`tools/spec/tools.json`（唯一来源），速查表见[使用指南](docs/usage.md#工具速查)。

### 🤖 任务编排

- **子代理**：后台派生独立 agent（继承策略与工具链），落定后通知注入父会话
- **goal**：带 revision 校验的目标状态机，blocked 自动重试，完成即停
- **workflow**：JS 脚本编排（`agent()` / `pipeline` / `parallel` + 顶层
  `await` + schema 校验），由内置迷你 JS 解释器执行（资源受限：深度/步数/内存护栏）
- **ralph**：新鲜代理迭代循环，直到产出 COMPLETE / BLOCKER 结论

### 🛡 安全边界

- **policy 规则串**：`write-workspace`（macOS 下 bash 经 sandbox-exec 限写工作区
  + 覆写前须先 read）、`read-workspace`（读边界）、`off`
- **越权即问**：越界写/沙箱拒绝时挂起征求用户批准，拒绝即终局
- **审批/计划模式**：逐工具确认 / 先计划后执行，用户把关
- **秘密隔离**：bash 与插件子进程环境自动剔除疑似密钥变量
- 详见[安全模型](docs/security.md)

### 📦 三种接入方式

| 方式 | 适用 | 文档 |
|---|---|---|
| **终端 CLI**（`gh` 全屏 / `gh "任务"` 单任务直出） | 日常使用、脚本、CI | [使用指南](docs/usage.md) |
| **Go 库嵌入**（`app.New`，全部能力可编程） | Go 应用 | [Go 库嵌入](docs/integration.md) |
| **HTTP Responses API**（`gh server`，ORS 兼容） | 任意语言宿主 | [HTTP 接入](docs/server.md) |

### 🔌 MCP 插件

增强能力经独立编译的插件扩展：**grf-browser**（chromedp 浏览器自动化，
17 工具，对齐 playwright-mcp）与 **grf-computer**（跨平台电脑操作，10 工具）。
本地 stdio 自动发现或远程 HTTP 接入，`gh plugins` 一键体检。
见 [MCP 插件](docs/plugins.md)；写自己的插件看[插件开发](docs/plugin-dev.md)。

### 💬 微信渠道

`gh connect weixin` 拉起守护进程，微信消息由本地 agent 回答（每用户独立
会话）。默认拒绝所有发送者（白名单制），工作区与策略独立收紧，工具审批
经微信问答转发。

## 快速开始

### 1. 安装

```sh
# 方式一：发布服务器下载（macOS / linux × amd64 / arm64，Windows 同）
#   https://download.gorun.fun/grf-harness/<平台>/<版本>/gh
# 下载后 `gh update` 可自更新（SHA256SUMS 校验）

# 方式二：源码构建（零依赖，克隆即编译）
git clone https://git.gorun.fun/share/grf-harness
cd grf-harness
go build -o gh .        # 或 ./run.sh（vet + build；交叉编译 ./run.sh all）
```

### 2. 首次配置

`~/.grf/settings.json`（`GRF_HOME` 可重定位）写入任意 OpenAI 兼容端点：

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
  "policy": "write-workspace"
}
```

或用环境变量临时指定：`OPENAI_BASE_URL` / `OPENAI_API_KEY` / `GRF_MODEL`。
完整字段说明见[配置参考](docs/configuration.md)。

### 3. 跑起来

```sh
gh                     # 全屏交互（默认；非 TTY 自动回退行模式）
gh -resume             # 恢复上一次会话
gh "统计当前目录 go 文件行数"   # 单任务模式：直接输出结果后退出
```

## 常用命令

```sh
gh model               # 模型配置页（多提供商 / 自动检测）
gh model set <p/m>     # 脚本化设置默认模型
gh config              # 配置摘要（prompt / steps / policy 子命令）
gh server              # Responses API 服务（默认 127.0.0.1:7400 + Bearer）
gh connect weixin      # 连接微信渠道（扫码后转后台守护进程）
gh plugins             # 插件体检；gh plugins download 下载官方插件
gh update              # 自更新（-y 跳过确认）
gh version
```

## 安全须知（简版，完整版见安全模型）

- gh 驱动**本机 bash 与文件工具**：默认 policy 为 `off`（无沙箱），建议显式
  配置；**Linux/Windows 无进程级 bash 沙箱**（文件工具边界全平台生效）
- `gh server` 是受控的本机 RCE 入口：默认仅环回 + Bearer token，绑外网前
  先读 [docs/server.md](docs/server.md)
- 微信渠道默认拒绝所有发送者：配置 `weixin.allow_senders` 后可用
- 插件进程不受 harness policy 约束，只安装可信来源
- 克隆的仓库携带的 `.grf/`（grants/memory）按不可信输入对待，系统提示词
  会披露其存在

## 开发

```sh
./run.sh              # vet + build（产物 ./gh；macOS 自动 adhoc 签名）
go test ./...         # 全部测试（无需网络与 API key）
go test -race ./agent ./tools ./term ./js ./llm ./server
```

工作区约定（分层规则、坑清单）见 `AGENTS.md`；架构说明见
[docs/architecture.md](docs/architecture.md)。

## 与原 gh 项目的关系

本仓库是对 `~/AiProjects/gh`（bubbletea + glamour 版）的重构：去除全部
第三方依赖（自研 `term` 包与迷你 JS 解释器），工具链重定义为 28 个工具
规范（内嵌 `tools/spec/tools.json`），agent 核心按文档语义重写；markdown
不再渲染，按需求显示原始文本。保留多提供商配置、会话持久化/恢复、输入
历史与模型配置页。
