# grf-harness 接入文档

面向要把 grf 作为 agent 能力嵌入自家应用的宿主程序。grf 零第三方依赖、
无 `internal/` 目录，所有包（模块路径 `git.gorun.fun/share/grf-harness`）
均可被外部 Go 项目导入。非 Go 宿主可走 HTTP（`gh server`，见
[server.md](server.md)）或 CLI 子进程（§10）；架构总览见
[architecture.md](architecture.md)。

## 1. 引入方式

模块路径与公开仓库地址一致：`git.gorun.fun/share/grf-harness`
（https://git.gorun.fun/share/grf-harness），版本 tag 形如
`v0.0.1-alpha.2`（首个携带本模块路径的 tag）。

**方式一：直接 go get**（宿主机器可访问 git.gorun.fun）

```sh
# 内网 git 服务：让 go 跳过公共模块代理与校验和数据库
go env -w GOPRIVATE=git.gorun.fun

go get git.gorun.fun/share/grf-harness@v0.0.1-alpha.2
```

```go
import "git.gorun.fun/share/grf-harness/app"
```

git 拉取认证走宿主机器的常规 git 配置（https 凭据 / ~/.netrc / ssh）；
服务端若是自签证书，把 CA 指给 git：`git config --global http.https://git.gorun.fun.sslCAInfo /path/to/ca.pem`。

**方式二：本地路径 replace**（联调 / 离线；克隆或软链到本地后指向目录）

```
require git.gorun.fun/share/grf-harness v0.0.0

replace git.gorun.fun/share/grf-harness => /path/to/grf-harness
```

由于 grf 零依赖，两种方式都不会向宿主 `go.mod` 引入任何第三方 require。

## 2. 库接入：app.New

`app` 包是嵌入方的门面，一次构建拿到完整 harness（agent 循环、26 个
工具、会话存储）：

```go
import "git.gorun.fun/share/grf-harness/app"

a, err := app.New(app.Options{
    Workspace:     "/path/to/工作区",       // 默认 cwd
    Client:        client,                  // *llm.Client；nil 时读 ~/.grf/settings.json
    Host:          host,                    // tools.Host；nil 时 NoopHost
    MaxSteps:      100,                     // 工具调用轮次上限；0 用配置/默认
    SystemPrompt:  "你是 XX 场景的助手……",  // 见 §3
    DepthLimit:    3,                       // 子代理深度上限
    DisabledTools: []string{"bash"},        // 见 §4
    Skills:        tools.NewSkillCatalog(), // 接管/关闭技能加载；见 §6
    VerboseEvents: func(kind, detail string) { /* 见 §5 */ },
    OnToolEvent:   func(e agent.ToolEvent) { /* 见 §5，重建工具时间线用 */ },

    // 运行策略覆盖：仅本次构建生效、不落盘；不设则沿用配置文件
    Sandbox:     "workspace-write",         // "off" | "workspace-write"
    FsPolicy:    "observe",                 // "observe" | "off"
    AutoApprove: &autoYes,                  // *bool；非交互自动批准
})
if err != nil { ... }
defer a.Close()

err = a.Harness.Root().Run(ctx, "帮我把今天的报表导出")
```

策略覆盖让嵌入方**不依赖机器全局配置**也能定死沙箱/审批行为；未覆盖的
项仍读 `~/.grf/settings.json`。环境变量 `GRF_HOME` 可重定位全局配置与
用户级技能（`settings.json`、`~/.grf/skills`）；**会话存档不归它管**——
跟随 `Options.Workspace` 落在 `<workspace>/.grf/sessions/`（见 §9）。

需要更细控制（自定义 `config.Settings`、子代理工厂、goal 状态机）时
直接用 `agent.Build(agent.Options{...})`，字段与 `app.Options` 对应。

## 3. 注入默认提示词

