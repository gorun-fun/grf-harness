# Configuration reference

[简体中文](configuration.md) | **English**

Every field of `~/.grf/settings.json`, the policy rule string, the
environment variable table, and the WeChat channel settings. Precedence is
always **environment variable > config file > built-in default**.

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

| Field | Meaning |
|---|---|
| `providers` | Provider table. Key = name; value holds `base_url` (OpenAI-compatible endpoint), `api_key`, `models` (optional model list) |
| `default` | Default model as `"provider/model"`; falls back to the alphabetically first provider's first model |
| `system_prompt` | Custom identity/how-to-work section; empty = built-in default. The inner prompt (tool docs) is always appended and never replaced |
| `max_steps` | Max steps per turn; 0 = built-in default |
| `policy` | Unified rule string (see below). Empty = `off` (all disabled); invalid values fall back to `write-workspace` |
| `auto_approve` | Auto-approve sandbox escalations and plans in non-interactive (one-shot/embedded) modes. Never applies to the WeChat channel (approvals go through WeChat Q&A) |
| `compact_threshold` | Auto-compaction threshold in estimated tokens: 0 = default (100k), positive overrides, negative disables |
| `weixin` | WeChat channel security settings (dedicated section below) |
| `plugins` | MCP plugin wiring: key absent = auto-discover executables under `~/.grf/plugins/`, `[]` = disable discovery, non-empty = use only the listed items. See the [plugin docs](plugins.en.md) |

## The policy rule string

A pipe-combined token string parsed by `config.ParsePolicy`:

| Token | Write side | Read side |
|---|---|---|
| `write-workspace` | bash restricted to workspace writes via macOS sandbox-exec (**process-level sandbox is macOS-only**); write/edit require a prior read before overwriting (new files exempt) | unrestricted |
| `read-workspace` | unrestricted | read/glob/grep/read_image limited to workspace paths (hard denial, no escalation protocol) |
| `off` / empty | off (the write boundary still applies unconditionally: file tools only write inside the workspace and granted dirs) | off |

- Ask-on-escalation: when the write boundary or the bash sandbox denies, the
  harness pauses for user approval (`Host.ApproveEscalation`). Approve =
  allow once (bash re-runs the same command unsandboxed); deny = final
  (retrying with `sandbox_permissions` will not re-ask)
- Granted directories: a write/edit escalation approval may include a
  GrantDir, persisted to the workspace's `.grf/grants.json` (effective for
  this workspace only); subsequent writes there skip the prompt. The file
  itself is hard-blocked from model-side writes; grants shipped with a repo
  are disclosed in the system prompt
- `set_policy` tool: the model can only raise a popup with a justification;
  the user alone picks from preset options, effective for the current session
  only and never persisted; tightening propagates to all existing sub-agents

## Model name resolution

`default` / `GRF_MODEL` / per-session switching share one rule set:

- **Qualified name** `provider/model`: the provider must exist; a model listed
  under it switches directly, an unlisted model is still trusted as an
  explicit override (handy for testing new models without registering them)
- **Bare name**: preferred if the default provider has it; otherwise switch
  when exactly one provider offers it; ambiguous (multiple providers, default
  provider doesn't have it) or zero hits keep the current model

## Environment variables

| Variable | Purpose | Default |
|---|---|---|
| `GRF_HOME` | Config root (settings/plugins/sessions/weixin state all live here) | `~/.grf` |
| `GRF_MODEL` | Model override (bare / qualified / passthrough — see above) | — |
| `OPENAI_BASE_URL` | Final LLM endpoint override (highest precedence) | provider `base_url` |
| `OPENAI_API_KEY` | Final LLM key override (highest precedence) | provider `api_key` |
| `GRF_MOUSE` | Initial mouse state in full-screen mode; `0`/`false` disables | enabled |
| `GRF_DOWNLOAD_BASE` | Base URL for self-update and plugin downloads | `https://download.gorun.fun/grf-harness` |
| `GRF_SERVER_ACCESS_LOG` | Set to `off` to disable `gh server` access logging | enabled |
| `GRF_TURN_WEBHOOK_URL` | Webhook URL for turn-settled events from `gh server` (POST `turn.settled`) | empty = off |
| `GRF_TURN_WEBHOOK_SECRET` | HMAC-SHA256 secret for the `X-GRF-Signature` header | empty = unsigned |

**Child-process env stripping**: bash and MCP plugin child processes get an
environment with secret-looking variables removed (case-insensitive match on
the `api_key` / `apikey` / `secret` / `token` / `password` / `passwd` /
`credential` name families; implemented in `tools/env.go` and
`mcp/stdio.go`). Export explicitly inside a command if you really need one.

## WeChat channel settings

```json
"weixin": {
  "allow_senders": ["wxid_xxxx"],
  "policy": ""
}
```

- `allow_senders`: allowlist of WeChat user IDs allowed to drive the agent.
  **Empty = deny every message** (safe default); unauthorized senders get a
  reply containing their ID and setup instructions (throttled to one per
  sender per 60 seconds)
- `policy`: channel policy override; empty = `write-workspace|read-workspace`
  (tighter than the global `off` default — WeChat messages are arbitrary
  external input)
- The global `auto_approve` never applies: tool approvals (per-tool /
  escalation / plan) are forwarded as WeChat questions; timeouts count as
  rejection
- Channel-default workspace is `~/.grf/weixin/ws` (overridable with
  `-workspace`); inbound media downloads accept only `*.weixin.qq.com` hosts

## Other on-disk files

| Path | Content | Mode |
|---|---|---|
| `~/.grf/settings.json` | Configuration (contains api_key) | 0600, atomic writes |
| `~/.grf/history` | Input history | — |
| `~/.grf/sessions/` (global) / `<workspace>/.grf/sessions/` | Session transcripts (may contain sensitive content) | 0600 |
| `<workspace>/.grf/memory.json` | Project-scope cross-session memory (treated as untrusted when shipped with a repo; disclosed in the system prompt) | 0600 |
| `~/.grf/memory.json` | User-scope cross-session memory | 0600 |
| `<workspace>/.grf/grants.json` | Granted out-of-workspace write dirs (model-side writes blocked; loading is disclosed) | 0600 |
| `~/.grf/server/` | `gh server` data dir (token / conversation store / snapshots) | token 0600 |
| `~/.grf/weixin.log` / `weixin.pid` / `weixin_state.json` | WeChat daemon log / PID / login state | 0600 |
