# MCP 插件（plugins/）

grf-harness 的增强能力走 **MCP（Model Context Protocol）插件**：插件是
独立编译的可执行二进制，放在 `plugins/` 目录（独立 Go 模块，**不参与主
程序构建**）。主程序内置一个零依赖的 MCP 客户端（`mcp` 包 + `plugin`
管理器），把插件提供的工具注册进工具注册表——对模型来说，插件工具与
28 个内置工具同等可见、同等调用。

一个插件二进制同时支持两种执行模式：

| 模式 | 启动方式 | 接入方式 | 适用场景 |
|------|---------|---------|---------|
| **本地 stdio** | grf-harness 直接 spawn（无参数） | 自动发现或 `plugins[].command` | 插件与主程序同机 |
| **远程 HTTP** | 人工/运维在远端 `插件 serve --addr` 启动 | `plugins[].url`（Streamable HTTP） | 插件跑在服务器/容器里，多机共享浏览器等有状态实例 |

## 快速开始

```sh
gh plugins download        # 从官方下载服务器安装插件到 ~/.grf/plugins/
gh plugins                 # 体检：列出插件、连接状态、工具清单
./gh "用 browser_navigate 打开 https://example.com 并截图"
```

主程序分发一般不含插件（保持单二进制零依赖），`gh plugins download`
按当前平台从下载服务器（`https://download.gorun.fun/grf-harness/`，
`GRF_DOWNLOAD_BASE` 可覆盖）拉取：缺省取与主程序配套的版本，没有则取
最新版；可带名称过滤（`gh plugins download browser`，grf- 前缀可省略），
`-version` 指定版本。名称缺省下载全部，装完重启 gh 生效。

从源码构建则用：

```sh
./plugins/build.sh install   # 构建并安装到 ~/.grf/plugins/
```

`install` 把 `grf-browser`、`grf-computer` 复制到 `~/.grf/plugins/`——
该目录下的可执行文件会被**自动发现**（每个文件视为一个 stdio 插件），
无需任何配置。在 settings.json 写 `"plugins": []` 可显式关闭自动发现。

## 配置（~/.grf/settings.json）

```jsonc
{
  "plugins": [
    // 本地模式：显式指定可执行文件与参数（auto 发现之外的插件）
    {"name": "browser", "command": ["/opt/grf/grf-browser", "--headless=false"]},
    // 远程模式：MCP Streamable HTTP 端点（远端先启动 serve）
    {"name": "remote-browser", "url": "http://10.8.0.5:7610/mcp", "token": "共享密钥"}
  ]
}
```

字段：`command`（本地）与 `url`（远程）二选一；`name` 缺省取可执行文件
名 / URL 主机；`timeout` 单次工具调用超时秒数（默认 120）。

优先级：`plugins` 键**存在**（含 `[]`）→ 只用配置项；键**缺省**（nil）→
使用配置项 + 自动发现 `~/.grf/plugins`。项目级 `.grf/plugins/` **不会**
被执行（防止仓库内二进制未经确认即运行）。

## 内置插件

### grf-browser（浏览器操作，21 个工具）

chromedp（纯 Go CDP 客户端）实现，工具面对齐微软 playwright-mcp 的
工作流：**先 `browser_snapshot` 拿元素引用（ref），再按 ref 操作**。
快照与定位（shadow DOM / 同源 iframe 深度穿透）、点击为真实 CDP
鼠标事件（isTrusted，iframe 内自动累加外层偏移）。浏览器懒启动：
第一个工具调用才拉起 chrome（无头模式，`--headless=false` 带界面；
`--chrome` 指定浏览器路径）。

- 导航：`browser_navigate` / `browser_back` / `browser_forward` / `browser_reload`
- 快照与感知：`browser_snapshot`（大纲 + 文本块 + [ref=eN]）、
  `browser_get_text`（页面可见文本）、`browser_wait_for`
  （等文本出现/URL 包含）、`browser_console_messages`
  （console/异常，含自动应答的 [dialog] 与 [download] 事件）、
  `browser_network_requests`（含资源类型/耗时/大小/Content-Type）
- 交互：`browser_click`（ref/selector，真实鼠标事件，支持双击）、
  `browser_type`（填表 + 可选回车提交）、`browser_press_key`
  （enter/ctrl+c/cmd+t 等）、`browser_hover`（先移出再移入）、
  `browser_select_option`、`browser_scroll`（方向滚动或把 ref 滚到
  视口中央，触达懒加载内容）
- 文件与状态：`browser_file_upload`（input[type=file] 设文件）、
  `browser_cookies`（list/set/delete）
- 其他：`browser_evaluate`（任意 JS）、`browser_take_screenshot`
  （视口/整页，图片多模态回传或存文件）、`browser_tabs`
  （list/new/select/close）、`browser_close`

原生对话框（alert/confirm/prompt）自动 dismiss 并记入控制台日志，
避免挂起后续指令；下载自动落到插件工作目录 `downloads/`。

### grf-computer（电脑操作，11 个工具）

经操作系统自带命令实现（插件本身零 cgo、零第三方依赖）。坐标一律为全局
虚拟桌面坐标（原点=主屏左上，多屏时副屏坐标可为负）：

- `computer_screenshot`（PNG 多模态回传，可选 `display` 编号=只截该屏，
  省略=整桌面）、`computer_screen_size`（可选 `display`，省略=主屏）
