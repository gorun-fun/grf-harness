# Plugin development spec

[简体中文](plugin-dev.md) | **English**

For developers writing MCP plugins for grf-harness: the runtime contract,
tool admission rules, SDK usage, the protocol subset, testing and
distribution. Installing/configuring plugins and the built-in plugins
(grf-browser / grf-computer) are covered in `docs/plugins.md` — that page is
about *using* them, this one about *writing them right*. Every behavioral
clause here matches the current implementations of the host (`plugin` /
`mcp` packages) and the plugin-side SDK (`plugins/internal/mcpserver`) — not
aspirations.

Wording: **MUST** (violations are rejected by the host or render the plugin
unusable), **SHOULD** (strongly recommended; deviate only with a reason),
**MAY** (optional).

## 1. Form and general principles

A plugin is an **independently compiled executable** acting as an MCP server
that provides tools to the host; to the model, plugin tools are as visible
and callable as the 28 built-ins. One binary supports two modes:

| Mode | Started by | Transport |
|------|--------|------|
| stdio (local) | spawned directly by the host (no args when auto-discovered; settings.json `plugins[].command` may carry args like `--headless=false`) | line-delimited JSON-RPC over stdin/stdout |
| HTTP (remote) | run manually/ops-side as `plugin serve --addr ...` | MCP Streamable HTTP (POST a single JSON-RPC) |

General principles:

- **Zero-config onboarding**: a stdio binary dropped into `~/.grf/plugins/`
  (executable, non-hidden, not `.md`, not `.old*` leftovers) is
  auto-discovered; Windows has no exec bit, so admission goes by extension
  (.exe/.bat/.cmd/.com/.ps1). Explicit wiring uses the `plugins` key in
  settings.json (when the key exists only configured entries are used —
  auto-discovery stops).
- **Failures only degrade**: connection failures produce warnings (into the
  system prompt, visible to the model); single-tool failures return error
  content; a plugin crash never affects the host or other plugins.
- **One codebase, both modes**: tool implementations register once and both
  stdio and serve modes work — free with the SDK, mandatory even when
  writing bare.

## 2. Runtime contract (how the host drives plugins)

Full stdio-mode sequence:

```
host spawns the plugin (child env = host env with secret-looking vars stripped)
  → initialize (declares protocol version 2025-06-18)
  ← initialize reply (echoes the negotiated version + serverInfo.name/version)
  → notifications/initialized (notification, no reply)
  → tools/list (client auto-pages)
  ← tool list (host admits each one, see §4)
  ... idle; when the model calls:
  → tools/call {"name":..., "arguments":{...}}
  ← {"content":[...], "isError"?}
  ... host exits:
  closes the plugin's stdin → the plugin should clean up and exit (the host
  waits 3 seconds, then kills)
```

Hard limits and shared facts plugins **MUST** be designed around:

| Item | Value | Source |
|----|----|------|
| Overall connect+handshake+enumeration budget | 15 seconds | `plugin.handshakeTimeout` |
| Per tools/call timeout | 120 seconds by default; settings.json `plugins[].timeout` (seconds) overrides | `plugin.DefaultToolTimeout` |
| Exit grace after stdin closes | 3 seconds | `Close` in `mcp/stdio.go` |
| Connections | process-shared: every gh server session and every weixin user reuses the same connections | `plugin.Shared` |

Consequences:

- **MUST lazily initialize heavy resources**: registration only in main;
  browsers, display caches and other heavy work goes to the first tool call
  (see grf-browser's lazy Chrome) — the handshake has 15 seconds, blocking
  it means connection failure.
- **MUST respect ctx**: the handler's ctx is cancelled after the call
  timeout; every blocking operation (command execution, network, waiting)
  must bind ctx, or the plugin stays occupied past the timeout.
- **MUST be concurrency-safe**: shared connections mean several sessions may
  call the same tool concurrently; the SDK only guarantees registry
  concurrency — the plugin's own shared state (browser instances, screen
  caches) must be locked or serialized.
- **MUST clean up orphan resources**: child processes and browsers started
  by the plugin must clean up on both the stdin-EOF and SIGINT/SIGTERM paths
  (pattern in §7.3 — default signal killing skips defers).

## 3. stdio discipline (the most common pitfalls)

- **Never** write anything but JSON-RPC replies to stdout. The host client
  parses line by line and silently drops unparseable lines — debug output on
  stdout is lost and pollutes protocol debugging.
- **SHOULD** write logs, panic stacks and startup failures to stderr: the
  host keeps a tail-capped 8KB ring buffer and returns it with errors when
  the plugin process dies (`plugin process exited (stderr: ...)`), the only
  post-mortem diagnostics channel.
- stdin EOF is the quit signal: clean up and exit 0; don't linger.