优先级：`Options.SystemPrompt` > 配置文件 `system_prompt`（`gh config
prompt set` 落盘的那份）> 内置默认。非最高优先级非空者**替换**内置静态
提示词，但工作区路径、模型名、技能清单等
动态事实段仍会自动拼接（见 `agent.BuildSystemPrompt`）。

## 4. 工具：自定义、替换实现与禁用

- **注册新工具**：`tools.Registry.RegisterCustom(def llm.Tool, h Handler)`
  是嵌入方的扩展入口——工具定义（名称/描述/参数 Schema）由调用方完整
  给出，随请求发给模型。名字不得与内置 26 个冲突（spec 是内置工具定义
  的唯一来源，不允许借道覆盖），违反即 panic。

  ```go
  a.Root().Reg().RegisterCustom(llm.Tool{
      Name:        "export_report",
      Description: "导出当前会话报表到指定格式",
      Parameters:  map[string]any{"type": "object", "properties": map[string]any{
          "format": map[string]any{"type": "string"},
      }},
  }, func(ctx context.Context, args string) (tools.Result, error) {
      return tools.TextResult("exported"), nil
  })
  ```
- **替换内置实现**：对根 agent 的 registry 重新 `Register` 同名工具即可
  换掉内置 Handler。注意：`tools.Registry.Register` 要求名字必须在内置
  spec 的 26 个之内，未知名 panic。

  ```go
  a.Root().Reg().Register("web_search", myHandler) // 前提：该工具已注册
  ```
- **禁用**：`Options.DisabledTools` 按名剔除（内置与自定义统一生效），
  根 agent 与子代理一致。被禁用的工具不随请求发给模型；若模型仍调用，
  `Execute` 返回 unknown tool 文本（不中断会话）。

  ```go
  DisabledTools: []string{"bash", "subagent", "web_search"},
  ```

- 运行期也可直接操作 registry：`tools.Registry.Disable(names...)`、
  `Registered()` 查看现存工具。

## 5. 事件与流式响应

三条互不冲突的回调链：

**结构化工具事件**（推荐嵌入方重建工具时间线用）——`Options.OnToolEvent`
（`app.New` 时挂到根 agent），或运行期 `Agent.SetOnToolEvent`。每次工具
调用按 CallID 推进状态机，`Input`/`Output` 均为**全量原文不截断**，展示
裁剪由宿主自理：

| 字段     | 含义                                                        |
|----------|-------------------------------------------------------------|
| `CallID` | 工具调用 ID（LLM 返回的 tool_call id；正文回退解析生成），进程内唯一 |
| `Name`   | 工具名                                                      |
| `Status` | `running` → `done` \| `error` \| `denied`（终态）           |
| `Input`  | 完整参数 JSON                                               |
| `Output` | 完整结果文本（`running` 时为空）                            |
| `Err`    | `Status=error` 时的错误信息（来自 `tools.Result.Err`）      |
| `AgentID`/`Label` | 发起调用的 agent（根为 `agent_root`/`main`）      |

`running` 在审批前发出（等待用户审批的卡片也处于进行中）；`denied` 表示
审批模式被拒，`Output` 携带拒绝原因与用户反馈。**子代理的工具事件也会
回流到根 agent 的回调**（用 `AgentID`/`Label` 区分来源），宿主可选择只
展示根会话。

**过程事件**（工具调用、结果、报错、todo/goal 变化）——`Options.VerboseEvents`
（`app.New` 时自动挂到根 agent），或运行期 `Agent.SetOnEvent`，`kind` 取值：

| kind     | 含义                                |
|----------|-------------------------------------|
| `tool`   | 工具调用开始（detail: 名字+参数摘要）|
| `result` | 工具结果首行（错误也在结果文本里）   |
| `done`   | 一次 Run 结束                       |
| `notice` | 系统通知（子代理落定等）             |
| `todos` / `goal` / `plan` / `phase` | 状态变化，detail 为摘要或 JSON |

字符串事件适合行式日志；需要调用级关联与终态判定（进行中→完成/失败
卡片、展开看输出）请用上面的 `OnToolEvent`。

