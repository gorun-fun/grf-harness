# 配置参考

**简体中文** | [English](configuration.en.md)

`~/.grf/settings.json` 全字段、policy 规则串语义、环境变量一览与
微信渠道配置。取值优先级一律为**环境变量 > 配置文件 > 内置默认**。

## settings.json

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
  "system_prompt": "",
  "max_steps": 0,
  "policy": "write-workspace|read-workspace",
  "auto_approve": false,
  "compact_threshold": 0,
  "weixin": {
    "allow_senders": ["wxid_xxxx"],
    "policy": ""
  },
  "plugins": []
}
```

| 字段 | 说明 |
|---|---|
| `providers` | 模型提供商表。键为名称，值含 `base_url`（OpenAI 兼容端点）、`api_key`、`models`（可选模型列表） |
| `default` | 默认模型，形如 `"provider/model"`；未设时取字典序第一个供应商的首个模型 |
| `system_prompt` | 自定义身份/工作方式段（custom 段）；空用内置默认。inner prompt（工具说明）恒定拼装，不被它替换 |
| `max_steps` | 单回合最大步数；0 用内置默认 |
| `policy` | 统一规则串（见下节）。空值默认 `off`（全关）；非法值回退 `write-workspace` |
| `auto_approve` | 非交互（单任务/嵌入）模式下自动批准沙箱升级与计划。微信渠道恒不生效（审批走微信问答） |
| `compact_threshold` | 会话自动压缩阈值（估算 token）：0 用默认（100k），正数覆盖，负数关闭 |
| `weixin` | 微信渠道安全配置（见下文专节） |
| `plugins` | MCP 插件接入配置；缺省键 = 自动发现 `~/.grf/plugins/` 下可执行文件，`[]` = 关闭自动发现，非空 = 仅使用列出的项。详见[插件文档](plugins.md) |

## policy 规则串

竖线组合的统一规则串（`config.ParsePolicy` 解析）：

| token | 写侧 | 读侧 |
|---|---|---|
| `write-workspace` | bash 用 macOS sandbox-exec 限写工作区（**仅 macOS 有进程级沙箱**）；write/edit 覆写前须先 read（新建文件不受限） | 不限制 |
| `read-workspace` | 不限制 | read/glob/grep/read_image 仅可访问工作区内路径（硬拒绝，无升级协议） |
| `off` / 空值 | 关（写边界仍无条件生效：文件工具只写工作区与已授权目录） | 关 |

- 越界即问：写边界或 bash 沙箱拒绝时挂起征求用户批准（`Host.ApproveEscalation`），
  批准 = 当次放行（bash 脱沙箱重跑同一命令），拒绝 = 终局（带
  `sandbox_permissions` 重试也不会二次询问）
- 授权目录：write/edit 的越权审批可选 GrantDir，持久化到工作区
  `.grf/grants.json`（仅本工作区生效），该目录后续写不再询问。该文件禁止
  模型侧写入；随仓库分发的授权会在系统提示词中披露
- `set_policy` 工具：模型只能拉起弹窗并附理由，用户从预制选项亲自选择，
  仅本会话生效不落盘；收紧时全树同步到已存在的子代理

## 模型名解析

`default` / `GRF_MODEL` / 会话层切换共用一套规则：

- **限定名** `provider/model`：供应商须存在；模型在其 `models` 表内即切换，
  不在表内但供应商已知则信任显式覆盖（测试新模型免登记）
- **裸名**：默认供应商含该模型优先；否则全表恰一家命中时切换；歧义
  （多供应商同名且默认供应商不含）或零命中沿用当前

## 环境变量一览

| 变量 | 作用 | 默认 |
|---|---|---|
| `GRF_HOME` | 配置根目录（settings/plugins/sessions/weixin 状态等全在此） | `~/.grf` |
| `GRF_MODEL` | 模型覆盖（裸名 / 限定名 / 整串透传，见上节） | — |
| `OPENAI_BASE_URL` | LLM 端点最终覆盖（优先级最高） | provider 的 `base_url` |
| `OPENAI_API_KEY` | LLM 密钥最终覆盖（优先级最高） | provider 的 `api_key` |
| `GRF_MOUSE` | 全屏模式鼠标初始状态；`0`/`false` 关闭 | 开启 |
| `GRF_DOWNLOAD_BASE` | 自更新与插件下载基址 | `https://download.gorun.fun/grf-harness` |
| `GRF_SERVER_ACCESS_LOG` | 设为 `off` 关闭 `gh server` 访问日志 | 开启 |
| `GRF_TURN_WEBHOOK_URL` | `gh server` 回合终态事件外发地址（POST `turn.settled`） | 空 = 不外发 |
| `GRF_TURN_WEBHOOK_SECRET` | webhook 的 `X-GRF-Signature` HMAC-SHA256 签名密钥 | 空 = 不签名 |

**子进程环境剥离**：bash 与 MCP 插件子进程的环境会剔除名称疑似密钥的变量
（大小写不敏感匹配 `api_key` / `apikey` / `secret` / `token` / `password` /
`passwd` / `credential` 命名族，实现见 `tools/env.go` 与 `mcp/stdio.go`）。
确需传递时在命令内自行 `export`。

## 微信渠道配置

```json
"weixin": {
  "allow_senders": ["wxid_xxxx"],
  "policy": ""
}
```

- `allow_senders`：允许驱动 agent 的微信用户 ID 白名单。**空 = 拒绝所有
  消息**（默认安全侧）；未授权发送者会收到含其 ID 的配置指引回执
  （每发送者 60 秒至多一条）
- `policy`：渠道策略覆盖；空 = `write-workspace|read-workspace`（比全局
  默认 off 更紧——微信消息是任意外部输入）
- 全局 `auto_approve` 不带入微信渠道：工具审批（逐工具/沙箱升级/计划）
  一律经微信问答转发，等待超时按拒绝处理
- 渠道默认工作区 `~/.grf/weixin/ws`（`-workspace` 覆盖），入站媒体下载
  仅接受 `*.weixin.qq.com` 域

## 其他落盘文件

| 路径 | 内容 | 权限 |
|---|---|---|
| `~/.grf/settings.json` | 配置（含 api_key） | 0600，原子写 |
| `~/.grf/history` | 输入历史 | — |
| `~/.grf/sessions/`（全局）/ `<工作区>/.grf/sessions/` | 会话转录（可能含敏感内容） | 0600 |
| `<工作区>/.grf/memory.json` | 项目级跨会话记忆（随仓库分发按不可信对待，系统提示词会披露） | 0600 |
| `~/.grf/memory.json` | 用户级跨会话记忆 | 0600 |
| `<工作区>/.grf/grants.json` | 越工作区写授权目录（禁止模型侧写入，加载时披露） | 0600 |
| `~/.grf/server/` | `gh server` 数据目录（token / 会话盘 / 快照） | token 0600 |
| `~/.grf/weixin.log` / `weixin.pid` / `weixin_state.json` | 微信守护进程日志 / PID / 登录态 | 0600 |
