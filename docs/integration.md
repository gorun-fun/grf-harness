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
})
if err != nil { ... }
defer a.Close()

err = a.Harness.Root().Run(ctx, "帮我把今天的报表导出")
```

需要更细控制（自定义工具 Handler、子代理工厂、goal 状态机）时直接用
`agent.Build(agent.Options{...})`，字段与 `app.Options` 对应。

## 3. 注入默认提示词

`Options.SystemPrompt` 非空时**替换**内置静态提示词（结构镜像
`docs/prompt.md`），但工作区路径、模型名、技能清单等动态事实段仍会自动
拼接（见 `agent.BuildSystemPrompt`）。传空串则用内置提示词。

## 4. 工具：替换实现与禁用

- **替换实现**：`app.New` 之后对根 agent 的 registry 重新 `Register`
  同名工具即可换掉内置 Handler。注意：`tools.Registry.Register` 要求名字
  必须在 `tools/spec/tools.json` 的 25 个之内，不能凭空加新工具名。

  ```go
  a.Harness.Root().Reg().Register("web_search", myHandler) // 前提：该工具已注册
  ```
- **禁用**：`Options.DisabledTools` 按名剔除，根 agent 与子代理统一生效。
  被禁用的工具不随请求发给模型；若模型仍调用，`Execute` 返回 unknown
  tool 文本（不中断会话）。不存在的名字静默忽略。

  ```go
  DisabledTools: []string{"bash", "subagent", "web_search"},
  ```

- 运行期也可直接操作 registry：`tools.Registry.Disable(names...)`、
  `Registered()` 查看现存工具。

## 5. 事件与流式响应

两条互不冲突的回调链：

**过程事件**（工具调用、结果、报错、todo/goal 变化）——`Options.VerboseEvents`
或 `Agent.SetOnEvent`，`kind` 取值：

| kind     | 含义                                |
|----------|-------------------------------------|
| `tool`   | 工具调用开始（detail: 名字+参数摘要）|
| `result` | 工具结果首行（错误也在结果文本里）   |
| `done`   | 一次 Run 结束                       |
| `notice` | 系统通知（子代理落定等）             |
| `todos` / `goal` / `plan` / `phase` | 状态变化，detail 为摘要或 JSON |

**流式文本**——`Agent.SetStreamHooks(content, reasoning func(string))`，
逐 token 回调正文与思考过程。宿主在回调里转发到自己的 UI / 消息总线即可。

注意：回调在 agent 的运行 goroutine 上触发，宿主侧自行保证回调线程安全；
不要在回调里直接阻塞（长处理请投递到 channel）。

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
  `~/.grf/settings.json`。TUI 模型配置页（`tui/modelpage.go`）即此用法。
- **运行时指定**：直接 `llm.NewClient(llm.Config{BaseURL, APIKey, Model})`
  塞进 `Options.Client`，绕过配置文件。

## 8. 非 Go 宿主：CLI 子进程

```sh
./gh "任务"   # 单任务直出，跑完退出
```

非 TTY 环境自动回退纯文本行模式，可在脚本/CI/服务端调用，解析 stdout
即可。会话历史落在 `<workspace>/.grf/sessions/`。

## 9. 注意事项

- **并发**：agent 历史一律经内部锁；宿主若自建 UI，事件请从
  `VerboseEvents` 投递到单一 goroutine 消费（参考 tui 的 events 通道模式）。
- **路径**：文件工具的相对路径相对**工作区**（`Options.Workspace`），
  不是进程 CWD。
- **观察策略**：默认 `fs_policy=observe` 下，write/edit 覆写已存在文件
  前要求本会话 read 过；新建文件不受限。
- **回话文本**：回复不做 markdown 渲染，事件/结果均为原始文本。
- **会话持久化**：`a.SaveSession()` 手动保存；`a.AutoSave()` 回合结束
  自动落盘。
