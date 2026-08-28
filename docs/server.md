# gh server：Responses API 接入

`gh server` 把 harness 的 agent 能力以 **OpenAI Responses API** 形状暴露为
HTTP 服务：核心对话/工具路径按 **Open Responses Specification（ORS）
2026-04-24** 实现（基础文本、instructions、客户端函数工具、流式、多轮、
图像输入、compact 等），并附 OpenAI 形状的超集端点（retrieve / cancel /
conversations）。gh 特有能力全部走 `grf:` 冒号前缀扩展（ORS §11 扩展规则）。
零第三方依赖（纯 `net/http`）。

暂停/恢复协议的底层机制（agent 回合循环）见
[architecture.md](architecture.md) §3.2；Go 库接入见
[integration.md](integration.md)。

```sh
gh server [-addr 127.0.0.1:7400] [-data ~/.grf/server] [-token <t>]
```

## 1. 安全模型（先读这个）

服务在**你的机器上**执行 bash 与文件工具（每会话独立 workspace，
`<data>/conversations/<sid>/workspace/`）。把它暴露到网络等于开放一个
受控 RCE 入口：

- 默认只绑 `127.0.0.1`；`-addr` 绑非环回前务必理解上述风险
- 全部端点要求 `Authorization: Bearer <token>`；令牌优先级
  `-token` > `<data>/token`（首启自动生成、0600 落盘并打印一次）
- 会话级沙箱/审批策略见 §4 `grf:session`

## 2. 端点

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/v1/responses` | 核心端点：发消息/续聊/应答交互/回传工具结果，全在这一个入口 |
| POST | `/v1/responses/compact` | 上下文压缩（`model` 必填，缺省 400） |
| GET | `/v1/responses/{id}` | 查终态（超集；404 = 回合蒸发，见 §6） |
| POST | `/v1/responses/{id}/cancel` | 取消在跑回合（超集，幂等） |
| GET | `/v1/conversations` | 会话列表（超集） |
| GET | `/v1/conversations/{id}` | 会话详情：paused/running/用量（超集） |
| DELETE | `/v1/conversations/{id}` | 归档：终止回合 → 存档 → 标记；**目录保留**（agent 写过的文件不删） |
| GET | `/v1/conversations/{id}/items` | 完整消息历史（llm.Message 线格式，超集） |
| GET | `/v1/health` | 探活（需鉴权） |

## 3. 会话模型：磁盘为准，previous 链

- 不带 `previous_response_id` 的请求**懒创建**新会话，响应里返回
  `grf:session_id`（会话即 `<data>/conversations/<sid>/` 目录）
- 带 `previous_response_id` 续聊；**必须是该会话最新一个 response**，
  否则 409（不支持历史分叉）
- server 重启后内存缓存清空，**凭 previous_response_id 直接续接**——
  存档自动加载（含暂停态），配置读 meta.json 不漂移
- 同会话串行（agent 核心约束）：在跑回合期间的新请求返回 409，
  **不排队**

## 4. POST /v1/responses 参数

**实现生效**：

| 参数 | 行为 |
|------|------|
| `input`（string / items） | string 与 user message → 新输入；`input_image`（data URL）→ 多模态；assistant / `function_call` / `reasoning` item → 注入历史；`function_call_output` / `grf:input_result` → 暂停应答 |
| `previous_response_id` | 续聊（链尾校验） |
| `instructions` | 会话系统提示词（每请求生效；变更使提示词缓存失效） |
| `tools`（function） | 注册为**客户端执行**工具：模型调用时回合暂停、产出 `function_call` item，接入方执行后在下轮 input 里带 `function_call_output` 续交。缺省时沿用上轮注册；显式空数组清空 |
| `tool_choice` | `"none"` 禁全部；`{"type":"allowed_tools","tools":[...]}` 仅留列出的（保缓存语义） |
| `model` | best-effort：`provider/model` 命中配置则切换，否则沿用当前（任意值可跑） |
| `max_tool_calls` | 工具循环轮次上限 |
| `prompt_cache_key` | 透传会话缓存键 |
| `store: false` | 不落存档、不写快照（注意：重启后无法续接） |
| `stream: true` | SSE 事件流（见 §5） |
| `background: true` | 立即返回 queued，`GET /v1/responses/{id}` 轮询终态 |

**接受但忽略**（回显不生效）：`temperature` / `top_p` /
`max_output_tokens` / `parallel_tool_calls` / `presence_penalty` /
`frequency_penalty` / `top_logprobs` / `reasoning` / `service_tier` /
`safety_identifier` / `metadata` / `include` /
`truncation`（超长输入不截断也不报错）。`text.format` 仅支持 text。

**gh 扩展参数**（首次请求生效，写 meta.json）：

```json
{"grf:session": {
  "sandbox": "off",            // off | workspace-write
  "fs_policy": "observe",      // observe | off
  "auto_approve": true,        // 沙箱升级/计划审批的默认值（见 §7）
  "system_prompt": "…",        // 等同 instructions 的会话级版本
  "max_steps": 100,
  "disabled_tools": ["ralph"]
}}
```

## 5. 流式事件（SSE）

`stream: true` 时返回 `text/event-stream`：`event:` 行与 data JSON 的
`type` 一致，每个事件带会话级单调 `sequence_number`，`data: [DONE]` 终止。
序列：`response.created` → `response.in_progress` →
（`response.output_item.added` → 内容 delta 族 → `response.output_item.done`）*
→ 终态事件（`response.completed` / `.failed` / `.incomplete`，携带完整
response 快照）。

内置 25 个工具是 hosted tool（服务端执行），每次调用产出
`grf:tool_call` item（字段：`name` / `input` / `output_text` / `err` /
`agent_id` / `label`，全量不截断），事件 `grf:tool_call.in_progress` →
`.completed` / `.failed` / `.denied`。思考流映射为标准 reasoning summary
（`response.reasoning_summary_text.delta`）。行式过程事件（todos/goal 等）
以 `grf:notice {kind, detail}` 透出。

## 6. 交互协议（ask_user / 审批）：链式续交

gh 的 ask_user 提问与逐工具审批不阻塞 HTTP 连接，而是**暂停回合**：

- response 以 `status: "incomplete"` 终止，
  `incomplete_details.reason = "grf:input_required"`，
  `grf_requests[]` 列出待应答调用（`call_id` / `kind: question |
  tool_approval` / `arguments` 原文）
- 应答 = 普通续聊请求：

```json
POST /v1/responses
{"model":"…", "previous_response_id":"resp_…",
 "input": [{"type": "grf:input_result", "call_id": "c1",
            "answers": {"q1": "甜"} }]}
