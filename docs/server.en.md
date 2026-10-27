# gh server: Responses API access

[简体中文](server.md) | **English**

`gh server` exposes the harness's agent capabilities as an HTTP service in
the **OpenAI Responses API** shape: the core conversation/tool paths follow
the **Open Responses Specification (ORS) 2026-04-24** (basic text,
instructions, client function tools, streaming, multi-turn, image input,
compaction, etc.), plus superset endpoints in the OpenAI shape (retrieve /
cancel / conversations). gh-specific capabilities all ride the `grf:`
colon-prefix extension channel (ORS §11 extension rules). Zero third-party
dependencies (pure `net/http`).

The underlying pause/resume mechanism (the agent turn loop) is documented in
[architecture.md](architecture.en.md) §3.2; Go library access in
[integration.md](integration.en.md).

```sh
gh server [-addr 127.0.0.1:7400] [-data ~/.grf/server] [-token <t>]
```

## 1. Security model (read this first)

The service runs bash and file tools **on your machine** (one workspace per
session, `<data>/conversations/<sid>/workspace/`). Exposing it to a network
opens a controlled RCE entry:

- Binds `127.0.0.1` only by default; understand the risk before `-addr`
  binds non-loopback
- Every endpoint requires `Authorization: Bearer <token>`; token precedence
  is `-token` > `<data>/token` (auto-generated on first start, persisted
  0600, printed once)
- Per-session sandbox/approval policy: see §4 `grf:session`

## 2. Endpoints

| Method | Path | Purpose |
|------|------|------|
| POST | `/v1/responses` | The core endpoint: new input / continuation / interaction answers / tool results — all through this one entry |
| POST | `/v1/responses/compact` | Context compaction (`model` required, 400 when missing) |
| GET | `/v1/responses/{id}` | Fetch final state (superset; 404 = the turn evaporated, see §6) |
| POST | `/v1/responses/{id}/cancel` | Cancel a running turn (superset, idempotent) |
| GET | `/v1/conversations` | Conversation list (superset) |
| GET | `/v1/conversations/{id}` | Conversation details: paused/running/usage (superset) |
| DELETE | `/v1/conversations/{id}` | Archive: stop the turn → persist → mark; **directory kept** (files the agent wrote are not deleted) |
| GET | `/v1/conversations/{id}/items` | Full message history (llm.Message wire format, superset) |
| GET | `/v1/agent/models/resolve?name=` | Structured model-name resolution (bare / qualified / empty = default; `{status: ok\|no_default\|not_found\|ambiguous\|unreadable, resolved?, candidates?}`, candidates listed on ambiguity). The **single authority** for model resolution — goder-serve's host-admin `resolve-model` forwards to this endpoint so headless host tasks can fail fast; re-reads settings.json per request, so provider declarations just written by a host are visible immediately |
| GET | `/v1/health` | Liveness probe (authenticated) |

## 3. Session model: disk is truth, previous-chain

- Requests without `previous_response_id` **lazily create** a session; the
  response returns `grf:session_id` (the session is the
  `<data>/conversations/<sid>/` directory)
- With `previous_response_id` the chat continues; it **must be the latest
  response of that session**, otherwise 409 (no historical forking)
- After a server restart the in-memory cache is empty — continuation works
  directly via `previous_response_id`: archives load automatically
  (including paused states) and config is read from meta.json without drift
- Same-session serialization (an agent core constraint): new requests
  during a running turn return 409, **no queueing**

## 4. POST /v1/responses parameters

**Effective**:

| Parameter | Behavior |
|------|------|
| `input` (string / items) | strings and user messages → new input; `input_image` (data URL) → multimodal; assistant / `function_call` / `reasoning` items → injected into history; `function_call_output` / `grf:input_result` → pause answers |
| `previous_response_id` | continue (chain-tail check) |
| `instructions` | session system prompt (applies per request; changes invalidate the prompt cache) |
| `tools` (function) | registered as **client-executed** tools: when the model calls one, the turn pauses and produces a `function_call` item; the caller executes it and continues with `function_call_output` in the next input. Absent → keep last round's registrations; explicit empty array clears |
| `tool_choice` | `"none"` disables all; `{"type":"allowed_tools","tools":[...]}` keeps only the listed (cache-preserving semantics) |
| `model` | best-effort: `provider/model` switches when it matches config, otherwise keeps the current one (any value runs). Bare model names resolve against the provider table: the default provider wins if it has the model, otherwise a single provider offering it does; ambiguous/zero hits keep the current (hosts no longer need to pre-qualify the name) |
| `max_tool_calls` | cap on tool-loop rounds |
| `prompt_cache_key` | pass-through session cache key |
| `store: false` | no archive, no snapshots (note: cannot continue after a restart) |
| `stream: true` | SSE event stream (see §5) |
| `background: true` | returns queued immediately; poll final state with `GET /v1/responses/{id}` |

