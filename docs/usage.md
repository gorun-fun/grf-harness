# 使用指南

**简体中文** | [English](usage.en.md)

gh 的三种运行形态、全屏交互操作、斜杠指令与 28 个内置工具速查。
配置文件与政策规则见[配置参考](configuration.md)；HTTP 服务形态的完整
协议见[HTTP 接入](server.md)。

## 三种运行形态

| 形态 | 启动 | 适用 |
|---|---|---|
| **全屏交互**（默认） | `gh` | 日常使用：备用屏幕 + 底部输入框，流式输出、工具事件、表单与审批弹窗 |
| **行模式 REPL** | 非 TTY 下自动回退（管道/CI） | 脚本环境：逐行输入输出，无 ANSI 转义 |
| **单任务直出** | `gh "<任务描述>"` | 一次性任务：直接流式输出结果后退出，适合脚本与 CI 管道 |

常用启动参数：`-resume`（恢复上一次会话）、`-plan`（以计划模式启动）、
`-model <provider/model>`（临时指定模型）。

全屏启动时后台探测新版本（dev 构建跳过，网络故障静默），有更新在欢迎行
下方提示；`gh update` 手动更新（`-y` 跳过确认）。

## 全屏快捷键

| 按键 | 功能 |
|---|---|
| `↑` / `↓` | 输入历史回溯（`~/.grf/history`）；指令提示弹出时选择 |
| `←` `→` `Home` `End`，`Ctrl+A/E/K/U/W` | 行编辑（UTF-8 感知） |
| `Enter` | 提交（指令提示弹出时先补全；运行中提交 = 插话，步骤边界并入当前回合） |
| `Tab` | 补全指令 / 切换鼠标上报 |
| `PgUp` / `PgDn` / 滚轮 | 内容区翻页/滚动 |
| `Ctrl+O`、鼠标单击 | 展开/折叠思考摘要与工具调用合并块 |
| `F2` 或 `/plan` | 进入/退出计划模式（`Tab` 循环 auto→计划→审批） |
| `Ctrl+C` | 中断当前执行（空闲时退出） |
| `Ctrl+D` | 空输入时退出 |
| 鼠标拖选 | 按行选择上方内容，松开 OSC52 复制 |

斜杠指令：`/help` `/model` `/resume` `/plan` `/goal` `/save` `/exit`。

## 计划与审批模式

- **计划模式**：模型只调研不改动（write/edit/bash 等被硬拦截），产出计划后
  经 `exit_plan_mode` 呈交审批；批准即退出执行，拒绝可带反馈继续计划。
  模型也可经 `enter_plan_mode` 主动申请进入。
- **审批模式**：每个工具执行前弹确认（可附文字反馈，模型据此调整方案）。

## 会话持久化

每回合自动落盘 `.grf/sessions/`（含完整消息与 system 提示词原文）；
`gh -resume` 或全屏内 `/resume` 从列表恢复。`/save` 显式保存当前会话。
会话文件含敏感内容，权限 0600，不要提交进仓库。

## 微信渠道

```sh
gh connect weixin       # 扫码登录后转后台守护进程（日志 ~/.grf/weixin.log）
gh disconnect weixin    # 停止
```

- **发送者白名单**：默认拒绝所有消息。首次收到未授权消息会回执指引
  （附你的微信 ID），把 ID 加进 `settings.json` 的 `weixin.allow_senders`
  即可授权
- 渠道默认工作区 `~/.grf/weixin/ws`、默认策略 `write-workspace|read-workspace`
  （`weixin.policy` 覆盖）；工具审批经微信问答转发（超时按拒绝）
- 运行中发纯文本 = 插话并入当前任务；图片/文件回队下一轮处理；
  `/new` 重开会话

## 工具速查

28 个工具的定义（名称/描述/参数 schema）唯一来源是 `tools/spec/tools.json`
（编译期内嵌）；嵌入方可经 `RegisterCustom` 扩展、经 `DisabledTools` 裁剪。

| 类别 | 工具 |
|---|---|
| 文件 | `read`（行号/分页）、`write`、`edit`（唯一匹配/replace_all）、`glob`（mtime 序/100 条内联）、`grep`（250 条内联/include 过滤）、`read_image`（PNG/JPEG/GIF 解码缩放，WebP 透传） |
| 执行 | `bash`（workdir/timeout/exit code 标记/长输出截尾落盘/沙箱与越权即问/超时晋升后台作业）、`job_output` / `job_kill` / `job_list`（后台作业，流式增量读取，输出缓冲 8MB 上限） |
| 网络 | `web_search`（1-4 查询合并，DDG 摘要 + 来源列表） |
| 记忆 | `memory`（跨会话 JSON 条目记忆：项目级默认落工作区 `.grf/memory.json`，用户级落 `~/.grf/memory.json`；list/find/add/update/delete，key 自选唯一，find 按 key 精确/子串、内容子串三级检索，单文件 4KB 上限） |
| 交互 | `ask_user_question`（表单）、`enter_plan_mode` / `exit_plan_mode`（计划审批）、`todo_write`（面板展示）、`skill`（SKILL.md 目录）、`set_policy`（模型拉起弹窗说明理由，用户亲自选择，仅本会话生效） |
| 目标 | `create_goal` / `get_goal` / `update_goal`（revision 校验、blocked 自动续跑） |
| 编排 | `subagent` / `subagent_fork`（后台默认+落定通知）、`send_message` / `interrupt_agent` / `list_agents`、`workflow`（JS 脚本 + 并发/总量上限 + schema 校验 + 资源护栏）、`ralph`（新鲜代理循环） |
| 插件 | 由 MCP 插件动态注册（grf-browser 17 个 / grf-computer 10 个，见[插件文档](plugins.md)） |
