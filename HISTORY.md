# History — completed efforts

Archive of finished design/plan docs, newest first. Live leftovers from each
effort are tracked in TODO.md, not here.

---

## Bugfix: stale pane rows under framed borders (August 2026)

Symptom: with `pane_borders = "framed"`, typed/deleted characters didn't
appear until something forced a bigger repaint — ~1s lag with the animated
sidebar visible (its 1s status tick marks all content rows dirty), stuck
indefinitely with the sidebar hidden until a Ctrl-C or pane switch. The cursor
still moved, which made it look like a server-side dropped-frame problem.

Root cause (client, `internal/client/compositor.go` `apply()`): pane diffs
marked dirty rows as `pr.Row + localRow` — **content-space** rows — but
`emit()`/`buildRow()` paint **physical** rows. Physical = content +
`contentOffset()` (top status/docks + the framed inset). With `framed` the
offset is 1, so every diff repainted the row *above* the changed one; the
changed row itself only rendered when a layout/status/copy-mode path called
`markAll` or marked full-content spans. `activeCursor()` already applied the
offset for the same rect; the dirty-marking site didn't. Latent since framed
borders landed — invisible with the default `simple` borders (offset 0) and
invisible to the e2e suite, which ran the default config.

Fix: add `contentOffset()` to both dirty marks in the `PaneContent` loop
(changed lines + cursor row).

Debugging notes for next time:

- The server-side rework done while chasing this (per-view coalescing render
  mailbox in `actor.go`, per-attachment ordered outbox in `session.go`) fixed
  a real but *different* bug: destructive `dirtyContent()` diffs dropped on a
  full channel under flood. It could not explain idle-typing latency — a
  drained queue never drops.
- What cracked it was reproducing with the **user's real `client.lua`** in the
  pty harness (`harness.StartWithConfig`), then bisecting config options one
  per run. Only `pane_borders = "framed"` failed; HEAD and older commits
  failed too, proving it predated the uncommitted work.
- "Appears after exactly N seconds / on Ctrl-C" means the bytes are being
  *painted wrong or not at all*, not delayed in a queue — every queue in the
  path was flushed by the 1s status tick, which the dock-hidden case proved
  wasn't the flush that mattered.

---

## Runtime options — live config via Lua re-eval (July 2026)

Was `RUNTIME_OPTIONS.md`. All four steps landed; late-attach inheritance
closed. Still-open leftovers moved to TODO.md.

### Principle

Config is already Lua. "Runtime-settable" = **re-run Lua live**, not enumerate
options in a `set-option` switch. Anything exposed to the Lua surface
(`gtmux.set_option`, `gtmux.bind`, `gtmux.options.*`) becomes live-settable for
free — no per-option code, no per-option wire message.

### Confirmed constraints (read from the code before planning)

- **`LoadClient` returns `ClientConfig` by value** (client.go:130). The
  `set_option` closures capture a heap `cfg` the compositor doesn't share, so
  "keep the VM alive and let closures mutate" changes a dead struct — nothing
  redraws. Materialization must produce a **fresh** config the client swaps in.
- **Binds accumulate** — `binds.Binds[b] = fn` and `binds.ops = append(...)`
  never reset. Whole-file re-eval merges old+new: a bind the new file removed
  stays bound. Re-eval must **reset-then-eval**.
- **`main_pane_*` is read on the session goroutine** (server-side). A client
  `:set main_pane_width` must **forward to the server**, not eval client-local.
  Need an explicit name→owner map.
