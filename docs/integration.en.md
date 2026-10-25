# grf-harness integration guide

[简体中文](integration.md) | **English**

For host programs embedding grf as the agent capability inside their own
apps. grf has zero third-party dependencies and no `internal/` directory —
every package (module path `git.gorun.fun/share/grf-harness`) is importable
by external Go projects. Non-Go hosts can go through HTTP (`gh server`, see
[server.md](server.en.md)) or a CLI subprocess (§10); the architecture
overview is [architecture.md](architecture.en.md).

## 1. Getting the module

The module path matches the public repo:
`git.gorun.fun/share/grf-harness`
(https://git.gorun.fun/share/grf-harness), with version tags shaped like
`v0.0.1-alpha.2` (the first tag carrying this module path).

**Option A: plain go get** (host machine can reach git.gorun.fun)

```sh
# private git service: make go skip the public proxy and checksum DB
go env -w GOPRIVATE=git.gorun.fun

go get git.gorun.fun/share/grf-harness@v0.0.1-alpha.2
```

```go
import "git.gorun.fun/share/grf-harness/app"
```

Git authentication follows the machine's regular git configuration (https
credentials / ~/.netrc / ssh); with a self-signed server certificate, point
git at the CA: `git config --global http.https://git.gorun.fun.sslCAInfo /path/to/ca.pem`.

**Option B: local path replace** (co-development / offline; clone or symlink
the repo, then point at the directory)

```
require git.gorun.fun/share/grf-harness v0.0.0

replace git.gorun.fun/share/grf-harness => /path/to/grf-harness
```

Since grf is dependency-free, neither option adds any third-party require to
the host's `go.mod`.

## 2. Library access: app.New

The `app` package is the embedder's facade — one build yields the complete
harness (agent loop, 28 tools, session storage):

```go
import "git.gorun.fun/share/grf-harness/app"

a, err := app.New(app.Options{
    Workspace:     "/path/to/workspace",     // default cwd
    Client:        client,                   // *llm.Client; nil reads ~/.grf/settings.json
    Host:          host,                     // tools.Host; nil means NoopHost
    MaxSteps:      100,                      // tool-loop round cap; 0 uses config/default
    SystemPrompt:  "You are an assistant for …", // see §3
    DepthLimit:    3,                        // sub-agent depth cap
    DisabledTools: []string{"bash"},        // see §4
    Skills:        tools.NewSkillCatalog(),  // take over / disable skill loading; see §6
    VerboseEvents: func(kind, detail string) { /* see §5 */ },
    OnToolEvent:   func(e agent.ToolEvent) { /* see §5, for tool timelines */ },

    // runtime policy overrides: effective for this build only, never persisted;
    // unset items still follow the config file
    Policy:      "write-workspace|read-workspace", // unified rule string; empty = off
    AutoApprove: &autoYes,                  // *bool; auto-approve for non-interactive use
})
if err != nil { ... }
defer a.Close()

err = a.Harness.Root().Run(ctx, "export today's report for me")
```

The policy overrides let embedders pin sandbox/approval behavior **without
depending on the machine's global config**; items not overridden still read
`~/.grf/settings.json`. `GRF_HOME` relocates the global config and
user-level skills (`settings.json`, `~/.grf/skills`); **session archives are
not governed by it** — they follow `Options.Workspace` into
`<workspace>/.grf/sessions/` (see §9).

For finer control (custom `config.Settings`, the sub-agent factory, the goal
state machine) use `agent.Build(agent.Options{...})` directly; fields
correspond to `app.Options`.

## 3. Injecting the default prompt

Precedence: `Options.SystemPrompt` > config-file `system_prompt` (what
`gh config prompt set` persists) > built-in default. All three decide only
the **custom section** — the agent's identity and working style (the
built-in default carries the terminal-UI identity, the AGENTS.md
conventions, `@`-path references and a final-reply deliverables list). **The
built-in tool-usage section (inner prompt) is always appended**: it is
assembled per actually-available built-in tool (not disabled, registered for
the role), and no custom setting can replace it; dynamic fact sections
(workspace path, model name, skill catalog) are also still appended (see
`agent.BuildSystemPrompt`).

## 4. Tools: custom, replaced implementations, disabled