- `computer_list_displays`（多屏枚举：编号/全局边界/主屏标记/Retina
  缩放；macOS NSScreen、Windows AllScreens、Linux xrandr，无 xrandr
  退回单屏）
- `computer_click`（按键/连击）、`computer_move`、`computer_drag`、
  `computer_scroll`
- `computer_type`、`computer_key`、`computer_clipboard`（get/set）、
  `computer_wait`

平台要求：

| 平台 | 依赖 | 授权 |
|------|------|------|
| macOS | 内置 osascript / screencapture / pbcopy | 终端 App 需"辅助功能"授权（截屏另需"屏幕录制"） |
| Linux | xdotool；截屏 scrot / gnome-screenshot / import / grim 任一；剪贴板 xclip/xsel | X11（Wayland 需经 XWayland） |
| Windows | 内置 PowerShell | 无 |

## 写一个新插件

`plugins/` 是独立 Go 模块（`git.gorun.fun/share/grf-harness/plugins`），
内置 `internal/mcpserver` SDK——工具实现一次注册，stdio / HTTP 双模式
自动可用：

```go
package main

import (
	"context"
	"encoding/json"

	"git.gorun.fun/share/grf-harness/plugins/internal/mcpserver"
)

func main() {
	s := mcpserver.New("grf-demo", "0.1.0")
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
		ReadOnly: true, // 只读工具：计划模式放行
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			var a struct{ Who string `json:"who"` }
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			return []mcpserver.Content{mcpserver.Text("hello, " + a.Who)}, nil
		},
	})
	if err := s.ServeStdio(context.Background()); err != nil { // 本地模式
		panic(err)
	}
	// 远程模式：编译后以 `grf-demo serve --addr 127.0.0.1:7620` 启动
}
```

约定：

- **工具名自带命名空间前缀**（如 `demo_*`），限 `[a-zA-Z0-9_-]{1,64}`；
  与内置 28 个工具重名会被跳过（警告），多插件重名先连者胜。
- 有副作用的工具不要设 `ReadOnly`——harness 据此在计划模式下拦截。
- 工具失败返回 error（SDK 转为 `isError` 内容），不要 panic、不要退出
  进程；进程死亡即视为插件失效（会话内报错，主程序不受影响）。
- 图像结果用 `mcpserver.ImagePNG(base64)`，harness 会以多模态回传给模型。

## 协议子集

主程序客户端与插件 SDK 均只实现 harness 所需的 MCP 子集
（协议版本 2025-06-18，兼容 2024-11-05 / 2025-03-26 客户端）：

- `initialize` 握手 + `notifications/initialized`
- `tools/list`（自动翻页）、`tools/call`、`ping`
- 通知 / 服务端反向请求（sampling、roots 等）一律忽略

传输：

- **stdio**：行分隔 JSON-RPC（MCP 约定）；插件 stderr 进入环形缓冲，
  连接失败时随错误信息带回，便于诊断。
- **Streamable HTTP**：POST 单条 JSON-RPC 到端点，应答 application/json
  （客户端亦兼容 SSE 形态应答）；`Authorization: Bearer <token>` 鉴权；
  通知应答 202 无体。

## 安全模型

- 插件工具与内置工具一样走**审批模式**（每工具执行前经用户确认）与
  **计划模式**（未标只读的插件工具一律拦截，`agent/turn.go` 的
  `planBlocked`）。
- 插件进程与主程序**同权限**运行（文件系统/网络不受 harness 沙箱约束
  ——沙箱只约束内置 bash 工具）。只安装可信来源的插件；远程模式建议
  配置 `--token` 并仅监听内网/回环地址。
- 插件连接是**进程级共享**的：同一进程内多个会话（gh server 每会话
  harness、weixin 每用户）复用同一批插件连接，浏览器等有状态上下文跨
  会话保留；`go test` 下插件自动空转（`testing.Testing()`），测试不产生
  副作用。

## 构建与发布

```sh
./plugins/build.sh            # 当前系统 → plugins/bin/
./plugins/build.sh install    # 构建并安装到 ~/.grf/plugins/
./plugins/build.sh all        # 交叉编译 6 平台 → plugins/dist/（CI 发布用）
```

`plugins/` 有自己的 go.mod——这是整个仓库**唯一允许第三方依赖**的地方
（当前仅 chromedp 及其传递依赖）；主模块保持纯标准库零依赖不变，主程序
本地 `build.sh` 不交叉编译插件（仅缺省路径顺带 `install` 当前系统）。
插件版本号经 ldflags 注入（取 git describe），`gh plugins` 会显示各插件
自报的版本。

CI（打 tag 触发）在主程序之外同时构建插件并随版本发布到下载服务器：

```
{base}/{平台}/{版本}/gh(.exe)                    # 主程序
{base}/{平台}/{版本}/plugins/grf-browser 等      # 插件（去平台后缀，windows 带 .exe）
```

`build.sh all` 对 linux/windows 产物做 **upx 压缩**（chromedp 体积大，
可减 60% 以上；darwin 与 windows/arm64 跳过——upx 会破坏 macOS adhoc
签名，且尚不支持 win64/arm64 PE，`UPX=0` 可禁用）。`gh plugins
download` 即按上述布局探测下载（探测逻辑在 `plugin/download.go`，目录
列表与下载替换复用 selfupdate 的实现，基址同为
`https://download.gorun.fun/grf-harness/`）。
