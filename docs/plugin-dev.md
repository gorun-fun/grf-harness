# 插件开发规范

面向为 grf-harness 编写 MCP 插件的开发者：运行时契约、工具准入规则、
SDK 用法、协议子集、测试与分发。插件的安装配置与内置插件
（grf-browser / grf-computer）说明见 `docs/plugins.md`——那边讲"用"，
本文讲"怎么写、怎么写对"。文中行为条款均对应宿主（`plugin` / `mcp`
包）与插件侧 SDK（`plugins/internal/mcpserver`）的现行实现，不是愿景。

条款用词：**必须**（违反即被宿主拒绝或插件不可用）、**应该**（强烈
建议，偏离要有理由）、**可以**（任选）。

## 1. 形态与总原则

插件是**独立编译的可执行二进制**，作为 MCP 服务端向宿主提供工具；对
模型而言，插件工具与 28 个内置工具同等可见、同等调用。同一份二进制
支持两种模式：

| 模式 | 启动方 | 传输 |
|------|--------|------|
| stdio（本地） | 宿主直接 spawn（自动发现时无参数；settings.json `plugins[].command` 可带参数，如 `--headless=false`） | stdin/stdout 行分隔 JSON-RPC |
| HTTP（远程） | 人工/运维执行 `插件 serve --addr ...` | MCP Streamable HTTP（POST 单条 JSON-RPC） |

总原则：

- **接入零配置**：stdio 二进制放进 `~/.grf/plugins/`（可执行、非隐藏、
  非 `.md`、非 `.old*` 残留）即被自动发现；Windows 无可执行位，按后缀
  判定（.exe/.bat/.cmd/.com/.ps1）。显式配置走 settings.json 的
  `plugins` 键（键存在则只用配置项，不再自动发现）。
- **失败只降级**：连接失败只产生警告（进系统提示词，模型可见），
  单工具失败返回错误内容；插件崩溃不影响宿主与其他插件。
- **一份代码双模式**：工具实现注册一次，stdio 与 serve 模式同时可用
  ——用 SDK 自然获得，裸写也必须保证。

## 2. 运行时契约（宿主如何驱动插件）

stdio 模式完整时序：

```
宿主 spawn 插件（继承宿主全部环境变量）
  → initialize（声明协议版本 2025-06-18）
  ← initialize 应答（回显协商版本 + serverInfo.name/version）
  → notifications/initialized（通知，无应答）
  → tools/list（客户端自动翻页）
  ← 工具清单（宿主逐一准入，见 §4）
  …… 空闲；模型调用时：
  → tools/call {"name":..., "arguments":{...}}
  ← {"content":[...], "isError"?}
  …… 宿主退出：
  关闭插件 stdin → 插件应清理资源后退出（宿主等 3 秒，未退则强杀）
```

硬性时限与共享事实，插件**必须**据此设计：

| 项 | 值 | 来源 |
|----|----|------|
| 建连+握手+工具枚举整体时限 | 15 秒 | `plugin.handshakeTimeout` |
| 单次 tools/call 超时 | 默认 120 秒，settings.json `plugins[].timeout`（秒）可覆盖 | `plugin.DefaultToolTimeout` |
| stdin 关闭后的退出宽限 | 3 秒 | `mcp/stdio.go` 的 Close |
| 连接 | 进程级共享：gh server 每会话、weixin 每用户复用同一批连接 | `plugin.Shared` |

由此推出：

- **必须懒初始化重资源**：main 里只做注册，浏览器、显示缓存等重操作
  放到首个工具调用（参考 grf-browser 懒启动 chrome）——握手只有
  15 秒，阻塞它即连接失败。
- **必须尊重 ctx**：handler 收到的 ctx 在调用超时后取消，一切阻塞操作
  （命令执行、网络、等待）都要绑定 ctx，否则超时后插件仍被占住。
- **必须并发安全**：连接共享意味着多个会话可能并发调用同一工具；SDK
  只保证工具注册表并发安全，插件自己的共享状态（浏览器实例、屏幕
  信息缓存）须自行加锁或串行化。
