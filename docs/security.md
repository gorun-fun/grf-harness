# 安全模型

**简体中文** | [English](security.en.md)

gh 驱动的是**本机 bash 与文件工具**——它的能力边界就是它的安全边界。
本文是完整的安全模型速览：威胁面、各渠道默认姿态与使用须知。
配置字段细节见[配置参考](configuration.md)。

## 威胁模型

| 通道 | 谁能驱动 agent | 默认防线 |
|---|---|---|
| 终端 CLI / Go 嵌入 | 本机用户 / 嵌入方 | policy 规则串（沙箱/边界/审批） |
| `gh server`（HTTP） | 持有 Bearer token 的客户端 | 默认仅绑 `127.0.0.1:7400` + 常时比较鉴权 |
| 微信渠道 | **白名单内的微信用户** | 默认拒绝所有发送者；工作区与策略独立收紧 |
| MCP 插件 | 安装了插件的 harness | 插件与主程序同权限运行（见下文） |

**token 持有者 = 受控的本机 RCE**：这是 `gh server` 的既定语义（docs/server.md
§1），环回默认 + 鉴权是其全部边界；绑非环回地址等于把本机执行能力暴露给
网络，务必自行加防护。

## 通道默认姿态

### 终端与嵌入

- policy 未配置时为 `off`（无沙箱）。建议显式配置 `write-workspace`
- **macOS 独有进程级 bash 沙箱**（sandbox-exec 限写工作区）；**Linux/Windows
  没有进程级 bash 沙箱**——write/edit 文件工具的边界全平台生效，但 bash
  命令在这两个平台不受 OS 级限制，越权仅靠升级审批问询
- 越界即问：拒绝记录一次性消费，拒绝即终局；`sandbox_permissions` 升级
  重试必须携带 justification 且精确匹配被拒命令
- 子代理继承根策略，无升级能力（构造性拒绝）；`set_policy` 收紧时全树同步

### gh server

- 默认 `127.0.0.1:7400`，全部端点 Bearer 鉴权（`subtle.ConstantTimeCompare`），
  token 为 24 字节 crypto/rand hex，落盘 0600
- 请求体限额（主端点 64MB / 配置端点 1MB）、ReadHeaderTimeout/ReadTimeout/
  IdleTimeout；访问日志不含凭据与请求体
- 客户端 function tools 不在本地执行（转入暂停协议，由接入方执行后回传）；
  回合终态 webhook（`GRF_TURN_WEBHOOK_URL`）带 HMAC-SHA256 签名，密钥
  经环境变量注入

### 微信渠道

- **发送者白名单默认拒绝**（`weixin.allow_senders`）；未授权回执含配置指引，
  按发送者 60 秒节流
- 渠道默认工作区 `~/.grf/weixin/ws`（不默认放开整个 home）、默认策略
  `write-workspace|read-workspace`（`weixin.policy` 覆盖）
- 全局 `auto_approve` 不带入：逐工具/升级/计划审批经微信问答转发，
  超时按拒绝
- 入站媒体下载钉域 `*.weixin.qq.com`（防 full_url SSRF）；守护进程日志与
  登录态 0600（日志含消息原文与登录二维码链接）

### MCP 插件

- 插件进程与主程序**同权限**运行：读写与命令执行不经过文件工具边界与
  bash 沙箱。只安装可信来源的插件
- 插件远程模式（`serve --addr`）**强制 Bearer token**（`--token` 缺省自动
  生成随机值并打印），服务端不发 CORS 放行头（防任意网页 fetch 环回端口
  驱动浏览器/电脑插件）
- 插件子进程环境剔除疑似密钥变量（与 bash 同规则）
- `gh plugins download` 下载的文件名校验（拒绝路径分量）；官方插件与自更新
  共用 HTTPS + 同目录 SHA256SUMS 校验

## 信任边界文件（工作区随附 = 不可信输入）

| 文件 | 风险 | 处置 |
|---|---|---|
| `.grf/grants.json` | 越工作区写**预授权**（跳过审批） | 加载时披露进系统提示词；`/` 与家目录级条目拒载；模型侧写入硬拒 |
| `.grf/memory.json` | 跨会话跨用户的提示注入载体 | 加载时披露"来源不可信"；执行其中指令前须征得用户确认 |
| `.grf/sessions/` | 会话转录（可能含密钥等敏感内容） | 0600；不要提交进仓库 |
| `SKILL.md` / 默认技能全文 | 注入系统提示词 | 仅在显式配置为默认技能时注入（嵌入方信任决策） |
| `grf:session.host_context` | 以 system 位置注入的宿主简报 | 嵌入方语义：宿主必须保证其内容不含未消毒的外部输入 |

## 进程与供应链

- **子进程环境剥离**：bash 与插件子进程剔除疑似密钥命名的环境变量
  （`*API_KEY*` / `*TOKEN*` / `*SECRET*` 等，`tools/env.go`）
- **自更新/插件下载**：HTTPS + 同目录 SHA256SUMS 完整性校验——防传输
  损坏，**防不了下载服务器自身失陷**（清单与产物同信道）；`GRF_DOWNLOAD_BASE`
  可改基址（私有部署/调试）
- **workflow JS 解释器资源护栏**：嵌套深度上限（防栈溢出致命）、步数预算
  与 ctx 取消观察（防自旋卡死）、字符串操作 16MB 上限、并发有界
- **后台作业输出缓冲** 8MB 上限（内存炸弹护栏）

## 部署检查清单

1. 显式配置 `policy`（不要依赖 `off` 默认）；Linux/Windows 记住 bash 无沙箱
2. `gh server` 保持环回绑定；绑外网前自行加 TLS 反代与访问控制
3. 微信渠道先配 `weixin.allow_senders` 再投入使用
4. 插件只装可信来源；远程模式用自动生成的 token 且只监听回环/内网
5. 处理不可信仓库时，先检查并按需删除其 `.grf/` 目录
6. 敏感环境变量不要依赖"反正子进程看不到"以外的传递方式——需要时在命令内
   显式 export