**流式文本**——`Agent.SetStreamHooks(content, reasoning func(string))`，
逐 token 回调正文与思考过程。宿主在回调里转发到自己的 UI / 消息总线即可。

注意：

- 回调在 agent 的运行 goroutine 上触发，宿主侧自行保证回调线程安全；
  不要在回调里直接阻塞（长处理请投递到 channel）。
- `app.RunTask` 会为根 agent 挂上自己的 stdout 渲染，**覆盖**之前设置的
  OnEvent/StreamHooks——需要自定义回调的嵌入方直接用
  `Root().Run / RunAutoGoal` 驱动，不要经过 `RunTask`。

## 6. 注入 Skill（工作区目录方式）

Skill 就是 `<name>/SKILL.md` 目录，无其他格式要求。在 `app.New` **之前**
写入工作区即可，`agent.Build` 的 `LoadSkills` 只扫一次、不热加载：

```
<workspace>/.grf/skills/报表导出/SKILL.md
```

两级目录，工作区优先（同名覆盖全局）：

1. `<workspace>/.grf/skills/<name>/SKILL.md` — 项目级
2. `~/.grf/skills/<name>/SKILL.md` — 用户全局

SKILL.md 首行写 `# 标题 + 一句话用途`（system prompt 清单摘要取首行），
正文写触发条件与操作步骤。技能加载后自动出现在 system prompt 清单中，
模型按需调用 `skill({"name": "报表导出"})` 取全文执行，宿主无需接线。

**接管技能目录**（`Options.Skills`）：不设时按上述两级目录自动加载；
传入 `*tools.SkillCatalog` 则**完全替换**默认加载——传空目录即关闭技能
（提示词不注入清单），或用 `NewSkillCatalog().Add` 从宿主自带来源编程式
装配：

```go
// 关闭默认技能加载（不读工作区与 ~/.grf/skills）
a, _ := app.New(app.Options{ Skills: tools.NewSkillCatalog() })

// 或自带技能：不依赖任何目录约定
cat := tools.NewSkillCatalog()
cat.Add("报表导出", reportSkillMD)
a, _ := app.New(app.Options{ Skills: cat })
```

宿主安装自带技能示例：

```go
func installSkills(workspace string, skills map[string]string) error {
    for name, md := range skills {
        dir := filepath.Join(workspace, ".grf", "skills", name)
        if err := os.MkdirAll(dir, 0o755); err != nil { return err }
        if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o644); err != nil {
            return err
        }
    }
    return nil
}
```

## 7. 模型管理

- **配置文件层**：`config.LoadFileForUpdate()` 拿可写 `Settings` →
  `Provider.AddModel(model)`（去重）→ `Settings.Save()` 落盘
  `~/.grf/settings.json`（受 `GRF_HOME` 重定位）。TUI 模型配置页
  （`tui.RunModelPage()`，无 app 依赖可直接调用）即此用法。
- **运行时指定**：直接 `llm.NewClient(llm.Config{BaseURL, APIKey, Model})`
  塞进 `Options.Client`，绕过配置文件。
- **会话内切换**：`a.SwitchModel(provider, model)`（缓存键随会话迁移）。

## 8. 编程式驱动（进阶）

以下入口都已导出，适合宿主自己写事件循环而不是复用 CLI/TUI：

- **多模态输入**：`Agent.RunWithImages(ctx, text, []tools.Image)` ——
  用户消息带图像（data URL）发送；无图像退化为 `Run`。
- **注入通知与自动续跑**：`Agent.EnqueueNotice(text)` 向会话注入系统
  通知；agent 空闲时触发 `Agent.OnIdleNotice` 回调——宿主注册它即可实现
  "后台事件到了自动让 agent 续跑"（消息渠道/机器人场景）。
