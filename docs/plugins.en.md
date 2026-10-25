# MCP plugins (plugins/)

[简体中文](plugins.md) | **English**

Enhanced capabilities in grf-harness come from **MCP (Model Context Protocol)
plugins**: independently compiled executables living in `plugins/` (a
separate Go module, **not part of the main build**). The main binary embeds
a zero-dependency MCP client (the `mcp` package + the `plugin` manager) and
registers plugin tools into the tool registry — to the model, plugin tools
are as visible and callable as the 28 built-ins.

One plugin binary supports two execution modes:

| Mode | Startup | Wiring | For |
|------|---------|--------|-----|
| **Local stdio** | spawned directly by grf-harness (no args) | auto-discovery or `plugins[].command` | plugin on the same machine |
| **Remote HTTP** | started manually/ops-side with `plugin serve --addr` | `plugins[].url` (Streamable HTTP) | plugins on servers/containers, sharing stateful instances like browsers across machines |

## Quick start

```sh
gh plugins download        # install plugins from the official download server to ~/.grf/plugins/
gh plugins                 # health check: list plugins, connection status, tool inventory
./gh "open https://example.com with browser_navigate and take a screenshot"
```

Main-binary distributions generally exclude plugins (keeping the single
binary zero-dependency); `gh plugins download` fetches per-platform builds
from the download server (`https://download.gorun.fun/grf-harness/`,
overridable via `GRF_DOWNLOAD_BASE`): the version matching the main binary
by default, otherwise the newest; name filters accepted
(`gh plugins download browser`, the grf- prefix is optional), `-version`
pins a version. Without a name it downloads everything; restart gh to
activate.

Building from source instead:

```sh
./plugins/build.sh install   # build and install to ~/.grf/plugins/
```

`install` copies `grf-browser` and `grf-computer` to `~/.grf/plugins/` —
executables there are **auto-discovered** (each file is treated as one
stdio plugin) with zero configuration. Setting `"plugins": []` in
settings.json explicitly disables auto-discovery.

## Configuration (~/.grf/settings.json)

```jsonc
{
  "plugins": [
    // local mode: explicit executable and args (beyond auto-discovery)
    {"name": "browser", "command": ["/opt/grf/grf-browser", "--headless=false"]},
    // remote mode: MCP Streamable HTTP endpoint (start serve on the remote first)
    {"name": "remote-browser", "url": "http://10.0.0.1:7610/mcp", "token": "shared-secret"}
  ]
}
```

Fields: `command` (local) or `url` (remote) — pick one; `name` defaults to
the executable name / URL host; `timeout` is the per-call timeout in seconds
(default 120).

