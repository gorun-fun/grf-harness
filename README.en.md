# grf-harness (gh)

[简体中文](README.md) | **English**

A zero-dependency terminal AI agent harness: pure Go standard library (no
third-party deps in go.mod), 28 built-in tools, full-screen terminal UI by
default, embeddable as a Go module, and exposable to any-language hosts via
`gh server` speaking the OpenAI Responses API shape. It reads code, runs
commands, edits files and spawns sub-agents in your workspace until the job
is done.

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE) ![Go](https://img.shields.io/badge/Go-1.26-00ADD8) ![Dependencies](https://img.shields.io/badge/deps-0-success) ![Platforms](https://img.shields.io/badge/platforms-macos%20%7C%20linux%20%7C%20windows-9feaf9)

## Contents

| Doc | What's inside |
|---|---|
| [Usage guide](docs/usage.en.md) | Three run modes, full-screen keybindings, slash commands, 28-tool cheat sheet |
| [Configuration reference](docs/configuration.en.md) | Every `settings.json` field, the policy rule string, environment variables |
| [Security model](docs/security.en.md) | Threat boundaries, per-channel defaults, must-read before deploying |
| [Go library embedding](docs/integration.en.md) | `app.New` facade, event stream, multi-session |
| [HTTP access](docs/server.en.md) | `gh server`: an ORS-compatible Responses API service |
| [MCP plugins](docs/plugins.en.md) | grf-browser / grf-computer — install, configure, security model |
| [Plugin development](docs/plugin-dev.en.md) | mcpserver SDK, runtime contract, build & distribution |
| [Architecture overview](docs/architecture.en.md) | Layering, the turn loop, pause/resume protocol, concurrency rules |

## Highlights

### 🖥 Full-screen terminal experience

Alternate screen + bottom input box + scrolling content area; reflows and
repaints on terminal resize; thinking progress lines, raw-text output (no
markdown rendering), collapsible tool-event blocks (`Ctrl+O`), todo/goal
panels, slash-command hints, ask forms and plan-approval popups, mouse wheel
and drag-select copy (OSC52). Falls back to a plain-text line REPL when stdin
or stdout is not a TTY.

### 🔧 28 built-in tools

File read/write/edit/search (`read` / `write` / `edit` / `glob` / `grep` /
`read_image`), bash execution with background jobs (`bash` / `job_*`),
cross-session memory (`memory`, two scopes), web search (`web_search`),
interactive Q&A and plan mode (`ask_user_question` / `enter/exit_plan_mode`),
goal tracking (`create/get/update_goal`), and orchestration (`subagent` /
`subagent_fork` / `send_message` / `interrupt_agent` / `list_agents` /
`workflow` / `ralph`). Definitions are embedded from
`tools/spec/tools.json` (single source of truth); cheat sheet in the
[usage guide](docs/usage.en.md#tool-cheat-sheet).

### 🤖 Orchestration

- **Sub-agents**: background agents with inherited policy and toolchain;
  results are injected into the parent session as notices
- **Goals**: revision-checked goal state machine with automatic blocked-retry
- **Workflow**: JS-script orchestration (`agent()` / `pipeline` / `parallel`,
  top-level `await`, schema validation) executed by the built-in mini JS
  interpreter with resource guards (depth / step budget / memory caps)
- **ralph**: fresh-agent iteration loops until COMPLETE / BLOCKER

### 🛡 Security boundaries

- **Policy rule string**: `write-workspace` (macOS sandbox-exec limits bash
  writes to the workspace + read-before-overwrite), `read-workspace` (read
  boundary), `off`
- **Ask-on-escalation**: out-of-bounds writes or sandbox denials pause for
  user approval; rejection is final
- **Approval/plan modes**: per-tool confirmation / plan-first execution
- **Secret isolation**: secret-looking environment variables are stripped
  from bash and plugin child processes
- Full details in the [security model](docs/security.en.md)

### 📦 Three ways in

| Way | For | Doc |
|---|---|---|
| **Terminal CLI** (`gh` full-screen / `gh "task"` one-shot) | Daily use, scripts, CI | [Usage guide](docs/usage.en.md) |
| **Go library** (`app.New`, everything programmable) | Go apps | [Go embedding](docs/integration.en.md) |
| **HTTP Responses API** (`gh server`, ORS-compatible) | Any-language hosts | [HTTP access](docs/server.en.md) |

### 🔌 MCP plugins

Extended capabilities come from independently compiled plugins:
**grf-browser** (chromedp browser automation, 17 tools, aligned with
playwright-mcp) and **grf-computer** (cross-platform computer control,
10 tools). Auto-discovered locally over stdio or connected remotely over
HTTP; `gh plugins` runs a health check. See [MCP plugins](docs/plugins.en.md);
write your own with the [plugin dev guide](docs/plugin-dev.en.md).

### 💬 WeChat channel

`gh connect weixin` starts a daemon that answers WeChat messages with the
local agent (one session per user). All senders are denied by default
(allowlist); the workspace and policy are independently tightened; tool
approvals are forwarded as WeChat questions.

## Quick start

### 1. Install

```sh
# Option A: download from the release server
#   https://download.gorun.fun/grf-harness/<platform>/<version>/gh
# Self-update afterwards with `gh update` (SHA256SUMS-verified)

# Option B: build from source (zero deps — clone and go)
git clone https://git.gorun.fun/share/grf-harness
cd grf-harness
go build -o gh .        # or ./run.sh (vet + build; cross-compile with ./run.sh all)
```

### 2. First-time configuration

Write any OpenAI-compatible endpoint into `~/.grf/settings.json`
(relocatable via `GRF_HOME`):

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
  "policy": "write-workspace"
}
```

Or override ad hoc with environment variables: `OPENAI_BASE_URL` /
`OPENAI_API_KEY` / `GRF_MODEL`. Every field is documented in the
[configuration reference](docs/configuration.en.md).

### 3. Run it

```sh
gh                     # full-screen interactive (default; line REPL when not a TTY)
gh -resume             # resume the last session
gh "count the lines of go files in this directory"   # one-shot task, then exit
```

## Common commands

```sh
gh model               # model configuration page (multi-provider / auto-detect)
gh model set <p/m>     # set the default model from scripts
gh config              # config summary (prompt / steps / policy subcommands)
gh server              # Responses API service (default 127.0.0.1:7400 + bearer)
gh connect weixin      # connect the WeChat channel (QR login, then background daemon)
gh plugins             # plugin health check; gh plugins download fetches official plugins
gh update              # self-update (-y skips confirmation)
gh version
```

## Security notes (short version — full model here)

- gh drives **local bash and file tools**: the default policy is `off`
  (no sandbox) — set one explicitly; **Linux/Windows have no process-level
  bash sandbox** (file-tool boundaries hold on all platforms)
- `gh server` is a controlled local RCE entry: loopback-only + bearer token
  by default; read [docs/server.en.md](docs/server.en.md) before binding
  wider
- The WeChat channel denies all senders until `weixin.allow_senders` is set
- Plugin processes are not constrained by harness policy — install only
  trusted plugins
- Treat `.grf/` files shipped inside a cloned repo (grants/memory) as
  untrusted input; their presence is disclosed in the system prompt

## Development

```sh
./run.sh              # vet + build (produces ./gh; macOS adhoc signing included)
go test ./...         # all tests (no network or API key needed)
go test -race ./agent ./tools ./term ./js ./llm ./server
```

Workspace conventions (layering rules, pitfall list) live in `AGENTS.md`;
architecture is documented in [docs/architecture.en.md](docs/architecture.en.md).

## Relationship to the original gh project

This repo is a rebuild of `~/AiProjects/gh` (the bubbletea + glamour
version): all third-party dependencies removed (homegrown `term` package and
mini JS interpreter), the toolchain redefined as a 28-tool spec embedded in
`tools/spec/tools.json`, and the agent core rewritten to the documented
semantics; markdown is no longer rendered — raw text is shown by requirement.
Multi-provider config, session persistence/resume, input history and the
model configuration page carry over.
