# grf-harness architecture overview

[简体中文](architecture.md) | **English**

A repo-wide map organized by functional lines. Embedders should read
[integration.md](integration.en.md) (Go library) and [server.md](server.en.md)
(HTTP); workspace rules live in AGENTS.md at the repo root.

## 1. Functional layers (four tiers)

```
┌─ Access surfaces (parallel, thin assemblies, all wiring agent.Build) ┐
│  main (CLI, package main)                                            │
│    ├─ tui/      full-screen chat + model config page (interactive)   │
│    ├─ app/      library facade: app.New + one-shot + REPL + TaskHost │
│    ├─ server/   HTTP Responses API (ORS-compatible, gh server)       │
│    └─ weixin/   WeChat ClawBot channel (gh connect weixin daemon)    │
├─ Core ───────────────────────────────────────────────────────────────┤
│  agent/       turn loop (turn.go) + pause/resume (pause.go) +        │
│               sub-agent Manager + goal state machine + workflow/ralph│
├─ Capability ─────────────────────────────────────────────────────────┤
│  tools/       28 tool implementations + Registry (embedded spec)     │
│  js/          mini JS interpreter (workflow-only, controlled subset) │
│  llm/         OpenAI-compatible client (SSE/multimodal) + fallback   │
│               tool-call parsing from message bodies                  │
├─ Foundation ─────────────────────────────────────────────────────────┤
│  term/        terminal primitives (raw/input parsing/CJK width/frame)│
│  config/      ~/.grf/settings.json (env > file > default)            │
│  store/       .grf/sessions persistence                              │
└──────────────────────────────────────────────────────────────────────┘
```

Dependency rule: strictly top-down, acyclic. Sibling dependencies across the
capability/foundation tiers are only `tools→llm` (wire shape of tool
definitions) and `store→llm` (message wire format). `term` is the lowest text
and terminal library; any higher layer (including agent) may use its pure
text helpers (`term.FirstLine` etc.).

## 2. Package responsibilities

| Package | Responsibility | Key files |
|---|---|---|
| `main` (root) | CLI dispatch and subcommands | `main.go`, `chat.go`, `settings_cmd.go`, `connect.go`, `server_cmd.go`, `daemon_unix/other.go` (platforms) |
| `tui` | Full-screen chat state machine | `chat.go` (event loop), `hub.go` (stream merging), `render.go` (framing/stats), `input.go`, `popups.go`, `host.go`, `modelpage.go`, `editor.go` |
| `app` | Library facade | `app.go` (Options/New/session facade), `render.go` (one-shot render + RunTask), `taskhost.go`, `repl.go` |
| `server` | ORS Responses API | `server.go` (routing/errors), `create.go` (main endpoint), `session.go` (session pool, durable on disk), `bridge.go` (events→SSE), `dto.go`, `extras.go`, `compact.go`, `deferred.go` |
| `weixin` | WeChat channel | `bot.go` (Run/login/polling), `pool.go` (per-user agent pool), `mux.go` (dispatch/turn execution), `login.go`, `media.go`, `ilink.go`, `host.go`, `state.go` |
| `agent` | Core loop and orchestration | `turn.go` (runTurn), `pause.go`, `run.go` (Run* entry family), `agent.go`, `history.go`, `notice.go`, `events.go`, `manager.go`, `goals.go`, `workflow.go`, `ralph.go`, `build.go` (assembly), `prompt.go` |
| `tools` | Tools and registry | `registry.go`, `spec/` (single source of definitions), `fs.go`/`glob.go`/`grep.go`/`fspolicy.go`, `bash.go`, `jobs.go`, `web.go`, `image.go`, `askuser.go`, `todo.go`, `skill.go`, `plan.go`, `host.go`, `artifacts.go` |
| `js` | JS interpreter | `lexer.go`, `parser.go`, `interp.go`, `eval.go`, `value.go`, `builtins.go`, `namespace.go` |
| `llm` | Model client | `client.go`, `message.go`, `stats.go`, `toolcall_fallback.go`/`dsml.go`/`fallback_tags.go`/`stream_display.go` (the fallback-parsing line) |
| `term` | Terminal and text | `term.go` + `tty_*` (platforms), `input.go`, `width.go`, `screen.go`, `lines.go` |
| `config` / `store` | Config / session store | single-file packages |

## 3. Key mechanisms

### 3.1 The turn loop (agent/turn.go)