## 4. Tool admission rules (host-side hard checks)

`plugin.Manager` admits every tool returned by tools/list; violations are
skipped with a warning:

1. Names must match `^[a-zA-Z0-9_-]{1,64}$`.
2. No clashes with the 28 built-ins (`tools/spec/tools.json`, the single
   source).
3. Cross-plugin duplicates: first connected wins — connections are
   concurrent and unordered, so this is **undefined behavior**; rely on
   prefixes, never ordering.

Tool names therefore **MUST** carry a namespace prefix (`browser_*`,
`computer_*`, `demo_*`) — the prefix is the plugin's identity. A missing
`inputSchema` is filled with `{"type":"object"}` by the host, but you
**SHOULD** always provide it explicitly.

After admission, the host does three things developers **SHOULD** know:

- Registration goes through `tools.Registry.RegisterCustom` (the same
  channel as embedder custom tools, outside the built-in 28's namespace).
- `\n\n[grf plugin <name>]` is appended to descriptions so the model can
  attribute tools.
- `annotations.readOnlyHint` decides plan-mode behavior: annotated →
  allowed; unannotated → blocked as mutating (`planBlocked` in
  `agent/turn.go`). In approval mode plugin tools are confirmed per call
  like built-ins (`Host.ApproveTool`; on gh server it becomes a
  `tool_approval` pause).

## 5. Tool definition rules

### 5.1 Naming and description

- Names: `prefix_verb[_object]`, lowercase with underscores, matching the
  built-in style.
- Descriptions are written for the **model**, not people: what it does, when
  to use it, key semantics (coordinate systems, return shapes, required
  prior steps). grf-computer's `computer_screenshot` even spells out
  "pixels ÷ scale + screen origin = global coordinates for clicks" — the
  model uses the tool correctly on that text alone.

### 5.2 Parameter schemas

- Standard JSON Schema (object); every parameter carries a `description`.
- Optional parameters need deterministic omission semantics (like "omit =
  main display") — models handle defaults far better than enumeration.
- Use `enum` for enumerations; write defaults and ranges where possible.

### 5.3 ReadOnly annotation

- Pure reads (queries, screenshots, listings) **SHOULD** set
  `ReadOnly: true` — only then can the model use them for research in plan
  mode.
- Anything with side effects (clicks, file writes, remote state changes)
  **MUST NOT** be annotated. A false annotation defeats the plan-mode hard
  block — a security defect.

### 5.4 Return content

`[]Content` supports `Text` and `ImagePNG` (multimodal) only:

- Text **SHOULD** be concise and structured (key-value/list over
  paragraphs); multiple text items are joined with `\n` by the host into the
  tool's text result.
- Screenshot-like results **SHOULD** return combos: `Text` with metadata
  first (coordinate ranges, scale conversion), then `ImagePNG(base64)` —
  see `computer_screenshot`.
- Other content types (audio, resource, …) become `[type omitted]`
  placeholders — **MUST NOT** be used.
- An empty handler return is filled with `(no output)` by the SDK — you
  **SHOULD** always return meaningful text, even just "done, no output".

### 5.5 Error handling

- Tool execution failure: return `error`. The SDK turns it into
  `isError: true` content; the error text goes back to the model, which can
  fix arguments or retry differently.
- **MUST NOT** rely on panic recovery (the SDK converts panics to isError,
  but that's the last line of defense) and **MUST NOT** call `os.Exit` — a
  dead process is a dead plugin, unrecoverable for the session.
- Invalid arguments **SHOULD** return an error naming the field (the model
  will correct and retry); don't fail silently.

## 6. SDK or bare protocol

The SDK lives in `plugins/internal/mcpserver` (`server.go` + `stdio.go` +
`http.go`, pure stdlib, zero deps):

| Scenario | How |
|------|------|
| Official plugins (in this repo) | new directory inside the `plugins/` module importing the SDK directly; add the name to the `PLUGINS` list in `plugins/build.sh` |
| External plugins (any repo) | the SDK is a Go internal package and **cannot** be imported from outside — copy the three files into your project wholesale (zero-dep, compiles standalone), or write the protocol subset bare per §8 |

The `plugins/` module (`git.gorun.fun/share/grf-harness/plugins`) is the
**only** place in the repo allowed third-party deps (currently chromedp);
external plugins have no such constraint but should stay lean too: the value
is the tool surface, not the dependency tree.

## 7. SDK reference

### 7.1 API overview

| API | Purpose |
|-----|------|
| `New(name, version)` | build the server; name/version go into the initialize reply's serverInfo (shown by `gh plugins`) |
| `s.Add(mcpserver.Tool{...})` | register a tool (same-name overwrite; keep names unique yourself) |
| `s.Names()` | registered tool names (sorted) for assembly self-checks |
| `mcpserver.Text(s)` / `ImagePNG(b64)` | build content items |
| `s.ServeStdio(ctx)` | local mode: read stdin, write stdout, nil on EOF |
| `s.Serve(ctx, r, w)` | line loop with injectable reader/writer — protocol tests |
| `s.ServeHTTP(ctx, addr, path, token)` | serve mode: Streamable HTTP endpoint (path defaults to `/mcp`), blocks until error |
| `s.Handler(token)` | expose an `http.Handler` — for httptest |
| `mcpserver.ParseServeArgs(args, defaultAddr)` | parse serve's `--addr` / `--token` / `--path` |
| `s.Dispatch(ctx, raw)` | process one raw JSON-RPC message — custom transports |

Built-in SDK behaviors (no need to write): handler panics converge to
isError; unknown tools return isError with the available tool list; a
missing `arguments` gets an empty object; `ping` returns an empty result;
protocol version negotiation (2024-11-05 / 2025-03-26 / 2025-06-18 echoed;
unknown versions fall back to 2025-06-18).

### 7.2 Complete entry template

Distilled from the grf-browser / grf-computer entry patterns (copy to
start):

```go
// grf-demo: an example plugin (stdio / serve dual mode).
//
// Usage:
//
//	grf-demo                  stdio mode (auto-discovered and spawned by grf-harness)
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

// version is injected at build time via -ldflags "-X main.version=..."
// (build.sh uses git describe / the CI tag); this is what gh plugins shows.
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
	_ = fs.Parse(args) // stdio mode: the host may pass args per settings.json
	if err := newServer().ServeStdio(context.Background()); err != nil {
		fatal(err)
	}
}

func newServer() *mcpserver.Server {
	s := mcpserver.New("grf-demo", version)
	registerTools(s) // registration only, no heavy init (handshake budget is 15s)
	return s
}

func registerTools(s *mcpserver.Server) {
	s.Add(mcpserver.Tool{
		Name:        "demo_hello",
		Description: "Example tool: returns a greeting",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"who": map[string]any{"type": "string", "description": "who to greet"},
			},
			"required": []string{"who"},
		},
		ReadOnly: true, // only for pure reads; never for side effects
		Handler: func(ctx context.Context, args json.RawMessage) ([]mcpserver.Content, error) {
			var a struct {
				Who string `json:"who"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err // bad args: text goes back, the model will fix and retry
			}
			return []mcpserver.Content{mcpserver.Text("hello, " + a.Who)}, nil
		},
	})
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "grf-demo:", err) // errors to stderr (stdio discipline)
	os.Exit(1)
}
```

### 7.3 Signal and cleanup pattern

Plugins that spawn child processes / browsers **MUST** add signal cleanup
(grf-browser's `watchSignals`): SIGINT/SIGTERM kill the process by default
without running defers — that's where orphan Chromes come from.

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

The stdio main path also `defer cleanup()` (stdin EOF runs defers). Plugins
without heavy resources (like grf-computer) may skip it.

## 8. Bare protocol subset reference

Plugins without the SDK **MUST** implement this subset correctly (aligned
with the `mcp` client): `initialize` / `tools/list` / `tools/call` / `ping`;
notifications (no `id`, or `id` null) never get replies; unknown methods
return JSON-RPC error `-32601`.

Reply shapes (jsonrpc/id envelope omitted, result only):

```jsonc
// initialize: if the client's protocolVersion is one of the three known
// versions echo it, otherwise reply with your own (the host only accepts
// known versions — anything else fails the handshake)
{"protocolVersion":"2025-06-18",
 "capabilities":{"tools":{"listChanged":false}},
 "serverInfo":{"name":"grf-demo","version":"0.1.0"}}