- **必须清理孤儿资源**：宿主拉起的子进程、浏览器等在 stdin EOF 与
  SIGINT/SIGTERM 两条路径都要触发清理（范式见 §7.3——默认信号杀进程
  不会走 defer）。

## 3. stdio 纪律（最常踩的坑）

- **不得**向 stdout 写任何非 JSON-RPC 应答的内容。宿主客户端逐行解析、
  静默丢弃无法解析的行——调试输出打到 stdout 等于全部丢失，还污染
  协议流排查。
- **应该**把日志、panic 堆栈、启动失败原因写到 stderr：宿主环形截留
  尾部 8KB，插件进程异常退出时随错误带回用户（`插件进程已退出
  （stderr: ...）`），是唯一的死后诊断通道。
- stdin EOF 即退出信号：完成清理后正常退出（exit 0），不要滞留。

## 4. 工具准入规则（宿主侧硬校验）

`plugin.Manager` 对 tools/list 返回的每个工具做准入，不合规即跳过并
记警告：

1. 名字必须匹配 `^[a-zA-Z0-9_-]{1,64}$`。
2. 不得与内置 28 个工具重名（`tools/spec/tools.json`，唯一来源）。
3. 跨插件重名：先完成连接者生效——连接是并发的、顺序不定，**视为
   未定义行为**，必须靠前缀避免，不要指望顺序。

因此工具名**必须**自带命名空间前缀（`browser_*`、`computer_*`、
`demo_*`），前缀即插件身份。`inputSchema` 缺省时宿主会补
`{"type":"object"}`，但**应该**总是显式给出。

准入通过后，宿主对插件工具做的三件事，开发者**应该**知晓：

- 注册走 `tools.Registry.RegisterCustom`（与嵌入方自定义工具同一通道，
  不挤占内置 28 个的名字空间）。
- 描述尾部追加 `\n\n[grf 插件 <插件名>]`，模型据此区分工具来源。
- `annotations.readOnlyHint` 决定计划模式行为：已标注 → 放行；未标注
  → 一律按改动类拦截（`agent/turn.go` 的 `planBlocked`）。审批模式下
  插件工具与内置工具一样逐次经用户确认（`Host.ApproveTool`；gh server
  侧转为 `tool_approval` 暂停点）。

## 5. 工具定义规范

### 5.1 命名与描述

- 名字：`前缀_动词[_宾语]`，小写加下划线，对齐内置风格。
- 描述写给**模型**看，不是写给人：说清干什么、何时该用、关键语义
  （坐标系、返回形态、前置步骤）。grf-computer 的
  `computer_screenshot` 连"图上像素 ÷ scale + 屏幕起点 = 点击用全局
  坐标"都写进描述——模型只靠这段文字用对工具。

### 5.2 参数 Schema

- 标准 JSON Schema（object）；每个参数都带 `description`。
- 可选参数给出确定的省缺语义（如"省略=主屏"），模型对省缺行为的把握
  远好于穷举。
- 枚举用 `enum`；默认值、取值范围能写则写。

### 5.3 ReadOnly 标注

- 纯读操作（查询、截图、枚举）**应该**标 `ReadOnly: true`——计划模式
  下模型才能用它做调研。
- 有任何副作用（点击、写文件、改变远端状态）**不得**标注。标注不实
  会骗过计划模式硬拦截，属于安全缺陷。

### 5.4 返回内容

`[]Content` 只支持 `Text` 与 `ImagePNG`（多模态回传）：

- 文本**应该**精炼、结构化（键值/列表优于长段落）；多个 text 项宿主以
  `\n` 拼接后作为工具文本结果。
- 截图类结果**应该**组合返回：先 `Text` 给元信息（坐标范围、倍率
  换算），再 `ImagePNG(base64)` 给图，参照 `computer_screenshot`。
- 其他 content type（audio、resource 等）宿主按 `[type 内容省略]`
  占位——**不得**使用。
- handler 返回空内容时 SDK 补 `(no output)`——**应该**总是返回有意义
  的文本，哪怕一句"已完成，无输出"。