- **Register new tools**: `tools.Registry.RegisterCustom(def llm.Tool, h
  Handler)` is the embedder extension point — the definition
  (name/description/parameter schema) is fully caller-provided and sent to
  the model with requests. Names must not clash with the 28 built-ins (the
  spec is the single source of built-in definitions and cannot be overridden
  via this route); violations panic.

  ```go
  a.Root().Reg().RegisterCustom(llm.Tool{
      Name:        "export_report",
      Description: "export the current session report to a format",
      Parameters:  map[string]any{"type": "object", "properties": map[string]any{
          "format": map[string]any{"type": "string"},
      }},
  }, func(ctx context.Context, args string) (tools.Result, error) {
      return tools.TextResult("exported"), nil
  })
  ```
- **Replace built-in implementations**: re-`Register` a same-named tool on
  the root agent's registry to swap the built-in Handler. Note
  `tools.Registry.Register` requires the name to be among the 28 in the
  built-in spec; unknown names panic.

  ```go
  a.Root().Reg().Register("web_search", myHandler) // precondition: the tool is registered
  ```
- **Disable**: `Options.DisabledTools` removes by name (built-ins and custom
  alike), identically for the root agent and sub-agents. Disabled tools are
  not sent to the model; if the model calls one anyway, `Execute` returns an
  unknown-tool text (the session continues).

  ```go
  DisabledTools: []string{"bash", "subagent", "web_search"},
  ```

- Runtime registry operations also work directly:
  `tools.Registry.Disable(names...)`, `Registered()` to list tools.

## 5. Events and streaming

Three non-conflicting callback chains:

**Structured tool events** (recommended for embedder-built tool timelines) —
`Options.OnToolEvent` (wired to the root agent at `app.New`), or
`Agent.SetOnToolEvent` at runtime. Every tool call advances a state machine
by CallID; `Input`/`Output` are **full untruncated originals** — hosts trim
for display themselves:

| Field | Meaning |
|----------|-------------------------------------------------------------|
| `CallID` | Tool-call ID (the tool_call id from the LLM; generated for body-fallback parsing), unique per process |
| `Name`   | Tool name |
| `Status` | `running` → `done` \| `error` \| `denied` (terminal states) |
| `Input`  | Full argument JSON |
| `Output` | Full result text (empty while `running`) |
| `Err`    | Error message when `Status=error` (from `tools.Result.Err`) |
| `AgentID`/`Label` | Originating agent (root is `agent_root`/`main`) |

`running` fires before approval (a card awaiting user approval is still in
progress); `denied` means approval-mode rejection, with `Output` carrying
the reason and user feedback. **Sub-agent tool events also flow back to the
root agent's callback** (distinguish origins via `AgentID`/`Label`); hosts
may show only the root session.

**Progress events** (tool calls, results, errors, todo/goal changes) —
`Options.VerboseEvents` (auto-wired at `app.New`), or `Agent.SetOnEvent` at
runtime, `kind` values:

| kind     | meaning                                |
|----------|-------------------------------------|
| `tool`   | tool call started (detail: name + arg digest)|
| `result` | first line of the tool result (errors included in result text)   |
| `done`   | one Run finished                       |
| `notice` | system notice (sub-agent settled etc.)             |
| `todos` / `goal` / `plan` / `phase` | state changes, detail is a digest or JSON |

String events suit line-style logs; for call-level correlation and terminal
states (in-progress → done/failed cards with expandable output), use
`OnToolEvent` above.

**Streaming text** — `Agent.SetStreamHooks(content, reasoning func(string))`,
token-by-token callbacks for body and thinking. Forward them to your own UI
/ message bus.

Notes:

- Callbacks fire on the agent's run goroutine; hosts must keep callbacks
  thread-safe and non-blocking (queue long work to a channel).
- `app.RunTask` installs its own stdout renderer on the root agent,
  **overriding** previously set OnEvent/StreamHooks — embedders needing
  custom callbacks should drive `Root().Run / RunAutoGoal` directly, not
  through `RunTask`.

## 6. Injecting skills (workspace-directory style)

A skill is just a `<name>/SKILL.md` directory — no other format required.
Write it into the workspace **before** `app.New`; `agent.Build`'s
`LoadSkills` scans once with no hot reload:

```
<workspace>/.grf/skills/report-export/SKILL.md
```

Two levels, workspace first (same name overrides global):

1. `<workspace>/.grf/skills/<name>/SKILL.md` — project scope
2. `~/.grf/skills/<name>/SKILL.md` — user-global

The first line of SKILL.md should be `# Title + one-line purpose` (the
system-prompt catalog digest uses it); the body holds trigger conditions and
steps. Loaded skills automatically appear in the system-prompt catalog; the
model calls `skill({"name": "report-export"})` on demand to read and follow
it — no host wiring needed.

**A default skill is configurable** (`Options.DefaultSkill`): the skill
whose name matches gets its SKILL.md **full text injected into the system
prompt** (no skill tool call needed; effective from session start). **Empty
injects nothing** — the terminal-usage skill `grf-usage` serves only the gh
CLI terminal form (configured explicitly by bootApp); third-party
integrations work differently, so nothing loads by default; integrations
name their own:

```go
a, _ := app.New(app.Options{ DefaultSkill: "grf-start" }) // injects grf-start in full
```

gh server callers configure the same thing per session: send
`"default_skill": "grf-start"` inside `grf:session` on the first request
(persisted to meta.json, kept on lazy load; empty = none).

**Taking over the catalog** (`Options.Skills`): unset loads the two-level
directories above; passing a `*tools.SkillCatalog` **fully replaces** the
default loading — an empty catalog disables skills (no catalog in the
prompt), or assemble programmatically from host-bundled sources with
`NewSkillCatalog().Add`:

```go
// disable default skill loading (no workspace or ~/.grf/skills reads)
a, _ := app.New(app.Options{ Skills: tools.NewSkillCatalog() })

// or host-bundled skills, no directory conventions needed
cat := tools.NewSkillCatalog()
cat.Add("report-export", reportSkillMD)
a, _ := app.New(app.Options{ Skills: cat })
```

Installing host-bundled skills into a workspace:

```go
func installSkills(workspace string, skills map[string]string) error {
    for name, md := range skills {
        dir := filepath.Join(workspace, ".grf", "skills", name)
        if err := os.MkdirAll(dir, 0o755); err != nil { return err }
        if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o644); err != nil {
            return err
        }
    }
    return nil
}
```

## 7. Model management

- **Config-file layer**: `config.LoadFileForUpdate()` returns a writable
  `Settings` → `Provider.AddModel(model)` (dedupes) → `Settings.Save()`
  persists `~/.grf/settings.json` (relocatable via `GRF_HOME`). The TUI
  model config page (`tui.RunModelPage()`, no app dependency, directly
  callable) does exactly this.
- **Runtime**: build `llm.NewClient(llm.Config{BaseURL, APIKey, Model})`
  and pass it via `Options.Client`, bypassing the config file.
- **In-session switch**: `a.SwitchModel(provider, model)` (the cache key
  migrates with the session).

## 8. Programmatic driving (advanced)

All entry points below are exported for hosts writing their own event loop
instead of reusing the CLI/TUI:

- **Multimodal input**: `Agent.RunWithImages(ctx, text, []tools.Image)` —
  sends a user message with images (data URLs); without images it degrades
  to `Run`.
- **Notices with auto-continuation**: `Agent.EnqueueNotice(text)` injects a
  system notice into the session; when idle it triggers the
  `Agent.OnIdleNotice` callback — register it for "background event arrived,
  let the agent continue" (message-channel / bot scenarios).
