# grf-harness 接入文档

面向要把 grf 作为 agent 能力嵌入自家应用的宿主程序。grf 零第三方依赖、
无 `internal/` 目录，所有包（模块路径 `grf`）均可被外部 Go 项目导入。

## 1. 引入方式

grf 尚未发布到模块仓库前，宿主 `go.mod` 用 replace 指向本地路径：

```
require grf v0.0.0

replace grf => /path/to/grf-harness
```

由于 grf 零依赖，replace 不会向宿主 go.mod 引入任何第三方 require。

## 2. 库接入：app.New

`app` 包是嵌入方的门面，一次构建拿到完整 harness（agent 循环、25 个
工具、会话存储）：

```go
import "grf/app"

a, err := app.New(app.Options{
    Workspace:     "/path/to/工作区",       // 默认 cwd
    Client:        client,                  // *llm.Client；nil 时读 ~/.grf/settings.json
    Host:          host,                    // tools.Host；nil 时 NoopHost
    MaxSteps:      100,                     // 工具调用轮次上限；0 用配置/默认
    SystemPrompt:  "你是 XX 场景的助手……",  // 见 §3
    DepthLimit:    3,                       // 子代理深度上限
    DisabledTools: []string{"bash"},        // 见 §4
    VerboseEvents: func(kind, detail string) { /* 见 §5 */ },

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
项仍读 `~/.grf/settings.json`（可用环境变量 `GRF_HOME` 把整个 `.grf`
目录重定位到宿主自己的位置，配置、技能、会话都随之迁移）。

需要更细控制（自定义 `config.Settings`、子代理工厂、goal 状态机）时
直接用 `agent.Build(agent.Options{...})`，字段与 `app.Options` 对应。

## 3. 注入默认提示词

优先级：`Options.SystemPrompt` > 配置文件 `system_prompt`（`gh config
prompt set` 落盘的那份）> 内置默认。非最高优先级非空者**替换**内置静态
提示词（结构镜像 `docs/prompt.md`），但工作区路径、模型名、技能清单等
动态事实段仍会自动拼接（见 `agent.BuildSystemPrompt`）。

## 4. 工具：自定义、替换实现与禁用

- **注册新工具**：`tools.Registry.RegisterCustom(def llm.Tool, h Handler)`
  是嵌入方的扩展入口——工具定义（名称/描述/参数 Schema）由调用方完整
  给出，随请求发给模型。名字不得与内置 25 个冲突（spec 是内置工具定义
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
  spec 的 25 个之内，未知名 panic。

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

两条互不冲突的回调链：

**过程事件**（工具调用、结果、报错、todo/goal 变化）——`Options.VerboseEvents`
（`app.New` 时自动挂到根 agent），或运行期 `Agent.SetOnEvent`，`kind` 取值：

| kind     | 含义                                |
|----------|-------------------------------------|
| `tool`   | 工具调用开始（detail: 名字+参数摘要）|
| `result` | 工具结果首行（错误也在结果文本里）   |
| `done`   | 一次 Run 结束                       |
| `notice` | 系统通知（子代理落定等）             |
| `todos` / `goal` / `plan` / `phase` | 状态变化，detail 为摘要或 JSON |

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
  `Host.ApproveTool(ctx, name, args)` 征求同意，拒绝以工具结果回给模型。
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

## 9. 非 Go 宿主：CLI 子进程

```sh
./gh "任务"   # 单任务直出，跑完退出
```

非 TTY 环境自动回退纯文本行模式，可在脚本/CI/服务端调用，解析 stdout
即可。会话历史落在 `<workspace>/.grf/sessions/`。目前没有 MCP/JSON-RPC
等结构化边界，事件与正文混排在 stdout，服务端集成建议走 Go 库方式。

## 10. 注意事项

- **并发**：agent 历史一律经内部锁；宿主若自建 UI，事件请从
  `VerboseEvents` 投递到单一 goroutine 消费（参考 tui 的 events 通道模式）。
- **路径**：文件工具的相对路径相对**工作区**（`Options.Workspace`），
  不是进程 CWD。
- **观察策略**：默认 `fs_policy=observe` 下，write/edit 覆写已存在文件
  前要求本会话 read 过；新建文件不受限。
- **回话文本**：回复不做 markdown 渲染，事件/结果均为原始文本。
- **会话持久化**：`a.SaveSession()` 手动保存；`a.AutoSave()` 回合结束
  自动落盘；`a.Restore(sess)` 续接历史（含 system 原文与缓存键回放）。
- **参考实现**：`weixin` 包（`weixin.Run(ctx, prompt, workspace)`）是
  一个完整的第三方嵌入样板——自含配置加载、client 构建、每用户独立
  harness 的常驻消息服务。