### 5.5 错误处理

- 工具执行失败：返回 `error`。SDK 转成 `isError: true` 的 content，
  错误文本回传模型，模型可据此修正参数或换路重试。
- **不得**依赖 panic 兜底（SDK 会收敛为 isError，但这是最后防线），
  **不得** `os.Exit`——进程死亡即整插件失效，本会话内不可恢复。
- 参数不合法**应该**直接返回带字段名的错误信息（模型会修正重试），
  不要静默容忍。

## 6. 用 SDK 还是裸写

SDK 位于 `plugins/internal/mcpserver`（`server.go` + `stdio.go` +
`http.go` 三个文件，纯标准库零依赖）：

| 场景 | 做法 |
|------|------|
| 官方插件（进本仓库） | 在 `plugins/` 模块内新开目录，直接 import SDK；把插件名加进 `plugins/build.sh` 的 `PLUGINS` 列表 |
| 外部插件（任意仓库） | SDK 是 Go internal 包，外部模块**无法** import——把三个文件整体拷进自己项目（零依赖、可独立编译），或按 §8 裸写协议子集 |

`plugins/` 模块（`git.gorun.fun/share/grf-harness/plugins`）是整个仓库
**唯一允许第三方依赖**的地方（现有 chromedp）；外部插件无此约束，
但同样建议保持低依赖：插件价值在工具面，不在依赖树。

## 7. SDK 参考

### 7.1 API 一览

| API | 用途 |
|-----|------|
| `New(name, version)` | 构建服务端；name/version 进 initialize 应答的 serverInfo（`gh plugins` 体检显示） |
| `s.Add(mcpserver.Tool{...})` | 注册工具（重名覆盖，插件自己保证不重） |
| `s.Names()` | 已注册工具名（字典序），装配自检用 |
| `mcpserver.Text(s)` / `ImagePNG(b64)` | 构造返回内容项 |
| `s.ServeStdio(ctx)` | 本地模式：stdin 读请求、stdout 写应答，EOF 返回 nil |
| `s.Serve(ctx, r, w)` | 行循环，可注入 reader/writer——协议测试用 |
| `s.ServeHTTP(ctx, addr, path, token)` | serve 模式：起 Streamable HTTP 端点（path 默认 `/mcp`），阻塞直至出错 |
| `s.Handler(token)` | 暴露 `http.Handler`——httptest 用 |
| `mcpserver.ParseServeArgs(args, defaultAddr)` | 解析 serve 的 `--addr` / `--token` / `--path` |
| `s.Dispatch(ctx, raw)` | 直接处理一条 JSON-RPC 消息——自定义传输用 |

SDK 内建行为（无需自己写）：handler panic 收敛为 isError；未知工具回
isError 并附可用工具清单；缺省 `arguments` 补空对象；`ping` 回空
result；协议版本协商（2024-11-05 / 2025-03-26 / 2025-06-18 回显，
未知版本回落 2025-06-18）。

### 7.2 完整入口模板

综合 grf-browser / grf-computer 的入口范式（可拷贝起步）：