- **Mid-run steering**: `Agent.Steer(text)` queues a user interjection
  during a run; the next step boundary (after tool results close) merges it
  as a plain user message — running tools are not interrupted and the model
  sees it on its next request. Queued-while-idle entries are drained at the
  start of the next `Run`; `Agent.DrainSteer()` empties the queue (hosts
  convert unmerged entries into the next turn's input at turn end). The
  in-memory queue is not persisted. TUI and the WeChat channel wire this
  built-in.
- **Session compaction**: `Agent.Compact(ctx)` summarizes history into one
  summary message in place (returns the summary; rejected mid-run or while
  paused); `Agent.SetCompactThreshold(n)` controls auto-compaction
  (estimated tokens: >0 explicit, 0 = default 100k, <0 off; also
  configurable via settings `compact_threshold` / `gh config compact <n>`).
  Auto-compaction is judged at new-turn entry and fails open.
- **Goal continuation and background settle**: `Agent.RunAutoGoal(ctx,
  input)` runs one round and auto-continues per goal state; after a round,
  `Agent.RunAndWaitBackground(dur)` waits for background sub-agents and
  retrieves their notices (`app.RunTask` is exactly this combination).
- **Per-tool approval**: with `Agent.SetApproveMode(true)`, every tool call
  first passes `Host.ApproveTool(ctx, name, args)` — `args` is the **full
  argument JSON (untruncated)**; hosts trim for display. Return
  `tools.ApproveDecision{Approved, Feedback}`: on rejection `Feedback` may
  carry user guidance (like "don't touch this file, use …"), returned to
  the model together with the rejection reason.
- **Usage stats**: `Agent.LLMStats()` (rounds / first-token / generation
  time / tokens / cache hit rate) and `Agent.ToolStats()` (call counts /
  cumulative time) for UI or metering.
- **Programmatic sub-agents**: `Harness.Manager`'s
  `Subagent/Fork/SendMessage/Interrupt/WaitForSettled/ListAgents/HandleOf`
  — drive the sub-agent tree without going through the model (`HandleOf(id)`
  fetches a handle by id).
- **Concurrency tuning**: `Agent.SetMaxParallelTools(n)` (default
  `agent.DefaultMaxParallelTools = 8`) — the pool cap for parallel-safe
  tools in one batch.
- **Extending the job table**: `tools.JobManager.NewFinal(kind, descr)`
  creates a final-type one-shot pending job and `FinishFinal(id, result,
  err)` settles it — embedders mount their own async tasks onto the `/jobs`
  view (built-ins only create stream-type jobs via bash timeout promotion).
- **Prompt caching**: `Harness.CacheKey()/SetCacheKey()` is the
  session-level prompt_cache_key, persisted with the session file and
  reused on resume (`app.Restore` handles it).

Interactive hosts can use `app.NewTaskHost(autoApprove)` (stdin/stdout
interaction with TTY autodetection) or compose `tools.NoopHost` and
implement the five-method `tools.Host` interface themselves (AskUser /
ApproveEscalation / ApprovePlan / ApproveTool / NotifyEvent).

## 9. Multi-session (concurrent conversations)

When a host needs several parallel conversations (multi-tab chat,
multi-user bots): **one conversation = one `app.New` (Harness) instance**.
All session state lives inside the instance; instances share no in-memory
state, and each conversation can `Run` concurrently on its own goroutine.
Per-instance exclusive resources:

| State | Notes |
|---------------------|---------------------------------------------------------------|
| Conversation history / notices / todos | guarded by agent-internal locks, isolated per instance |
| Background jobs | one JobManager per agent, isolated per conversation |
| Observe read-set | per-instance bookkeeping in FsPolicy: files read in conversation A don't unlock overwrites in B |
| Artifacts directory | per-harness under `os.TempDir` (cleaned by `Close()`) |
| Session-archive pointer | per-instance current-file pointer in `Store`; AutoSave overwrites its own file |

Closing a conversation = `a.Close()` + removing it from the table. Existing
in-repo references: `weixin`'s `agentPool` (`map[user]*Agent`, harness on
first touch, dropped on `/new`).

### Persistence ownership and shared-workspace caveats