`runTurn` is the one and only tool-loop implementation: merge user input (or
resume dispositions) → LLM request → execute tool calls → feed results back →
repeat, until a final reply or a pause point. All entry points
(`Run/RunAs/RunWithImages`, `RunTurn/RunTurnParts/Resume`, `RunAutoGoal`)
are thin delegates. MaxSteps bounds the loop; when output is truncated by a
token limit the turn does not end — a notice prompts the model to resend.

Tool calls within one batch go through `dispatchBatch` (dsh-style
semantics): consecutive **parallel-safe** tools (`parallelSafeTools`: the
pure read-only set — read/read_image/glob/grep/web_search/skill/job_list/
list_agents/get_goal etc.) enter a bounded pool (`MaxParallelTools`, default
8); the rest (side-effecting, pausing, custom/plugin/client tools) run
exclusively, serially, forming an ordering barrier. Results merge into
history in completion order regardless of the model's order — the request
snapshot is re-ordered closed by `canonicalizeToolResults` following the
initiating assistant's tool_calls sequence, keeping the wire format legal.
On turn cancellation, calls never started get synthesized
aborted-before-dispatch results (not run, safe to resend); unclosed calls
left behind by crashes are closed in the snapshot as outcome-unknown
placeholders (retry only idempotent operations). A streaming reply
interrupted by cancellation has its already-delivered prefix merged into
history as an interrupted assistant message (the next request contains what
the user saw). All tool results pass the shared pruning net
`pruneResultText` (>8192 bytes keeps head 4096 + tail 1024, UTF-8 aligned;
tools' own truncation + spillover to disk happens first). Identical
consecutive calls (name + arguments) draw escalating reminders at 3/5/8
thresholds (anti-spin). Mid-run steering (`agent/steer.go`): user input
queued via `Agent.Steer` merges as a plain user message at step boundaries
(running tools are not interrupted; idle-period queuing is drained at the
start of the next turn); the TUI's Enter-during-run is steering, unmerged
items are harvested at turn end into the next turn; in weixin, plain-text
messages during a run are steered with an acknowledgment (media/commands
queue for the next turn).

Session compaction (`agent/compact.go`): at the start of a new turn the
estimated token count is checked (default threshold 100k, overridden by
`Settings.CompactThreshold`, negative disables). Over the threshold — or on
manual `/compact` — the current model summarizes the session into a single
message and history collapses to [system verbatim, `[conversation summary]`].
Compaction only happens at turn boundaries (rejected mid-run or while
paused; never produces orphan tool messages); the automatic path fails
open. Summary output is normalized (strip `<summary>` tags and prefixes,
reasoning fallback — the transcript line style tempts some models to wrap
their output).

### 3.2 Pause/resume protocol (agent/pause.go)

Designed for cross-request interaction answers (gh server). **Pause state
lives entirely in the persisted history**: the tool loop returns mid-flight,
leaving an unclosed `tool_call` at the tail of history (a legal
intermediate state of the OpenAI wire format) whose arguments are the
interaction request itself — so the process can restart and resume. Three
triggers:

- `function`: client-executed tools (`Registry.RegisterClient`, ORS
  function tools)
- `question`: ask_user questions under a deferred host
- `tool_approval`: tool calls under a deferred host + approval mode

Resume = `Resume(ctx, []ResumeInput)` injects each unclosed call's
disposition (result / answers / approval) into history and continues. The
`Run*` family returns `*PausedError` at a pause point (it does not handle
pauses — callers should switch to RunTurn/Resume). Detecting the paused
state only needs history (`HasPausedCalls`).

### 3.3 Assembly convergence: agent.Build is the single assembly truth

All four host surfaces call `agent.Build(agent.Options{...})` directly (or
via the thin `app.New` wrapper) to get an `*agent.Harness`: root agent,
sub-agent factory (Manager), goal/todo/FsPolicy/JobManager/Store/skill
catalog all fall into place; the 28 tools register per root/sub-agent rules
(the goal trio, ralph and ask_user are root-only). Surfaces do not depend on
each other — `server` does not import `app`, `weixin` does not import `app`;
only main wires them into a CLI. Multiple sessions = multiple Harness
instances (see integration.md §9, the server's session pool, weixin's
agentPool).

### 3.4 Observation policy and sandbox escalation (tools/fspolicy.go, bash.go)

- `write-workspace` (one of the unified policy defaults): write/edit require
  a prior same-session read before overwriting an **existing** file
  (os.Stat distinguishes new files).
- `sandbox=write-workspace` (macOS sandbox-exec): out-of-bounds writes
  return a `[sandbox: file access denied]` marker plus the one-shot
  escalation retry protocol (`sandbox_permissions` + `justification` →
  `Host.ApproveEscalation`).
- Path resolution: relative paths of file tools resolve against the
  **workspace**, not the process CWD.
- Foreground bash timeout promotion (promoteOnTimeout): instead of killing
  the process, the command becomes a background job and hands over
  (one-shot reply with produced output so far + job id; job_output continues
  from the read cursor). Once promoted, the command belongs to the job and
  survives the current turn's cancellation. Without a JobManager it falls
  back to the old semantics (SIGINT on expiry).

### 3.5 Event streams

Three non-conflicting callback chains (field tables in integration.md §5):

- `OnToolEvent` (structured): advances by CallID running → done/error/
  denied, with full inputs and outputs; sub-agent events flow back to root.
- `OnEvent` (line-style): tool/result/notice/todos/goal/plan/phase.
- `StreamHooks`: token-by-token body/thinking.

Each surface maps them onto its own medium: TUI → content blocks and the
stats bar; app → direct stdout; server → SSE events (bridge.go); weixin →
console log + message replies.

### 3.6 Concurrency rules (per-surface single-thread disciplines)

| Surface | Rule |
|---|---|
| tui | all state changes happen only on the event-loop goroutine; background callbacks go through the `c.events` channel (onEvent/eventMsg) |
| agent | `history` always under `a.mu`; async notices via `EnqueueNotice` (idle triggers OnIdleNotice continuation), drained only right before LLM requests (tool results must follow the assistant message — an OpenAI protocol requirement) |
| server | one turn per session (beginTurn/endTurn); concurrent requests get 409, no queueing |
| weixin | one goroutine per user (userMux); the same user serially, different users concurrently; agentPool is lock-guarded |

### 3.7 Single source of tool definitions

The definitions (name/description/parameter schema) of the 28 built-in
tools are embedded from `tools/spec/tools.json` (go:embed); the registry's
`Register` rejects names outside the spec. Embedders extend via
`RegisterCustom` (bring your own definition, no clashes with built-ins);
ORS client tools go through `RegisterClient`. Changing tool behavior never
changes spec text.

### 3.8 Cross-session memory (tools/memory.go)

The `memory` tool stores cross-session memory as a **JSON entry store**
(CRUD), with two scopes and fixed locations (it bypasses FsPolicy — the
model cannot pass a path): **project (default, the main scope for terminal
use) = the workspace's `.grf/memory.json`, user = `~/.grf/memory.json`**
(`config.Home()`, relocatable via GRF_HOME; paths are wired by
`memoryDeps` in `agent/build.go`, shared by root and sub-agents). The store
is a plain array of `{key, content, updated}` — keys are caller-chosen
(unique per scope, single line, ≤64 chars, usable for search; add rejects
duplicate keys and points to update). Actions: list (without scope, both)
/ find (three-level matching: exact key → key substring → content
substring, case-insensitive; no hit returns a hint, not an error) / add /
update / delete — scope defaults to project; only list defaults to both.
Hard cap of 4KB per file (over-cap writes are rejected with guidance to
prune; list truncates over-cap stores to the first 20 entries); reads and
writes are serialized by a process-level mutex (gh server sessions share
the same pair of files); corrupted JSON errors out instead of silently
wiping. Allowed in plan mode — memory is harness metadata, not a workspace
deliverable, like todo_write.

## 4. Platform support

termios ioctls split by build tags into darwin/linux (`term/tty_*.go`);
Windows provides the same capabilities via Console APIs
(`term/tty_windows.go`: the input handle enables
ENABLE_VIRTUAL_TERMINAL_INPUT so arrow keys etc. are translated by
conhost/Windows Terminal into ANSI sequences feeding the existing parser in
`input.go`, and Ctrl+C arrives as byte \x03 instead of a signal, matching
unix raw behavior; the output handle enables VT processing so alternate
screen / full-frame repaint escapes work. On legacy conhost without VT,
OpenRaw errors and upper layers fall back to the plain-text line mode).
Resize listening is platform-split (`term/resize_*.go`: SIGWINCH on unix,
250ms polling on Windows, none elsewhere). Other platforms fall back to
non-interactive; daemon detachment and process probing live in
`daemon_unix.go` / `daemon_other.go` (package main). Cross-compiling: see
`./run.sh` (artifacts in `dist/`).