```go
// grf-demo：示例插件（stdio / serve 双模式）。
//
// 用法：
//
//	grf-demo                  stdio 模式（grf-harness 自动发现 spawn）
//	grf-demo serve --addr 127.0.0.1:7620 [--token xxx] [--path /mcp]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"git.gorun.fun/share/grf-harness/plugins/internal/mcpserver"
)

// version 构建期经 -ldflags "-X main.version=..." 注入（build.sh 取
// git describe / CI tag），gh plugins 体检显示的就是它。
var version = "dev"

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "serve" {
		addr, token, path, err := mcpserver.ParseServeArgs(args[1:], "127.0.0.1:7620")
		if err != nil {
			fatal(err)
		}
		if err := newServer().ServeHTTP(context.Background(), addr, path, token); err != nil {
			fatal(err)
		}
		return
	}
	fs := flag.NewFlagSet("grf-demo", flag.ExitOnError)
	_ = fs.Parse(args) // stdio 模式：宿主按 settings.json 可传参
	if err := newServer().ServeStdio(context.Background()); err != nil {
		fatal(err)
	}
}

func newServer() *mcpserver.Server {
	s := mcpserver.New("grf-demo", version)
	registerTools(s) // 只注册，不做重初始化（握手时限 15s）
	return s
}

func registerTools(s *mcpserver.Server) {
	s.Add(mcpserver.Tool{
		Name:        "demo_hello",
		Description: "示例工具：返回问候语",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"who": map[string]any{"type": "string", "description": "对谁问好"},
			},
			"required": []string{"who"},
		},
		ReadOnly: true, // 纯读才标；有副作用不得标
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			var a struct {
				Who string `json:"who"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err // 参数错：文本回传，模型自会修正重试
			}
			return []mcpserver.Content{mcpserver.Text("hello, " + a.Who)}, nil
		},
	})
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "grf-demo:", err) // 错误进 stderr（stdio 纪律）
	os.Exit(1)
}
```

### 7.3 信号与清理范式

插件拉起了子进程 / 浏览器等资源时，**必须**补信号清理（grf-browser
的 `watchSignals`）：SIGINT/SIGTERM 默认直接杀进程、不走 defer，孤儿
chrome 就是这么来的。

```go
func watchSignals(cleanup func()) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		cleanup()
		os.Exit(0)
	}()
	_ = stop
}
```

stdio 主路径同时 `defer cleanup()`（stdin EOF 走 defer）。无重资源的
插件（如 grf-computer）可以不加。

## 8. 裸写协议子集参考

不用 SDK 的插件**必须**正确实现以下子集（与 `mcp` 客户端对齐）：
`initialize` / `tools/list` / `tools/call` / `ping`；通知（无 `id` 或
`id` 为 null）一律无应答；未知 method 回 JSON-RPC error `-32601`。

应答形状（省略 jsonrpc/id 信封，仅 result）：

```jsonc
// initialize：客户端 protocolVersion 属于三个已知版本则回显之，
// 否则回自己的版本（宿主只接受已知版本，别的会握手失败）
{"protocolVersion":"2025-06-18",
 "capabilities":{"tools":{"listChanged":false}},
 "serverInfo":{"name":"grf-demo","version":"0.1.0"}}

// ping：result 必须出现（空对象）
{}

// tools/list：单页全量即可，无需分页（nextCursor 可不实现）
{"tools":[
  {"name":"demo_hello","description":"...",
   "inputSchema":{"type":"object","properties":{...},"required":[...]},
   "annotations":{"readOnlyHint":true}}]}

// tools/call 成功（image 项：data 为 base64）
{"content":[{"type":"text","text":"..."},
            {"type":"image","mimeType":"image/png","data":"..."}]}
// tools/call 工具自身失败：协议层正常，isError 标记，文本回传模型
{"content":[{"type":"text","text":"error: ..."}],"isError":true}
```

错误语义辨析：**工具执行失败用 `isError`**（协议层成功，错误文本作为
工具输出回传模型，信息可用）；JSON-RPC error 对象（-32601/-32700 等）
留给协议层错误（方法不存在、报文解析失败）。宿主把两者都归为工具
失败，但 isError 的文本模型看得见、可自纠。

传输语义：

- **stdio**：行分隔 JSON-RPC；stdin EOF 退出；stdout 只有应答、日志走
  stderr（同 §3）。
- **Streamable HTTP**：POST 单条 JSON-RPC 到端点，应答 200 +
  `application/json`；通知回 202 无体；`Authorization: Bearer <token>`
  鉴权（配了 token 才校验）；请求体上限 8MB；客户端兼容 SSE 形态
  应答，但插件只做纯 JSON 即可；CORS 宽松放开（便于 Web 类客户端
  接入）。SDK 的 `ServeHTTP` 已内置全部语义，拷贝它最省事。

## 9. 测试规范

插件测试零外部依赖、零副作用（不依赖宿主、不拉浏览器——懒启动保证
了这点）。三层写法：

```go
// 1) 装配自检：全部工具注册且命名合规（见 browser/main_test.go）
s := newServer(...)
for _, n := range s.Names() {
	if !nameRe.MatchString(n) { t.Fatalf("工具名不合法: %q", n) }
}