Session files are `<workspace>/.grf/sessions/session-<timestamp>.json` and
**follow `Options.Workspace`** (`GRF_HOME` doesn't move them). After
`NewSession`/`Restore` the next save starts a new file. When multiple
conversations share one workspace:

- Filename timestamps are second-precision: first saves landing in the same
  second auto-append a numeric suffix (`session-...-2.json`) — no
  overwriting.
- `ListSessions` / `LatestSessionFull` enumerate **all** sessions in the
  directory — interleaved across conversations, filenames carry no
  conversation ID; "latest" is meaningless with multiple conversations.
- `a.AutoSave()` returns no path; to index conversation ↔ file, call
  `a.SaveSession()` yourself (returns the file path).
- There is no session-deletion API; closed conversations leave archive
  files behind (functionally harmless).

### Recommended wirings

| Wiring | How | For |
|------|------|------|
| One workspace per conversation (recommended) | `Workspace: <appData>/conversations/<convID>` | sessions/skills/read-sets fully isolated; resume = load the latest session in that conversation's directory |
| Shared workspace + your own index | call `a.SaveSession()` each turn and store the path in your DB; resume with `store.LoadSessionFull(path)` + `a.Restore(sess)` | conversations sharing workspace files; mind the same-second collisions above |
| Bypass grf session files | snapshot `a.Root().History()` yourself; replay with `RestoreFull` after restart | hosts with their own persistence; cache-hit semantics are already guaranteed (system verbatim + CacheKey replayed with messages) |

Minimal skeleton for one-workspace-per-conversation:

```go
convs := map[string]*app.App{} // conversationID → harness

func openConv(id string) (*app.App, error) {
    a, err := app.New(app.Options{
        Workspace: filepath.Join(appData, "conversations", id), // archives/skills/read-set isolated per conversation
        Client:    client, // OnToolEvent: see §5
    })
    if err != nil {
        return nil, err
    }
    // cross-restart resume: latest session in this conversation's directory
    if infos := a.Harness.Store.ListSessions(1); len(infos) > 0 {
        if sess, err := store.LoadSessionFull(infos[0].Path); err == nil && len(sess.Messages) > 0 {
            a.Restore(sess)
        }
    }
    return a, nil
}

func closeConv(id string) {
    if a := convs[id]; a != nil {
        a.Close() // cleans the artifacts dir; archive files remain for the next resume
        delete(convs, id)
    }
}
```

## 10. Non-Go hosts: HTTP service and CLI subprocess

Two paths, chosen by the level of structure needed:

**HTTP Responses API (recommended, structured)**: `gh server` exposes the
full agent capability in the OpenAI Responses shape (ORS 2026-04-24
compatible; SSE streaming, client function tools, pause-answer chained
continuation for ask_user/approvals, multi-session, cross-restart resume).
Point any language's OpenAI SDK at the `base_url`; endpoint/parameter/event
semantics are in [server.md](server.en.md).

**CLI subprocess (lightweight)**:

```sh
./gh "task"   # one-shot, exits when done
```

Non-TTY environments automatically fall back to the plain-text line mode;
callable from scripts/CI/servers, just parse stdout. Session history lands
in `<workspace>/.grf/sessions/`. Note that events and body text are
interleaved on stdout (no structural boundary) — for structured output use
the HTTP path above or the Go library.

## 11. Notes

- **Concurrency**: agent history always goes through internal locks; for
  custom UIs, deliver events from `VerboseEvents` to a single consumer
  goroutine (see tui's events-channel pattern).
- **Paths**: relative paths of file tools resolve against the **workspace**
  (`Options.Workspace`), not the process CWD.
- **Observation policy**: when policy includes `write-workspace`, write/edit
  require a same-session read before overwriting existing files; new files
  are exempt (the default off has no such constraint).
- **Reply text**: replies are not markdown-rendered; events and results are
  raw text.
- **Session persistence**: `a.SaveSession()` saves manually;
  `a.AutoSave()` persists at turn end; `a.Restore(sess)` continues history
  (system verbatim and cache-key replay included). Isolation and indexing
  for concurrent conversations: §9.
- **Pausable turns (cross-request answers)**: `Agent.RunTurn /
  RunTurnParts` return `(reply, *PausedTurn, error)`; pause points (client
  tools / ask_user / approval) continue via `Agent.Resume(ctx,
  []ResumeInput)`; the paused state persists at the tail of history, and
  after a process restart `RestoreFull` + `Resume` continue as usual.
  `gh server` is exactly this usage (wire shape in server.md §6).
- **Reference implementations**: the `weixin` package
  (`weixin.Run(ctx, prompt, workspace)`) is a complete third-party embedding
  sample — self-contained config loading, client construction, and a
  resident messaging service with one harness per user; the `server` package
  is the second (HTTP form, disk-as-truth multi-session, pause-answer
  chaining) — both instantiate the "four parallel surfaces straight onto
  agent.Build" architecture (see architecture.md §3.3).