// ping: result must be present (empty object)
{}

// tools/list: one full page is fine, no pagination needed (nextCursor optional)
{"tools":[
  {"name":"demo_hello","description":"...",
   "inputSchema":{"type":"object","properties":{...},"required":[...]},
   "annotations":{"readOnlyHint":true}}]}

// tools/call success (image item: data is base64)
{"content":[{"type":"text","text":"..."},
            {"type":"image","mimeType":"image/png","data":"..."}]}
// tools/call tool-level failure: protocol fine, isError marks it, text goes back to the model
{"content":[{"type":"text","text":"error: ..."}],"isError":true}
```

Error semantics: **tool execution failures use `isError`** (protocol-level
success; the error text is usable tool output the model can self-correct
on); JSON-RPC error objects (-32601/-32700 etc.) are reserved for
protocol-level errors (unknown method, malformed message). The host treats
both as tool failures, but isError text is visible to the model.

Transport semantics:

- **stdio**: line-delimited JSON-RPC; exit on stdin EOF; stdout carries
  replies only, logs go to stderr (as §3).
- **Streamable HTTP**: POST a single JSON-RPC to the endpoint; reply 200 +
  `application/json`; notifications get 202 with no body;
  `Authorization: Bearer <token>` auth is **mandatory** (a random token is
  generated and printed when `--token` is omitted); request bodies are
  capped at 8MB; the client accepts SSE-style replies but plain JSON
  suffices; **no CORS grant headers** — these plugins drive browsers and
  computers, and `ACAO:*` would hand the loopback endpoint to any web page.
  The SDK's `ServeHTTP` has all of this built in — copying it is easiest.

## 9. Testing rules

Plugin tests are zero external dependency and zero side effect (no host, no
browser — lazy startup guarantees that). Three layers:

```go
// 1) assembly self-check: all tools registered and names legal (see browser/main_test.go)
s := newServer(...)
for _, n := range s.Names() {
	if !nameRe.MatchString(n) { t.Fatalf("illegal tool name: %q", n) }
}