- **goal 续跑与后台收尾**：`Agent.RunAutoGoal(ctx, input)` 执行一轮并按
  goal 状态自动续跑；一轮结束后 `Agent.RunAndWaitBackground(dur)` 等待
  后台子代理落定并取回通知（`app.RunTask` 即此组合）。
- **逐工具审批**：`Agent.SetApproveMode(true)` 后每次工具调用先经
  `Host.ApproveTool(ctx, name, args)` 征求同意——`args` 为**全量参数
  JSON（不截断）**，宿主自行做展示裁剪。返回 `tools.ApproveDecision{Approved, Feedback}`：
  拒绝时 `Feedback` 可携带用户反馈（如"别动这个文件，改用 …"），连同
  拒绝原因作为工具结果回给模型。
- **用量统计**：`Agent.LLMStats()`（轮次/首 token/生成耗时/tokens/缓存
  命中率）与 `Agent.ToolStats()`（调用数/累计耗时），UI 或计量用。
- **编程式子代理**：`Harness.Manager` 的 `Subagent/Fork/SendMessage/
  Interrupt/WaitForSettled/ListAgents`——不经过模型直接驱动子代理树。
- **提示词缓存**：`Harness.CacheKey()/SetCacheKey()` 会话级
  prompt_cache_key，随会话文件持久化，resume 自动复用（`app.Restore`
  已处理）。

交互宿主可用 `app.NewTaskHost(autoApprove)`（stdin/stdout 交互，自动
探测 TTY）或组合 `tools.NoopHost` 自己实现 `tools.Host` 五方法接口
（AskUser / ApproveEscalation / ApprovePlan / ApproveTool / NotifyEvent）。

## 9. 多会话（多对话并存）

宿主要有多个并行对话（多 tab 聊天、多用户机器人）时：**一个对话 = 一个
`app.New`（Harness）实例**。全部会话状态都在实例内、实例之间不共享任何
内存状态，各对话可在各自 goroutine 上并发 `Run`。每实例独占的资源：

| 状态                | 说明                                                          |
|---------------------|---------------------------------------------------------------|
| 对话历史/通知/todo  | Agent 内部锁保护，按实例隔离                                  |
| 后台作业            | JobManager 每 agent 一个，按对话隔离                          |
| observe 读集        | FsPolicy 每实例记账：对话 A read 过的文件不放行对话 B 的覆写  |
| 产物目录            | `os.TempDir` 下每 harness 独立（`Close()` 清理）              |
| 会话存档指针        | `Store` 的当前文件指针每实例独立，AutoSave 各覆写各的文件      |

关闭对话 = `a.Close()` + 从对话表删除。仓库内现成参考：`weixin` 的
`agentPool`（`map[用户]*Agent`，首次触达建 harness、`/new` 即丢弃）。

### 持久化归属与共享 workspace 的注意点

会话文件为 `<workspace>/.grf/sessions/session-<时间戳>.json`，**跟随
`Options.Workspace`**（`GRF_HOME` 不搬会话）。`NewSession`/`Restore`
之后下次保存另起新文件。多个对话共享同一 workspace 时注意：

- 文件名时间戳只精确到**秒**：两个对话的首次落盘落在同一秒会生成同名
  文件、交错覆写。
- `ListSessions` / `LatestSessionFull` 枚举目录下**全部**会话——跨对话
  混排，文件名不含对话 ID，"最近一场"在多对话下无意义。
- `a.AutoSave()` 不返回路径；要建对话↔文件索引请自己调 `a.SaveSession()`
  （返回文件路径）。
- 没有会话删除 API；关闭对话会残留存档文件（无功能影响）。

### 推荐接法

