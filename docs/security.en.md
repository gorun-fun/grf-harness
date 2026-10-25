# Security model

[简体中文](security.md) | **English**

gh drives **local bash and file tools** — its capability boundary is its
security boundary. This page is the complete overview: threat surfaces,
per-channel defaults, and deployment notes. Field-level details live in the
[configuration reference](configuration.en.md).

## Threat model

| Channel | Who can drive the agent | Default defenses |
|---|---|---|
| Terminal CLI / Go embedding | Local user / embedder | Policy rule string (sandbox/boundaries/approvals) |
| `gh server` (HTTP) | Clients holding the bearer token | Loopback-only `127.0.0.1:7400` + constant-time auth |
| WeChat channel | **Allowlisted WeChat users only** | Deny-all senders by default; tightened workspace and policy |
| MCP plugins | Any harness with the plugin installed | Plugins run with the same privileges as the main binary (see below) |

**Token holder = controlled local RCE**: that is the stated semantics of
`gh server` (docs/server.md §1); loopback binding plus authentication is its
entire boundary. Binding a non-loopback address exposes local execution to
the network — add your own protections first.

## Channel defaults

### Terminal and embedding

- The unconfigured policy is `off` (no sandbox). Set `write-workspace`
  explicitly
- **Process-level bash sandboxing is macOS-only** (sandbox-exec). **Linux and
  Windows have no process-level bash sandbox** — the write/edit file-tool
  boundary holds on every platform, but bash commands on those platforms are
  not OS-restricted; out-of-bounds behavior relies on escalation prompts
- Ask-on-escalation: denial records are consumed once and rejection is final;
  a `sandbox_permissions` retry must carry a justification and match the
  exact denied command
- Sub-agents inherit the root policy and have no escalation capability
  (constructively denied); `set_policy` tightening propagates tree-wide

### gh server

- Defaults to `127.0.0.1:7400`; every endpoint requires a bearer token
  (`subtle.ConstantTimeCompare`); the token is 24 crypto-random bytes hex,
  persisted 0600
- Request body limits (64MB main / 1MB config endpoints),
  ReadHeaderTimeout/ReadTimeout/IdleTimeout; access logs contain no
  credentials or bodies
- Client function tools never execute locally (they enter the pause
  protocol; the caller runs them and feeds results back); turn-settled
  webhooks (`GRF_TURN_WEBHOOK_URL`) carry an HMAC-SHA256 signature keyed by
  an environment-injected secret

### WeChat channel

- **Sender allowlist denies everyone by default** (`weixin.allow_senders`);
  unauthorized senders get a setup hint, throttled to one per sender per
  60 seconds
- Channel defaults: workspace `~/.grf/weixin/ws` (not the whole home) and
  policy `write-workspace|read-workspace` (override via `weixin.policy`)
- The global `auto_approve` never applies: per-tool / escalation / plan
  approvals are forwarded as WeChat questions; timeouts count as rejection
- Inbound media downloads are pinned to `*.weixin.qq.com` hosts (prevents
  SSRF via attacker-controlled `full_url`); the daemon log and login state
  are 0600 (the log contains message texts and login QR links)

### MCP plugins

- Plugin processes run **with the same privileges** as the main binary:
  their file access and command execution bypass the file-tool boundary and
  the bash sandbox. Install only trusted plugins
- Plugin remote mode (`serve --addr`) **requires a bearer token** (a random
  one is generated and printed when `--token` is omitted); the server sends
  no CORS grant headers (so a random web page cannot fetch the loopback port
  and drive a browser/computer plugin)
- Plugin child processes get the same secret-stripped environment as bash
- `gh plugins download` validates filenames (path components rejected);
  official plugins and self-update share HTTPS + same-directory SHA256SUMS
  verification

## Trust-boundary files (workspace-shipped = untrusted input)

| File | Risk | Handling |
|---|---|---|
| `.grf/grants.json` | Pre-authorized out-of-workspace **write grants** (skip prompts) | Disclosed in the system prompt on load; `/` and home-dir entries refused; model-side writes hard-blocked |
| `.grf/memory.json` | Cross-session, cross-user prompt-injection carrier | Disclosed as untrusted on load; instructions inside require user confirmation before acting |
| `.grf/sessions/` | Session transcripts (may contain secrets) | 0600; never commit them |
| `SKILL.md` / default skills | Injected into the system prompt | Only injected when explicitly configured as a default skill (an embedder trust decision) |
| `grf:session.host_context` | Host briefing injected at system-prompt position | Embedder semantics: the host must keep untrusted input out of it |

## Processes and supply chain

- **Child env stripping**: bash and plugin child processes drop
  secret-looking environment variables (`*API_KEY*` / `*TOKEN*` /
  `*SECRET*` etc., `tools/env.go`)
- **Self-update / plugin downloads**: HTTPS + same-directory SHA256SUMS
  integrity checks — they catch transport corruption but **not a fully
  compromised download server** (manifest and artifact share the channel);
  `GRF_DOWNLOAD_BASE` overrides the base URL (private deploys / debugging)
- **Workflow JS interpreter guards**: nesting depth cap (prevents fatal
  stack overflow), step budget with ctx observation (prevents spin-wedge),
  16MB per-string-operation cap, bounded concurrency
- **Background job output buffers** are capped at 8MB (memory-bomb guard)

## Deployment checklist

1. Set `policy` explicitly (never rely on the `off` default); remember bash
   is unsandboxed on Linux/Windows
2. Keep `gh server` on loopback; put TLS reverse proxies and access control
   in front of any wider binding
3. Configure `weixin.allow_senders` before putting the WeChat channel to work
4. Install only trusted plugins; in remote mode use the generated token and
   bind loopback/internal addresses only
5. When working with an untrusted repo, inspect (and if needed delete) its
   `.grf/` directory first
6. Don't rely on anything beyond "children can't see it" for passing secrets
   around — export explicitly inside commands when needed