// 2) handler 单测：handler 注册前先存为包级函数变量，绕过协议层直接调
got, err := demoHello(ctx, json.RawMessage(`{"who":"grf"}`))

// 3) 协议级：注入 reader/writer 走完整 stdio 行循环（见
//    mcpserver/server_test.go），或 httptest.NewServer(s.Handler("t"))
//    走 HTTP
srvIn, w := io.Pipe() // 测试写请求 → 服务端读
r, srvOut := io.Pipe() // 服务端写应答 → 测试读
go s.Serve(context.Background(), srvIn, srvOut)
```

`go test ./...` 在 plugins/ 模块内独立运行；宿主侧测试因
`testing.Testing()` 下插件自动空转，也不受插件影响。

## 10. 构建与分发

- 官方插件：名字加进 `plugins/build.sh` 的 `PLUGINS`，用
  `./plugins/build.sh [install|all|clean]` 构建。版本号经
  `-ldflags "-X main.version=..."` 注入（CI 取 tag、本地取 git
  describe，去 v 前缀）。
- 交叉编译 6 平台产物在 `plugins/dist/`；linux/windows 经 upx 压缩
  （darwin 与 windows/arm64 跳过——upx 破坏 macOS adhoc 签名、尚不
  支持 win64/arm64 PE；`UPX=0` 禁用）。
- macOS 安全覆盖安装的签名坑已由 build.sh 处理（先删旧文件、落点
  重签）；手工拷贝二进制到 `~/.grf/plugins/` 后若运行即 SIGKILL，
  `codesign -f -s - 文件` 重签。
- 官方发布：CI 打 tag 后落下载服务器
  `{base}/{平台}/{版本}/plugins/grf-xxx`（文件名去平台后缀，windows
  带 .exe），`gh plugins download` 按此布局探测。
- 第三方分发：任意途径交付二进制；用户放 `~/.grf/plugins/` 自动发现，
  或 settings.json `plugins[].command` 指定路径与参数、`plugins[].url`
  接远程 serve 实例。
- 发布前用 `gh plugins` 体检：确认连接状态、serverInfo 版本、工具
  清单与 readOnly 标注齐全。

## 11. 安全规范

- 插件进程与宿主**同权限**运行，不受 harness 沙箱约束（沙箱只约束
  内置 bash 工具）——只开发/安装可信来源的插件。
- serve 模式**必须**配 `--token` 并只监听回环或内网地址；端点能驱动
  的能力（浏览器、鼠标键盘）等价于操作者本人。
- `ReadOnly` 标注必须与实际副作用一致（§5.3）：它是计划模式硬拦截的
  放行依据，标错即绕过用户审批语义。
- 工具描述即对模型的契约：**不得**诱导模型做用户未意图的操作。
- 多租户部署（Workspace 用户隔离）下插件工具照走逐次审批，无豁免。

## 12. 发布前检查清单

- [ ] 工具名带插件前缀，匹配 `[a-zA-Z0-9_-]{1,64}`，不与内置 28 个
      及其他插件重名
- [ ] 每个参数有 description；可选参数有确定省缺语义
- [ ] `ReadOnly` 与实际副作用一致
- [ ] stdout 无任何非协议输出（检查所有日志/调试打印走 stderr）
- [ ] 错误经 error 返回；无 `os.Exit`、不依赖 panic 兜底
- [ ] handler 阻塞操作绑定 ctx；共享状态加锁
- [ ] 重资源懒启动；stdin EOF 与信号两路都触发清理
- [ ] serve 模式实测过（`--token` 鉴权生效、`--path` 可自定义）
- [ ] `go vet` + `go test ./...` 通过；装配自检覆盖全部工具名
- [ ] `./plugins/build.sh install` 后 `gh plugins` 体检：连接正常、
      版本正确、readOnlyHint 如预期