// 2) handler unit tests: store handlers as package-level func vars before
//    registration and call them directly, bypassing the protocol layer
got, err := demoHello(ctx, json.RawMessage(`{"who":"grf"}`))

// 3) protocol level: inject reader/writer and run the full stdio line loop
//    (see mcpserver/server_test.go), or httptest.NewServer(s.Handler("t"))
//    for HTTP
srvIn, w := io.Pipe() // test writes requests → server reads
r, srvOut := io.Pipe() // server writes replies → test reads
go s.Serve(context.Background(), srvIn, srvOut)
```

`go test ./...` runs inside the plugins/ module independently; host-side
tests are unaffected because plugins idle automatically under
`testing.Testing()`.

## 10. Build and distribution

- Official plugins: add the name to `PLUGINS` in `plugins/build.sh`, build
  with `./plugins/build.sh [install|all|clean]`. Versions are injected via
  `-ldflags "-X main.version=..."` (CI takes the tag, local takes git
  describe, v-prefix stripped).
- Cross-compiled six-platform artifacts land in `plugins/dist/`;
  linux/windows get upx compression (darwin and windows/arm64 skipped — upx
  breaks macOS adhoc signing and doesn't support win64/arm64 PE yet;
  `UPX=0` disables).
- The macOS signed-install pitfall is handled by build.sh (delete the old
  file first, re-sign at the destination); copying binaries manually into
  `~/.grf/plugins/` and running them gets SIGKILL — re-sign with
  `codesign -f -s - <file>`.
- Official releases: CI publishes to the download server on tag as
  `{base}/{platform}/{version}/plugins/grf-xxx` (platform suffix stripped,
  windows keeps .exe); `gh plugins download` probes exactly this layout.
- Third-party distribution: deliver binaries any way you like; users drop
  them into `~/.grf/plugins/` for auto-discovery, or point
  settings.json `plugins[].command` at path and args, or `plugins[].url`
  at a remote serve instance.
- Before releasing, run the `gh plugins` health check: connection status,
  serverInfo version, tool inventory and readOnly annotations.

## 11. Security rules

- Plugin processes run with the **same privileges** as the host and are not
  constrained by the harness sandbox (which covers only the built-in bash
  tool) — develop/install only trusted plugins.
- serve mode enforces authentication (auto-generated and printed when
  `--token` is omitted) and **SHOULD** bind loopback or internal addresses
  only; what the endpoint can drive (browsers, mouse and keyboard) equals
  the operator themselves.
- `ReadOnly` annotations must match actual side effects (§5.3): they gate
  the plan-mode hard block; mislabeling bypasses user-approval semantics.
- Tool descriptions are the contract with the model: **MUST NOT** induce
  the model to do anything the user didn't intend.
- Under multi-tenant deployments (workspace-per-user isolation) plugin tools
  still go through per-call approval — no exemptions.

## 12. Pre-release checklist

- [ ] Tool names carry the plugin prefix, match `[a-zA-Z0-9_-]{1,64}`, and
      don't clash with the 28 built-ins or other plugins
- [ ] Every parameter has a description; optional parameters have
      deterministic omission semantics
- [ ] `ReadOnly` matches actual side effects
- [ ] No non-protocol output on stdout (check all logs/debug prints go to
      stderr)
- [ ] Errors returned as error; no `os.Exit`, no panic reliance
- [ ] Handler blocking operations bind ctx; shared state locked
- [ ] Heavy resources lazy-start; cleanup fires on both stdin EOF and
      signals
- [ ] serve mode tested for real (`--token` auth works, `--path`
      customizable)
- [ ] `go vet` + `go test ./...` pass; assembly self-check covers all tool
      names
- [ ] After `./plugins/build.sh install`, `gh plugins` shows: connected,
      correct version, readOnlyHint as expected