| 接法 | 做法 | 适用 |
|------|------|------|
| 每对话独立 workspace（推荐） | `Workspace: <appData>/conversations/<对话ID>` | 会话/技能/读集全隔离；恢复 = 读该对话目录下最新一场 |
| 共享 workspace + 自建索引 | 每回合调 `a.SaveSession()` 拿路径存进自己的库；恢复用 `store.LoadSessionFull(path)` + `a.Restore(sess)` | 需要对话间共享工作区文件；注意上面的同秒冲突 |
| 绕开 grf 会话文件 | 快照 `a.Root().History()` 自存，重启后 `RestoreFull` 回放 | 宿主已有对话持久化；缓存命中语义 grf 已保证（system 原文 + CacheKey 随消息回放） |

每对话独立 workspace 的最小骨架：

```go
convs := map[string]*app.App{} // 对话ID → harness

func openConv(id string) (*app.App, error) {
    a, err := app.New(app.Options{
        Workspace: filepath.Join(appData, "conversations", id), // 存档/技能/读集随对话隔离
        Client:    client, // OnToolEvent 见 §5
    })
    if err != nil {
        return nil, err
    }
    // 跨重启恢复：该对话目录下最新一场
    if infos := a.Harness.Store.ListSessions(1); len(infos) > 0 {
        if sess, err := store.LoadSessionFull(infos[0].Path); err == nil && len(sess.Messages) > 0 {
            a.Restore(sess)
        }
    }
    return a, nil
}

func closeConv(id string) {
    if a := convs[id]; a != nil {
        a.Close() // 清产物目录；存档文件保留供下次恢复
        delete(convs, id)
    }
}
```

## 10. 非 Go 宿主：HTTP 服务与 CLI 子进程

两条路径，按结构化程度选择：

**HTTP Responses API（推荐，结构化）**：`gh server` 把完整 agent 能力以
OpenAI Responses 形状暴露（ORS 2026-04-24 兼容；SSE 流式、client
function tools、ask_user/审批的暂停应答链式续交、多会话、跨重启恢复）。
任意语言的 OpenAI SDK 指向 `base_url` 即可驱动，端点/参数/事件语义见
[server.md](server.md)。

**CLI 子进程（轻量）**：

```sh
./gh "任务"   # 单任务直出，跑完退出
```

非 TTY 环境自动回退纯文本行模式，可在脚本/CI/服务端调用，解析 stdout
即可。会话历史落在 `<workspace>/.grf/sessions/`。注意事件与正文混排在
stdout（无结构化边界）——需要结构化输出请用上面的 HTTP 路径或 Go 库方式。

## 11. 注意事项

- **并发**：agent 历史一律经内部锁；宿主若自建 UI，事件请从
  `VerboseEvents` 投递到单一 goroutine 消费（参考 tui 的 events 通道模式）。
- **路径**：文件工具的相对路径相对**工作区**（`Options.Workspace`），
  不是进程 CWD。
- **观察策略**：默认 `fs_policy=observe` 下，write/edit 覆写已存在文件
  前要求本会话 read 过；新建文件不受限。
- **回话文本**：回复不做 markdown 渲染，事件/结果均为原始文本。
- **会话持久化**：`a.SaveSession()` 手动保存；`a.AutoSave()` 回合结束
  自动落盘；`a.Restore(sess)` 续接历史（含 system 原文与缓存键回放）。
  多对话并存的隔离与索引见 §9。
- **可暂停回合（跨请求应答）**：`Agent.RunTurn / RunTurnParts` 返回
  `(reply, *PausedTurn, error)`，暂停点（client 工具 / ask_user / 审批）
  用 `Agent.Resume(ctx, []ResumeInput)` 续跑；暂停态持久化在历史尾部，
  进程重启后 `RestoreFull` + `Resume` 照常续接。`gh server` 即此用法
  （wire 形状见 server.md §6）。
- **参考实现**：`weixin` 包（`weixin.Run(ctx, prompt, workspace)`）是
  一个完整的第三方嵌入样板——自含配置加载、client 构建、每用户独立
  harness 的常驻消息服务；`server` 包是第二个（HTTP 形态、磁盘为真的
  多会话、暂停应答链），二者都是"四表面并列直连 agent.Build"架构的
  实例（见 architecture.md §3.3）。