```

  - question：`answers`（各问题 id → 答案）；`dismiss: true` = 取消
    （agent 视为"用户未答，自行假设继续"）
  - tool_approval：`approved: true`（服务端执行该工具）/ `false` +
    `feedback`（拒绝文本回给模型）
- 客户端函数工具的暂停是标准 ORS 形状：`status: "completed"` +
  `function_call` item，用 `function_call_output` 续交
- 暂停状态活在持久化历史里，**server 重启后照常应答续跑**（无超时）

## 7. 沙箱升级与计划审批

这两类交互发生在工具执行内部（bash 沙箱拒绝后的升级重试、
exit_plan_mode），不参与暂停协议，按 `grf:session.auto_approve` 处理：
true 放行 / false 拒绝（fail-closed）。需要人在环的审批请用
`tool_choice` 控制工具面 + 逐工具审批（`disabled_tools` 外配置
`grf:session`，且暂停协议覆盖审批模式）。

## 8. 故障语义

| 场景 | 行为 |
|------|------|
| 同会话并发请求 | 409（不排队） |
| server 重启时在跑的回合 | **蒸发**：无终态事件、`GET /v1/responses/{id}` 404；客户端重发输入即可（历史已存档，无重复副作用风险除外——工具执行过就执行过了） |
| 暂停中的会话收到普通新消息 | 400（必须先应答 `grf:input_result`） |
| 历史分叉（previous 非链尾） | 409 |
| 归档会话的任何请求 | 404 |
| store:false 的回合 | 重启后 404，无法续接 |

## 9. ORS 合规对照（17 项浏览器/CLI 套件 → gh 实现）

| 合规项 | 实现 |
|--------|------|
| Basic Text Response | ✅ §4 |
| Assistant Message Phase | phase 字段接受并回显（gh 模型侧无 commentary 区分，恒空） |
| Response Output Phase Schema | ✅ item 状态机 in_progress → completed / incomplete |
| Streaming Response | ✅ §5（sequence_number 单调、event=data.type、[DONE]） |
| System Prompt | ✅ instructions |
| Tool Calling（function） | ✅ 暂停协议（§6，completed + function_call item + 续交） |
| Image Input | ✅ data URL 多模态 |
| Multi-turn Conversation | ✅ previous_response_id 链 |
| Compaction Endpoint | ✅ §2 compact（LLM 总结 + encrypted_content 窗口） |
| Compaction Missing Model | ✅ 400 |
| WebSocket 传输（7 项 CLI） | ❌ 未实现（ORS MAY 可选）；HTTP + SSE 等价覆盖 |

## 10. curl 快速上手

```sh
TOKEN=…   # 启动时打印
H="Authorization: Bearer $TOKEN"

# 一问一答
curl -s -H "$H" -d '{"model":"…","input":"分析这个仓库的测试覆盖"}' \
     localhost:7400/v1/responses

# 流式
curl -sN -H "$H" -d '{"model":"…","input":"…","stream":true}' \
     localhost:7400/v1/responses

# 续聊（拿上一响应的 id）
curl -s -H "$H" -d '{"model":"…","input":"继续","previous_response_id":"resp_…"}' \
     localhost:7400/v1/responses
```

## 11. 与其它接入方式的关系

- **Go 嵌入**：`docs/integration.md`（app.New 门面）
- **CLI 子进程**：`./gh "任务"`（非 Go 宿主的轻量路径）
- **HTTP（本服务）**：非 Go 宿主的结构化路径；openai 客户端 SDK 指向
  `base_url=http://127.0.0.1:7400/v1` 即可驱动（Responses 资源形状兼容）