**Accepted but ignored** (echoed without effect): `temperature` / `top_p` /
`max_output_tokens` / `parallel_tool_calls` / `presence_penalty` /
`frequency_penalty` / `top_logprobs` / `reasoning` / `service_tier` /
`safety_identifier` / `metadata` / `include` /
`truncation` (over-long input is neither truncated nor rejected).
`text.format` supports text only.

**gh extension parameters** (effective on first request, persisted to
meta.json):

```json
{"grf:session": {
  "policy": "write-workspace|read-workspace",  // unified rule string; empty defaults to off | write-workspace | read-workspace
  "auto_approve": true,        // default for sandbox escalations / plan approvals (see §7)
  "system_prompt": "…",        // session-scoped custom section (identity/how-to-work); the built-in tool-doc section is always appended, never replaced
  "host_context": "You are running on host platform X, current user …", // host environment briefing: runtime facts
                               // injected by the embedder, as a standalone section after
                               // system_prompt and before the built-in tool docs
                               // (additive, not a replacement); shared by root and
                               // sub-agents; the host_context field of
                               // GET /v1/conversations/{id} echoes the settled value.
                               // Hosts use it to tell the session which platform it
                               // lives in without taking over the whole identity prompt.
  "default_skill": "grf-start", // default skill name (when a skill by that name exists, its SKILL.md full text is injected into the system prompt); by default nothing is injected
  "max_steps": 100,
  "disabled_tools": ["ralph"]
}}
```

## 5. Streaming events (SSE)

With `stream: true` the response is `text/event-stream`: `event:` lines
match the data JSON's `type`, every event carries a session-monotonic
`sequence_number`, and `data: [DONE]` terminates. Sequence:
`response.created` → `response.in_progress` →
(`response.output_item.added` → content delta family →
`response.output_item.done`)* → terminal event (`response.completed` /
`.failed` / `.incomplete`, carrying the full response snapshot).

The 28 built-in tools are hosted tools (server-side execution); each call
produces a `grf:tool_call` item (fields: `name` / `input` / `output_text` /
`err` / `agent_id` / `label`, untruncated), with events
`grf:tool_call.in_progress` → `.completed` / `.failed` / `.denied`. The
thinking stream maps onto standard reasoning summaries
(`response.reasoning_summary_text.delta`). Line-style progress events
(todos/goal etc.) surface as `grf:notice {kind, detail}`.

## 6. Interaction protocol (ask_user / approvals): chained continuation

gh's ask_user questions and per-tool approvals never block the HTTP
connection — they **pause the turn**:

- The response ends with `status: "incomplete"`,
  `incomplete_details.reason = "grf:input_required"`, and `grf_requests[]`
  listing the calls awaiting answers (`call_id` / `kind: question |
  tool_approval` / original `arguments`)
- Answering = an ordinary continuation request:

```json
POST /v1/responses
{"model":"…", "previous_response_id":"resp_…",
 "input": [{"type": "grf:input_result", "call_id": "c1",
            "answers": {"q1": "sweet"} }]}
```

  - question: `answers` (question id → answer); `dismiss: true` = cancel
    (the agent treats it as "user didn't answer, assume and continue")
  - tool_approval: `approved: true` (the server executes the tool) /
    `false` plus `feedback` (rejection text returned to the model)
- Pauses of client function tools use the standard ORS shape:
  `status: "completed"` + `function_call` item, continued with
  `function_call_output`
- Pause state lives in the persisted history — **after a server restart the
  answer still continues the turn** (no timeout)

## 7. Sandbox escalation and plan approval

