# Usage guide

[简体中文](usage.md) | **English**

The three run modes of gh, full-screen interaction, slash commands, and a
cheat sheet for the 28 built-in tools. Configuration and the policy rule
string live in the [configuration reference](configuration.en.md); the full
HTTP service protocol is in [HTTP access](server.en.md).

## Three run modes

| Mode | Launch | For |
|---|---|---|
| **Full-screen** (default) | `gh` | Daily use: alternate screen + input box, streaming output, tool events, forms and approval popups |
| **Line REPL** | automatic when not a TTY (pipes/CI) | Scripted environments: line-by-line I/O, no ANSI escapes |
| **One-shot task** | `gh "<task description>"` | Fire-and-forget: streams the result, then exits — fits scripts and CI pipelines |

Common launch flags: `-resume` (restore the last session), `-plan` (start in
plan mode), `-model <provider/model>` (ad-hoc model).

On startup the full-screen mode probes for updates in the background (skipped
for dev builds, silent on network failure) and shows a hint under the welcome
line when one exists; `gh update` updates manually (`-y` skips confirmation).

## Full-screen keybindings

| Key | Action |
|---|---|
| `↑` / `↓` | Input history (`~/.grf/history`); pick from slash-command hints |
| `←` `→` `Home` `End`, `Ctrl+A/E/K/U/W` | Line editing (UTF-8 aware) |
| `Enter` | Submit (completes the command hint first; submitting mid-run = steering, merged at the next step boundary) |
| `Tab` | Complete command / toggle mouse reporting |
| `PgUp` / `PgDn` / wheel | Scroll the content area |
| `Ctrl+O`, mouse click | Expand/collapse thinking summaries and merged tool-call blocks |
| `F2` or `/plan` | Toggle plan mode (`Tab` cycles auto → plan → approval) |
| `Ctrl+C` | Interrupt the current run (exit when idle) |
| `Ctrl+D` | Exit on empty input |
| Mouse drag | Select content above line-wise; release to copy via OSC52 |

Slash commands: `/help` `/model` `/resume` `/plan` `/goal` `/save` `/exit`.

## Plan and approval modes

- **Plan mode**: the model researches but cannot change anything (write/edit/
  bash etc. are hard-blocked); it submits the plan via `exit_plan_mode` for
  approval. Approving exits to execution; rejecting can carry feedback back
  into planning. The model may also request entry via `enter_plan_mode`.
- **Approval mode**: every tool call gets a confirmation popup (optionally
  with textual feedback the model uses to adjust its approach).

## Session persistence

Each turn is persisted to `.grf/sessions/` (full messages plus the original
system prompt); `gh -resume` or `/resume` inside the full-screen UI restores
from a list. `/save` saves explicitly. Session transcripts may contain
sensitive content — they are written 0600 and must not be committed.

## WeChat channel

```sh
gh connect weixin       # QR login, then a background daemon (log: ~/.grf/weixin.log)
gh disconnect weixin    # stop
```

- **Sender allowlist**: all messages are denied by default. The first
  unauthorized message gets a reply explaining how to authorize (including
  your WeChat ID); add the ID to `weixin.allow_senders` in `settings.json`
- Channel defaults: workspace `~/.grf/weixin/ws`, policy
  `write-workspace|read-workspace` (override with `weixin.policy`); tool
  approvals are forwarded as WeChat questions (timeout counts as rejection)
- Plain text sent mid-run = steering merged into the current task; images and
  files queue for the next turn; `/new` starts a fresh session

## Tool cheat sheet

The definitions (name/description/parameter schema) of all 28 tools come
solely from `tools/spec/tools.json` (embedded at build time); embedders
extend via `RegisterCustom` and trim via `DisabledTools`.

| Group | Tools |
|---|---|
| Files | `read` (line numbers/paging), `write`, `edit` (unique-match/replace_all), `glob` (mtime order/100 inline), `grep` (250 inline/include filter), `read_image` (PNG/JPEG/GIF decode+scale, WebP passthrough) |
| Execution | `bash` (workdir/timeout/exit-code marker/long-output truncation to disk/sandbox + ask-on-escalation/timeout promotes to a background job), `job_output` / `job_kill` / `job_list` (background jobs, streaming incremental reads, 8MB buffer cap) |
| Network | `web_search` (1-4 merged queries, DDG answer + source list) |
| Memory | `memory` (cross-session JSON entry store: project scope defaults to the workspace `.grf/memory.json`, user scope to `~/.grf/memory.json`; list/find/add/update/delete, self-chosen unique keys, three-level find matching, 4KB per-file cap) |
| Interaction | `ask_user_question` (forms), `enter_plan_mode` / `exit_plan_mode` (plan approval), `todo_write` (panel), `skill` (SKILL.md catalog), `set_policy` (model raises a popup with justification; only the user picks, session-scoped) |
| Goals | `create_goal` / `get_goal` / `update_goal` (revision checks, blocked auto-retry) |
| Orchestration | `subagent` / `subagent_fork` (background by default + completion notices), `send_message` / `interrupt_agent` / `list_agents`, `workflow` (JS scripts + concurrency/cap limits + schema validation + resource guards), `ralph` (fresh-agent loop) |
| Plugins | registered dynamically by MCP plugins (17 for grf-browser / 10 for grf-computer — see the [plugin docs](plugins.en.md)) |