- **Client concurrency**: `comp` is guarded by `compMu`; `binds` is read
  unguarded by the stdin goroutine (it's immutable today). A live swap must
  serialize with both.
- **Value escaping**: `set-option k v` sugar must pass the value as a **bound
  Lua arg**, never string-interpolate it into a Lua snippet (injection / VM
  crash).

### Mechanism

The client keeps its config VM alive as the authority, with a re-runnable
`rematerialize()`:

1. reset `cfg = DefaultClientConfig()` + fresh bind table,
2. re-run the file / eval the snippet in the VM,
3. read the VM state out into a new `(ClientConfig, ClientBinds)`,
4. swap into `comp.cfg` + `binds` under the right lock, then redraw.

- **`source-file <path>`** → rematerialize from the file (reset-then-eval).
- **`set-option k v`** → sugar over the same path (bound-arg, no interpolation).
- **Server options** (`main_pane_*`, `session_name`) → tiny live map on the
  session; `set-option` routes there by name.
- **Scripted/remote set of client options** (`gtmux run … set-option`) → one
  **generic** server→client "eval this config Lua" message, so scripting reaches
  client options without per-option wire code.

### Sequencing (all done)

1. **Client-side re-runnable materialization + `source-file`** ✅.
   Fresh-VM reload swaps binds + compositor chrome live; `source-file [path]`
   reloads (reset-then-eval), `gtmux.source_file()` bind. Verified: removed bind
   goes dead, no reattach.
2. **`set-option k v` sugar** ✅. Single `applyOption` registry (loader +
   runtime both funnel through it); `set-option`/`set` as an override
   re-derived over the file (last-wins), `source-file` clears overrides.
   Verified live: `:set status_left …`, revert on reload, last-wins.
3. **Server option live store + routing** ✅. Per-session live
   `main_pane_*` (owned by the session goroutine, no lock), `set-option` command
   mutates it, next select-layout uses it (no auto-relayout, like tmux). Client
   routes non-client options to the server via `config.IsClientOption`.
4. **Generic server→client push** ✅. `proto.SetOption` message; the
   server's set-option broadcasts non-server options to every attached client,
   which applies them via the same override path. Client `binds` is now an
   `atomic.Pointer` (lock-free per-key reads, safe swap from either goroutine);
   `cfgMu` guards the override list. Verified: multi-client converges; local
   `:set` + `-race` + full e2e clean.

### Late-attach client-option inheritance (closed)

A client that attaches *after* a runtime client-option `set-option` inherits
it. The registry holds a `clientOpts map[string]string` (guarded by its existing
mutex — the sanctioned shared store); the session's set-option records the latest
value of each client option there and replays them (as `proto.SetOption`) to a
newly-attached client in the attachEvent handler. Server options (`main_pane_*`)
already persisted since the session stores them.

---

## E2E harness build-out (July 2026)

The plan phase of what is now documented in `E2E_HARNESS.md` (current API,
backends, flags live there). Original staging, for the record:

### Work items (all done)

1. `proto.SockPath()` — honor `GTMUX_SOCK` (mirrors tmux `-S`; ~2 lines).
2. `internal/harness/harness.go` — build-once `TestMain`, `Start`, `Client`,
   subprocess + pty spawn, reader goroutine, `t.Cleanup`.
3. `internal/harness/screen.go` — `Screen` over an `emu.Terminal`: `Row`,
   `Status`, `Cell`, `String`, `Has`, `WaitFor`/`WaitForText`/`WaitForStatus`.
4. `internal/harness/input.go` — key/prefix/arrow/mouse byte encoders.
5. `internal/e2e/smoke_test.go` — the 3 first-cut scenarios.

### First cut (thin vertical slice — proved the harness, not the matrix)

1. **Smoke** — attach → `$` → `TypeLine("echo hi")` → wait for `hi`. Exercises
   build → spawn → pty → emu → `WaitFor`.
2. **Window op** — `Prefix("c")` → status shows `2:`.
3. **Multi-client dot-fill** — `NewPeer(190×9)`, peer acts, assert `·` on the
   small client.

The full matrix (copy-mode, pickers, resize, swap/break/mark/join, mouse, CLI,
run-shell auto-clear) was then ported incrementally, plus the tmux backends and
live-resize scenarios described in E2E_HARNESS.md.

---

## Refactor: client owns all input, server exposes actions (July 2026)

Was `REFACTOR.md`. All five stages landed. The client owns the prefix, the
keybind Lua VM, all overlays (copy-mode, prompts, pickers), and mouse→action
resolution; the server's `handleInput` is a one-line pass-through and
`server.lua` is status-only. Three mouse gestures deliberately stayed
server-side (they need live pane state the client doesn't hold) — tracked in
TODO.md as "client-side mouse purity".

### Goal

One fixed action vocabulary between server and client. The client resolves
every keystroke/mouse event into an action and calls it; the server only
exposes actions and holds state (sessions, windows, panes, layout, history).
How the end user triggers an action (which key, which mode, mouse vs. bind)
is entirely a client concern and invisible to the server.

### The split after the refactor

**Server = actions + state**
- Runs PTYs + emu terminals (incl. scrollback history — output lands here,
  it's the data source).
- Exposes named actions. `runCommand` in `session.go` is already this
  surface; both `gtmux run` (CLI) and interactive binds route through it.
- Streams out: layout, live pane content, a **history snapshot on
  pane-acquisition**, and **lines as they scroll off the top**.
- Expands status formats (needs git/path data) and sends the text.
- Holds the paste buffer (set via a `set-paste-buffer` action).

**Client = all input interpretation**
- Tracks the prefix key + pending-prefix state.
- Runs the keybind Lua VM; holds per-mode key tables (root, copy-mode,
  prompt, picker).
- Maintains a per-pane scrollback mirror (seed + stream).
- Runs copy-mode, search, selection, prompts, and pickers **entirely
  locally** over the mirror and the layout it already has.
- Resolves mouse → actions.
- Sends: action calls, plus raw bytes ONLY for genuine pass-through typing
  to the focused pane.

### Stages (all landed; each left a working, testable system)

1. **History mirror (additive, low risk).** Server bundles history on
   pane-acquisition + streams scrolled-off lines. Client builds the per-pane
   mirror.
2. **Copy-mode → client.** `copymode.go` logic moved client-side, running
   over the mirror. Yank = OSC52 to the client's own terminal (clipboard) +
   `set-paste-buffer <text>` action. Bonus: copy-mode became per-client (one
   client scrolls back while another follows live).
3. **Prompts + pickers → client.** Rename / command / search became local
   text entry drawn over the client's status row; pickers render locally
   over server-provided data. Server shed `promptKind` / `chooseKind`.
4. **Root binds + prefix → client.** Client runs the bind VM, tracks prefix,
   resolves → actions. Server's `handleInput` mostly died. Binds moved
   server.lua → client.lua.
5. **Mouse resolution → client.** Client maps coords → actions (focus,
   border-drag resize, status-bar click, copy-mode drag-select).

Order rationale: history mirror first because it's additive and de-risked the
seed+maintain mechanism before copy-mode depended on it. Copy-mode next (the
big win, but isolated). Then prompts/pickers, then the input-ownership stages
that gutted the server's `handleInput`. Mouse last because of the app
mouse-passthrough wrinkle.