These interactions happen inside tool execution (the escalation retry after
a bash sandbox denial, exit_plan_mode plan approval, enter_plan_mode entry
requests via `Host.AskUser`) and do not join the pause protocol: escalation
and plan approval follow `grf:session.auto_approve` (true grants / false
denies, fail-closed); enter_plan_mode under a non-interactive host fails
AskUser and is always treated as denied, steering the model to just
continue executing. For human-in-the-loop approval, control the tool
surface with `tool_choice` + per-tool approval (configure `grf:session`
alongside `disabled_tools`; the pause protocol covers approval mode).

## 8. Failure semantics

| Scenario | Behavior |
|------|------|
| Concurrent request to the same session | 409 (no queueing) |
| Turns running at server restart | **evaporate**: no terminal event, `GET /v1/responses/{id}` 404; clients can resend the input (history is archived; except for duplicate side effects — executed tools have executed) |
| New plain message to a paused session | 400 (answer `grf:input_result` first) |
| Historical forking (previous not the chain tail) | 409 |
| Any request to an archived conversation | 404 |
| `store:false` turns | 404 after restart, cannot continue |

## 8.5 Host integration hooks (environment variables)

| Variable | Purpose |
|------|------|
| `GRF_TURN_WEBHOOK_URL` | When a background (`background:true`) turn settles, POST a `turn.settled` event (`type/session_id/response_id/status/model/usage/ts`) to the host; synchronous/SSE turns are not sent. Best-effort delivery (5s timeout, no retry), idempotency key `response_id`; polling retrieve remains the fallback |
| `GRF_TURN_WEBHOOK_SECRET` | HMAC-SHA256 secret for the event body; when configured, an `X-GRF-Signature: sha256=<hex(body)>` header is attached — receivers should verify it in constant time before trusting the body |
| `GRF_SERVER_ACCESS_LOG` | Set to `off` to disable access logging (default on: one line per request "method path status duration remote" to stderr; SSE streams are logged after they end) |

Both are read at startup (host gateways inject them via child-process env,
see their `HOST_GH_ENV_<KEY>` passthrough channel (a host-side convention —
gh only honors the final injected KEY)).

**settings.json hot-following**: configuration-file changes (mtime+size
fingerprint) are detected at turn boundaries; sessions without an explicit
`model` automatically follow the new default provider/model on the next
turn (config-sync integrations just write the file — no restart or idle
recycling needed). Explicit `model` goes through trySwitchModel, which
already reads the file immediately.

## 9. ORS compliance matrix (17-item browser/CLI suite → gh)

| Compliance item | Implementation |
|--------|------|
| Basic Text Response | ✅ §4 |
| Assistant Message Phase | phase accepted and echoed (gh has no commentary distinction on the model side, always empty) |
| Response Output Phase Schema | ✅ item state machine in_progress → completed / incomplete |
| Streaming Response | ✅ §5 (monotonic sequence_number, event=data.type, [DONE]) |
| System Prompt | ✅ instructions |
| Tool Calling (function) | ✅ pause protocol (§6, completed + function_call item + continuation) |
| Image Input | ✅ data-URL multimodal |
| Multi-turn Conversation | ✅ previous_response_id chain |
| Compaction Endpoint | ✅ §2 compact (LLM summary + encrypted_content window) |
| Compaction Missing Model | ✅ 400 |
| WebSocket transport (7 CLI items) | ❌ not implemented (ORS MAY); HTTP + SSE covers it equivalently |

## 10. curl quick start

```sh
TOKEN=…   # printed at startup
H="Authorization: Bearer $TOKEN"

# one question, one answer
curl -s -H "$H" -d '{"model":"…","input":"analyze this repo's test coverage"}' \
     localhost:7400/v1/responses

# streaming
curl -sN -H "$H" -d '{"model":"…","input":"…","stream":true}' \
     localhost:7400/v1/responses

# continue (take the previous response id)
curl -s -H "$H" -d '{"model":"…","input":"continue","previous_response_id":"resp_…"}' \
     localhost:7400/v1/responses
```

## 11. Relation to the other access paths

- **Go embedding**: `docs/integration.en.md` (the app.New facade)
- **CLI subprocess**: `./gh "task"` (the lightweight path for non-Go hosts)
- **HTTP (this service)**: the structured path for non-Go hosts; point any
  openai client SDK at `base_url=http://127.0.0.1:7400/v1` and drive it
  (Responses resource shape compatible)