Precedence: if the `plugins` key **exists** (even `[]`) only configured
entries are used; if the key is **absent** (nil), configured entries plus
auto-discovery of `~/.grf/plugins` apply. A project-level `.grf/plugins/`
is **never** executed (so repo-shipped binaries can't run unconfirmed).

## Built-in plugins

### grf-browser (browser control, 21 tools)

Implemented with chromedp (a pure-Go CDP client), with the tool surface
aligned to Microsoft's playwright-mcp workflow: **take a
`browser_snapshot` first to obtain element references (refs), then act on
refs**. Snapshots and targeting penetrate shadow DOM / same-origin iframes;
clicks are real CDP mouse events (isTrusted; iframe offsets auto-accumulated).
The browser starts lazily: the first tool call launches Chrome (headless by
default; `--headless=false` shows UI; `--chrome` picks the browser path).

- Navigation: `browser_navigate` / `browser_back` / `browser_forward` /
  `browser_reload`
- Snapshot & sensing: `browser_snapshot` (outline + text blocks +
  [ref=eN]), `browser_get_text` (visible page text), `browser_wait_for`
  (wait for text / URL substring), `browser_console_messages` (console and
  exceptions, including auto-answered [dialog] and [download] events),
  `browser_network_requests` (resource type/duration/size/Content-Type)
- Interaction: `browser_click` (ref/selector, real mouse events, double
  click supported), `browser_type` (fill + optional submit-on-enter),
  `browser_press_key` (enter/ctrl+c/cmd+t etc.), `browser_hover` (out then
  in), `browser_select_option`, `browser_scroll` (directional or scroll a
  ref to viewport center to trigger lazy content)
- Files & state: `browser_file_upload` (set files on input[type=file]),
  `browser_cookies` (list/set/delete)
- Others: `browser_evaluate` (arbitrary JS), `browser_take_screenshot`
  (viewport/full page, returned as multimodal image or saved to file),
  `browser_tabs` (list/new/select/close), `browser_close`

Native dialogs (alert/confirm/prompt) are auto-dismissed and logged to the
console stream so later commands don't hang; downloads land in the plugin
working directory `downloads/`.

### grf-computer (computer control, 14 tools, element-first + coordinate fallback)

Designed after zcode-cua: **the Accessibility API is the primary path** —
the model first reads the UI element tree (each element with index/role/
name/value/coordinates), then acts precisely with `state_id+index` (no
coordinate math, IME or foreground-window dependency); screenshot-coordinate
clicks are the fallback. When an element signature mismatches, the action is
rejected with a prompt to re-observe — an observe → act → re-observe loop.
The plugin itself is zero-cgo and zero third-party deps; coordinates
(coordinate mode) are always global virtual-desktop coordinates (origin at
the main screen's top-left; secondary screens may go negative):

- Observation: `computer_get_app_state` (the core: element tree +
  `state_id`; omit app = frontmost application), `computer_list_apps`,
  `computer_screenshot` (whole `display` or a `window_id`, returned as
  multimodal PNG), `computer_screen_size`, `computer_list_displays`
- Actions (dual mode: `state_id+index` element / `x,y` coordinates):
  `computer_click` (element via AXPress; falls back to the element's center
  coordinates when no press action or for right-click/double-click),
  `computer_type` (element mode sets the value directly; coordinate mode
  types into focus, with CJK input routed through the clipboard),
  `computer_set_value` (text fields/checkboxes/sliders), `computer_scroll`
- Others: `computer_key`, `computer_drag`, `computer_activate_app`
  (launch/focus an app), `computer_clipboard` (get/set), `computer_wait`

Platform implementations:

| Platform | Element tree / actions | Dependencies | Authorization |
|------|-------------|------|------|
| macOS | System Events (AppleScript): `click UI element` already carries AXPress-first semantics; window screenshots capture the window-bounds region | built-in osascript / screencapture / pbcopy | terminal app needs Accessibility permission (screen capture additionally needs Screen Recording) |
| Windows | UI Automation (PowerShell calling the stock .NET patterns: Invoke/Toggle/Value etc.) | built-in PowerShell | none |
| Linux | AT-SPI2 (best-effort via python3-gobject; a clear hint suggests screenshot+coordinate mode when missing) | xdotool; wmctrl (app/window enumeration); any of scrot / gnome-screenshot / import / grim for capture; xclip/xsel for clipboard | X11 (Wayland via XWayland) |

Local manual smoke test: `cd plugins && go test ./computer/ -tags smoke -v`
(real osascript calls verifying the element tree / value setting / targeting
/ window screenshots end to end).

## Writing a new plugin

The full development spec (runtime contract, tool admission, SDK API, bare
protocol subset, testing, build & distribution, security & checklist) is in
**[docs/plugin-dev.md](plugin-dev.en.md)**. Essentials:

- `plugins/` is a separate Go module
  (`git.gorun.fun/share/grf-harness/plugins`) bundling the
  `internal/mcpserver` SDK — register tool implementations once and both
  stdio / HTTP modes work automatically (the SDK is internal; external
  repos copy the three files or follow the spec to write the protocol bare).
- **Tool names carry a namespace prefix** (e.g. `demo_*`), limited to
  `[a-zA-Z0-9_-]{1,64}`; clashes with the 28 built-ins are skipped (with a
  warning); first-connected wins among plugins with duplicate names.
- Do not set `ReadOnly` on side-effecting tools — the harness uses it to
  block them in plan mode.
- Tool failures return errors (the SDK converts them to `isError` content);
  never panic and never exit the process — a dead process means a dead
  plugin (reported for the session; the main binary is unaffected).
- Return images with `mcpserver.ImagePNG(base64)`; the harness passes them
  back to the model as multimodal content.

## Protocol subset

Both the client and the plugin SDK implement only the MCP subset the
harness needs (protocol version 2025-06-18, compatible with 2024-11-05 /
2025-03-26 clients):

- `initialize` handshake + `notifications/initialized`
- `tools/list` (auto-paging), `tools/call`, `ping`
- notifications and server-initiated requests (sampling, roots, etc.) are
  ignored

Transports:

- **stdio**: line-delimited JSON-RPC (MCP convention); plugin stderr goes
  into a ring buffer and is returned with errors on connection failure for
  diagnostics.
- **Streamable HTTP**: POST a single JSON-RPC to the endpoint; the reply is
  application/json (the client also accepts SSE-style replies);
  `Authorization: Bearer <token>` auth; notifications get 202 with no body.

## Security model

- Plugin tools go through the same **approval mode** (per-tool confirmation
  before execution) and **plan mode** (plugins not marked read-only are
  blocked, via `planBlocked` in `agent/turn.go`) as built-ins.
- Plugin processes run with the **same privileges** as the main binary
  (filesystem/network are not constrained by the harness sandbox — that
  covers only the built-in bash tool). Install only trusted plugins; plugin
  child environments have secret-looking variables stripped
  (`mcp/stdio.go`, same rule as `tools/env.go`).
- **Remote mode enforces authentication**: the bearer token for
  `serve --addr` is mandatory — when `--token` is omitted a random token is
  generated and printed to the startup log (configure it as the harness
  side's `plugins[].token`); the server **sends no CORS grant headers** —
  these plugins drive browsers/computers, and `ACAO:*` would let any web
  page drive them by fetching the loopback port. Still prefer
  loopback/internal addresses plus your own firewalling.
- Plugin connections are **process-shared**: multiple sessions in one
  process (per-session harnesses of gh server, per-user agents of weixin)
  reuse the same connections; stateful contexts like browsers survive
  across sessions; under `go test` plugins idle automatically
  (`testing.Testing()`), so tests have no side effects.

## Build & release

```sh
./plugins/build.sh            # current system → plugins/bin/
./plugins/build.sh install    # build and install to ~/.grf/plugins/
./plugins/build.sh all        # cross-compile 6 platforms → plugins/dist/ (for CI release)
```

`plugins/` has its own go.mod — the **only** place in the repo allowing
third-party dependencies (currently just chromedp and its transitive deps);
the main module stays pure stdlib, and the main `build.sh` does not
cross-compile plugins (it only opportunistically `install`s the current
system on the default path). Plugin versions are injected via ldflags
(from git describe); `gh plugins` shows each plugin's self-reported
version.

CI (tag-triggered) builds plugins alongside the main binary and publishes
them per version to the download server:

```
{base}/{platform}/{version}/gh(.exe)                # main binary
{base}/{platform}/{version}/plugins/grf-browser …   # plugins (platform suffix stripped; windows keeps .exe)
```

`build.sh all` applies **upx compression** to linux/windows artifacts
(chromedp is large — 60%+ reduction; darwin and windows/arm64 are skipped —
upx breaks macOS adhoc signing and doesn't support win64/arm64 PE yet;
`UPX=0` disables). `gh plugins download` probes exactly this layout
(detection in `plugin/download.go`; directory listing and replace-on-install
reuse the selfupdate implementation, same base URL
`https://download.gorun.fun/grf-harness/`).
