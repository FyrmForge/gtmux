# tmux parity: what's missing

Tracks gtmux features vs tmux. POC scope: not aiming for full tmux, just
"drives like my tmux" plus the scripting surface tools/scripts expect.

## Already implemented

Splits (`|`/`-`), windows (new/next/prev/select-by-index, last-window), sessions
(switch/choose/rename), kill-pane/window, `select-pane` (+ vim-aware nav,
`-l` last-pane), resize (repeat), swap-pane, swap/move-window, zoom,
break-pane, display-panes, mark+join-marked, copy-mode, paste, detach,
send-keys, run-shell, `display-message`, `list-panes`, `capture-pane`,
preset layouts (`select-layout`/`next-layout`/`rotate-window`, configurable
`main_pane_width`/`_height`), `-t` targeting (`%N` pane-id, `:N`
window-index, within one session).

---

## Tier 1: felt daily

### ~~Preset layouts~~ ✅ done
`select-layout` (tiled, even-horizontal, even-vertical, main-vertical,
main-horizontal), `next-layout` (prefix+Space), `rotate-window` (prefix+C-o).
main-* sizing matches tmux, tunable via server.lua `main_pane_width`/
`main_pane_height`. Verified geometry against real tmux at 80×24.

### ~~last-window (prefix+l)~~ ✅ done
`last-window` / `select-window -l` swap to the previously-active window
(`session.lastWindow`, kept valid across window removal). Unbound by default
(config uses `l` for resize); `gtmux.last_window()` available.

### ~~capture-pane~~ ✅ done
`capture-pane [-p] [-t] [-S/-E]` → pane text. `-S -` all scrollback, `-S -N`
last N history lines, `-E N` end row. Trailing-space trim + trailing blank
line match tmux byte-for-byte. Skipped: `-J`/`-e`/named-buffer capture.

### ~~Runtime options + source-file~~ ✅ done
Live `set-option`/`set` + `source-file`, no daemon restart. Since config is
Lua, "runtime-settable" = re-run Lua live: a single `applyOption` registry
(loader + runtime both funnel through it), client options applied via a
fresh-VM reload (reset-then-eval), server options (`main_pane_*`) held live on
the session goroutine. `source-file` reloads the file (clears overrides);
`set-option` layers a live override (last-wins). Scripted/other-client sets of
client options push to attached clients (`proto.SetOption`). Design +
sequencing in `HISTORY.md` (runtime-options section).
- `show-options`/`show`/`showw` print gtmux's server-side options (tmux-hyphenated
  names, `-v` for value-only); `setw`/`set-window-option` alias `set-option`
  (option names accept hyphens or underscores, leading `-g/-w/-s/-a/-t` stripped).
  ponytail: no separate per-window option store: window options fold into the
  session's. A late-attaching client now inherits runtime client-option changes:
  the server records the last value of each client option on the registry and
  replays them on attach (closes the late-attach gap from the runtime-options
  effort, HISTORY.md).

---

## Tier 2: scripting / power use

### ~~Full target syntax~~ ✅ done
`-t [sess:]window.pane`: window by index/name/relative (`+`/`-`/`+N`/`-N`/
`{next}`/`{previous}`/`{last}`/`{start}`/`{end}`), pane by index/`%id`/`.+`/
`.-`, plus the bare `%id` global lookup. Cross-session `sess:…` routes the
whole command to that session's goroutine at the dispatch layer (server.go
`targetSession` + acceptConn): each goroutine still only touches its own
tree, no shared state. Verified: within-session forms, cross-session read
(`display-message -t work:1`) and mutation (`select-pane -t work:1`).
- Cross-session from a client *keybind* now works too: a bind whose command
  carries a `-t sess:...` target is routed to that session on the connection
  goroutine (via `command()`, like scripting) instead of the local session's
  actionEvent. Overlay-opening actions still take the local path (the acting
  epoch must be known for client-targeted replies).

### ~~Buffer stack~~ ✅ done
Session-scoped buffer stack (newest first, `buffer0`/`buffer1`/… + named via
`-b`): `set-buffer [-b name] data`, `paste-buffer [-b name]` (paste also aliases
it), `list-buffers [-F]` (`buffer_name`/`buffer_size`/`buffer_sample` vars),
`show-buffer`, `save-buffer [-b name] path`, `delete-buffer [-b name]`,
`choose-buffer` (server-built picker → paste on select, `gtmux.choose_buffer`).
Copy-mode yank now pushes a buffer. `set-buffer -a` appends to the named (or
newest) buffer. Buffer names with spaces survive choose-buffer (the client
tokenizer is quote-aware; the picker target single-quotes the name). ponytail:
session-scoped, not server-global (tmux's are): keeps the no-mutex session-owner
model; a global store would need a lock.

### ~~list-sessions / list-windows~~ ✅ done
`list-windows [-F]` (one line per window: `window_index`/`window_name`/
`window_active`/`window_panes`/`window_width`/`window_height`), `list-sessions
[-F]` (one line per registry session: `session_name`/`session_windows`/
`session_attached`). list-sessions runs on a session goroutine, so it reads
local state for itself and `info()` for the others (self-`info()` would
deadlock). Skipped: `[WxH]` dims: add when a script needs them. (`session_created`
is now a var, for `#{t:...}`.)

### ~~Format language depth~~ ✅: see FORMATS.md
Stackable `#{...}` modifiers in internal/format: `b:` basename, `d:` dirname,
`=N:`/`=-N:` truncate (first/last N chars). Stacking falls out of recursion
(`#{=10:b:var}`). Position vars `pane_left/top/right/bottom` already added.
- **Intentional divergence from tmux**: shell substitution declares its side.
  tmux's single-process `#()` becomes `#client(cmd)` (run on the attached
  client) vs `#server(cmd)` (run on the server host): the two can be different
  machines. Bare `#()` is ignored (no side declared). Full mechanism + data
  flow in FORMATS.md.
- `#{t:var}` formats a unix-seconds var (`session_created` is the first) with a
  fixed ANSIC layout; truncate now counts runes, not bytes. See FORMATS.md.

---

## Tier 3: deeper tmux

- **copy-mode**: ~~search (`/`,`?`,`n`/`N` directional)~~ ✅, ~~rectangle select
  (`C-v`)~~ ✅, ~~vi/emacs keytables~~ ✅ (`mode_keys` option; emacs C-b/f/p/n,
  C-a/e, C-s/r, C-Space, C-v, `R`, M-f/b/w/</>/v: vi keys stay live too, and
  the table is fixed at copy-mode entry). ~~Wheel-scroll to enter~~ ✅ (with the
  mouse item below).
- ~~**hooks**~~ ✅: `set-hook [-a] [-u] <name> <cmd>` + `gtmux.set_hook` in
  server.lua. Session-scoped (seeded from a global config map, each session gets
  its own copy → runtime set-hook stays session-local, no shared mutable state).
  Wired events: after-new-window, after-split-window, after-select-window,
  after-select-pane, after-rename-window, pane-exited (fired only when the session
  survives the exit), session-renamed, client-attached, client-detached. The
  fire-guard is now per-hook-name (self-recursion suppressed, cross-hook chaining
  allowed). Deferred/noted: the remaining ~30 tmux hook names (add fire points as
  needed); a hook command that's client-side (e.g. command-prompt) still runs
  server-side and no-ops: the one architectural gap.
- ~~**display-popup** / **display-menu**~~ ✅: `display-menu` reuses the picker
  overlay (a "run" verb: each item's target is a command line);
  `gtmux.display_menu(title, name1, cmd1, …)`. `display-popup` is a session-scoped
  floating terminal: server spawns a windowless PTY (`spawnPopup`, its output/
  exit routed before the window-teardown path so a nil `win` never panics),
  broadcasts open/content/close, the client composites it in a centered bordered
  box and grabs all input while open; closes when the command exits.
  `gtmux.display_popup([cmd])`. Per-client now: popups are keyed by the opening
  client's epoch (`popups map[int]*pane`), sent/driven/torn-down per client.
  `-w/-h/-x/-y` honored (N, N%, or C for x/y; box left/top passed to the client).
  Deferred: mouse/resize in popup, stay-open-on-exit, popup styling flags
  (`-b/-s/-S/-T` consumed but not applied). (Harness injects client/server Lua via
  an isolated XDG_CONFIG_HOME, so these client-intercept features are e2e-tested.)
- ~~**flow control**~~ ✅: `command-prompt` (`-p` label, `-I` initial, `%1`/`%%`
  template substitution), `confirm-before` (y/Y runs; Enter and all else cancel),
  `if-shell shell then [else]` (shell runs async off the session goroutine, then/
  else posted back to it). Exposed as `gtmux.command_prompt/confirm_before/
  if_shell` Lua primitives (encoded as Actions; command-prompt/confirm-before
  open client overlays, if-shell runs server-side). Deferred: multi-word command
  parts rely on being passed as single Lua args: no shell-style quote parser, so
  `'%1'`-style quoting and multi-prompt (`-p "a,b"` / `%2`) aren't supported.
- ~~**multi-client**~~ ✅: `list-clients` now spans every session (self read
  locally, peers queried via a `clientsEvent` round-trip: a self-query would
  deadlock), id'd by epoch (gtmux has no client tty). `refresh-client` forces a
  full resync. Size reconciliation is a `window_size` option: `latest` (tmux's
  default), `smallest`, `largest`, and `manual` (grid set by `resize-window`
  `-x/-y` absolute or `-U/-D/-L/-R [N]` nudge, ignoring client sizes).
  ~~`detach-client` / `choose-client`~~ ✅: `detach-client [-t client-<epoch>@
  <session>]` detaches a specific client (routed to its session's goroutine if a
  peer); `choose-client` opens a filterable picker of every connected client whose
  selected row runs that `detach-client`. Both share `gatherClients` with
  list-clients. Test: `TestChooseClient`. Deferred: `-t session` filter on
  list-clients, `refresh-client -S`/`-C` flag depth.
- ~~**mouse**~~ ✅: drag-resize borders, focus-click, app passthrough (were
  done); drag-select-to-copy + wheel-scroll work inside copy-mode; wheel-up over
  a non-tracking pane now enters copy-mode (server-side, only it knows the app's
  mouse mode). Drag-to-*enter* copy-mode also works: a left-drag over a
  non-tracking pane arms on press and, on the first motion, enters copy-mode with
  the selection anchored at the press cell (a `Select` flag on the CopyModeEnter
  snapshot; a per-drag guard stops re-anchoring). Deferred: none.
- **misc** (partial): ~~`has-session`~~ ✅ (CLI `gtmux has-session <name>`, exit
  code contract), ~~`send-prefix`~~ ✅ (client sends the prefix byte to the pane),
  ~~`find-window`~~ ✅ (select first window whose name matches; name-only, not
  pane content). ~~`respawn-pane`/`respawn-window`~~ ✅: in-place process restart
  (same pane id/rect/window, new PTY+grid via `pane.respawn`); a `gen` counter
  tags PTY-reader events so the pre-respawn reader's trailing output/exit is
  dropped instead of tearing down the live pane. ~~`pipe-pane`~~ ✅ (tee a pane's
  output to a command's stdin, self-healing on the command's exit; toggle off
  with no command). ~~`set-environment` + `show-environment`~~ ✅ (session env map,
  shared by reference with each window, injected into panes spawned afterward:
  tmux's future-panes semantics). ~~`clock-mode`~~ ✅ / ~~`lock`~~ ✅ (client
  overlays). Clock is now tmux-style big ASCII digits. Lock takes an optional
  `lock_password` client option (typed + Enter to unlock; empty = any key
  dismisses, as before): compared client-side, no server state. `set-environment
  -g` sets a cross-session global env (stored on the registry, read live at each
  pane spawn; session env overrides it). `update-environment` refreshes tmux's
  default var set (SSH_AUTH_SOCK, DISPLAY, …) from the attaching client's
  environment (sent in Attach) into the session env. Client-option changes are
  recorded on the registry and replayed to a late-attaching client. Cross-session
  keybinds work (a bind whose command carries `-t sess:` routes to that session on
  the connection goroutine). Lua: `gtmux.send_prefix/find_window/respawn_pane/
  respawn_window/clock_mode/lock`. Deferred: real (PAM/system) lock password,
  configurable `update-environment` list, `update-environment` unset (it only
  adds/refreshes vars, never removes one absent from the client's env), mouse/
  resize in popup.

---

# Roadmap: beyond daily-driver parity

Tiers 1–3 are done: gtmux drives like tmux for interactive daily use and the
scripting surface tools expect. What's left is the *depth*: the option/format
DSL, extensibility, multi-session topology, and integration protocol. Ordered by
value for a "drives like my tmux" POC; some items carry an explicit **scope
call** (may stay out of a POC). Work it the way we did the deferred waves: pick
the top unchecked item, agree the approach, implement + test, tick it.

## Tier 4: option & format depth (highest value, in scope)

The biggest felt gap: `.tmux.conf` sets dozens of options gtmux doesn't have
(~22 vs tmux's ~170), and fancy status bars use format operators gtmux lacks.

### 4a: daily-driver options
Wire these to real behavior (not just accept-and-ignore). Grouped by subsystem:
- [x] **Indexing**: `base-index` / `pane-base-index` offset displayed *and*
  targeted window/pane numbers (server options; default 0 = tmux-faithful, shipped
  `default_server.lua` sets 1). Threads through `window_index`/`pane_index` vars,
  the status window list (`WindowInfo.Index`), and target resolution
  (`resolveWindowSpec`/`resolvePaneSpec`). Fixed `select-window` en route: it
  hardcoded 1-based and ignored `-t`; now routes all forms through the base-aware
  resolvers (also fixes choose-window under a non-default base). `renumber-windows`
  is inherently always-on: gtmux windows are a compact slice, no gaps possible;
  the same limitation means `new/move-window -t N` can't place at an arbitrary
  index with gaps.
- [x] **History/timing**: `history-limit` (server, config-time only: write-once
  package var seeded before any pane spawns, so no lock; shipped lua sets 5000,
  code default is tmux's 2000). `repeat-time` (client option, was hardcoded 500ms
  = tmux's default; captured at attach). `display-time` (server, session-local +
  runtime-settable like base-index; shipped lua sets 3000, code default tmux's 750).
  `escape-time` **N/A**: the client forwards ESC raw and the server's `emu` parser
  disambiguates sequences itself; there's no client-side ESC timer to configure, so
  the knob has no home here (implementing it means adding buffering that doesn't
  exist purely to expose an option).
- [x] **Naming/titles**: `automatic-rename` (on) + `automatic-rename-format`
  (`#{pane_command}`) + `allow-rename` (off) are server options; `window.name()`
  became a session `windowName` closure with tmux precedence: manual rename >
  app OSC title (allow-rename, read live from `emu.Title()`) > format expansion
  (refreshes a frozen `autoName`, so auto-rename-off keeps the last value). Auto
  name is built from a pane-only var set to avoid `window_name` recursion.
  `set-titles` (off) + `set-titles-string` (`#{session}:#{window_index}:#{window_name}`)
  are client options: the compositor emits OSC 0/2 to the outer terminal on
  title change, pushing/popping its title stack (`\e[22;2t`/`\e[23;2t`) so detach
  restores. Added `pane_title` (#T) var. All defaults tmux-faithful → no lua.
- [x] **Status styling** (all client options): `status-style` replaced the split
  `status_fg`/`status_bg` with one tmux-style string (`fg=…,bg=…,bold`, partial/
  cumulative parse → fg/bg/attr). `window-status-format` / `-current-format` are
  expanded client-side per entry (default `#{window_index}:#{window_name}#{window_flags}`,
  new `window_flags` var), joined by `window-status-separator`, positioned by
  `status-justify` (left/centre/right). `renderBar` records each entry's click
  span so `resolveMouse` stays correct under any justify/format. `status-position`
  top/bottom moves the bar via a `contentOffset` that shifts window rows, the
  cursor, and the mouse-Y forwarded to the server (which stays position-unaware).
  Defaults preserve the current look (no reskin). Deferred: `pane-border-status` +
  `pane-border-format`: border decoration, not the status bar; its own item.
- [x] **Pane border status**: `pane-border-status` (off/top/bottom) +
  `pane-border-format`. Faithful reserve-a-row approach: `layout.apply` shrinks
  each pane by one row for its label (`pane.borderRow`, window-space), the session
  expands `pane-border-format` per pane into `pane.borderLabel` before each layout
  send, and the client draws the border rule + label on that row. Options are
  server-side; changing them reflows every window. Test: `TestPaneBorderStatus`.
  ponytail: the label refreshes on layout change, not every output, so a
  `#{pane_command}` label can lag until the next relayout.
- [x] **Resize**: `aggressive-resize`: done, once window actors landed
  (Tier 6). The window actor now owns its grid, computed from its viewers'
  size votes; `window-size` (smallest/largest/latest/manual) combines the votes
  and `aggressive-resize` (config `aggressive-resize` + runtime set-option,
  default off) gates whether a viewer where the window isn't current counts.
  Tests: `TestSharedWindowSmallest`, `TestAggressiveResize`. Full design +
  ponytail ceilings in WINDOW_ACTORS.md.
- [x] Real **window option scope**: each `window` carries an `opts` override map;
  `setw`/`set-window-option` (or `set -w`) *without* `-g` writes the `-t` window's
  override (else the active one), while `-g`/plain `set-option` still set the
  session default. `windowName` and `show-window-options` resolve override →
  session-default per window; `show-options` shows the session default. Only the
  naming options (automatic-rename / -format / allow-rename) are window-scoped
  today; the store is generic so future ones slot in. (Fixed en route:
  `show-options -t X` misread the target as the name filter: now strips `-t`
  via `resolveTarget`'s `rest`.) Pane-scoped options (`set -p`) deferred: no
  pane-scoped option exists in gtmux yet.

### 4b: format language DSL
Extend `internal/format` (stays pure, no shell) with tmux's operators:
- [x] Arithmetic `#{e:...}` (`e|N:` precision) and numeric compares `#{==:a,b}` /
  `#{!=:}` / `#{<:}` / `#{>:}` / `#{<=:}` / `#{>=:}`: recursive-descent evaluator
  in `ops.go`. Compares are numeric when both operands parse, else lexical.
- [x] Logical `#{||:}` / `#{&&:}` (truthy = non-empty and not "0") and `#{m:}`
  (glob via `path.Match`) / `#{m/r:}` (regex). Also fixed `#{?cond,…}` to *evaluate*
  a nested-format condition (e.g. `#{?#{==:…},…}`), not just look it up as a var.
- [x] `#{a:N}` (char by code), `#{n:var}` (rune length). Operators reach the
  client status bar too: its expander delegates shell-free `#{}` bodies to the
  pure engine (keeps the shell-aware path for bodies nesting `#client()`/`#server()`).
- [x] Loops `#{S:...}` (sessions) / `#{W:...}` (windows) / `#{P:...}` (panes).
  `format.ExpandLoop(fmt, vars, loop)` threads a `LoopFunc` provider through the
  pure package; each item's vars merge over the outer scope. The server supplies
  `loopVars` (all sessions / this session's windows / the active window's panes)
  and uses `ExpandLoop` in `display-message` and `list-*`. Tests: `TestExpandLoop`
  (unit), `TestFormatLoops` (e2e). ponytail: flat only (a nested `#{W:#{P:}}`
  reuses the active window's panes); client status bar keeps its native window
  list rather than a `#{W:}` loop (server-side is the non-redundant use).
- [x] `#{t:var;spec}` custom strftime layout. Special-cased in `expandBrace` (the
  spec follows the var after `;`, so a `%H:%M` spec's colons don't mis-split);
  `strftimeToGo` maps the common directives (`%Y %m %d %H %M %S %p %b %B %a %A
  %Z %z %%`) to a Go reference layout, unknown ones pass through. Test in
  `TestExpand` (`#{t:ts;%Y-%m-%d %H:%M}` vs the stdlib).

## Tier 5: extensibility & key tables

- [x] **Custom key tables** (`bind -T <table>` / `switch-client -T`): `ClientBinds`
  gained a `Tables` map + `ResolveTable`; Lua `bind_table(table,key,fn)` registers
  keys and `key_table(name)` (a `BindOp{Table}`) switches the client into a table.
  The input machine holds a `curTable`: while set, the next key resolves there and
  reverts to root first (so a bind can chain into another table): tmux's one-shot
  multi-key-sequence behavior. Test: `TestCustomKeyTable`.
- [x] **User options** (`@foo`): `set[-g] @x val` stored in a session `userOpts`
  map, merged into every format var map (status, paneVars, windowVars) so `#{@x}`
  resolves like any variable: works in `display-message`/`list-*` and the client
  status bar (via `Status.Vars`). ponytail: session-scoped, not cross-session -g.
- [x] **`command-alias`**: `set command-alias name=expansion` (or `command-alias[N]`)
  stored in a session `cmdAlias` map; `runCommand` replaces a matching bare command
  with its expansion + the original args before the dispatch switch.
- [x] `send-keys` depth: `-l` (literal, no key-name lookup) and `-H` (hex byte
  values), plus `-t` accepted/ignored. ponytail: `-N` count / `-R` reset deferred.

## Tier 6: multi-session topology

- [x] **Session groups** (`new-session -t <existing>`): sessions sharing the
  same window set. Done via the window-actor refactor — each window is now its
  own owner goroutine; a joining session borrows the target's window actors as
  a snapshot (`groupJoinRequest`), so both sessions drive the same live windows
  with no shared mutable state. Each session keeps its own current-window +
  size. Test: `TestSessionGroup`. Design in WINDOW_ACTORS.md.
- [x] **Linked windows** (`link-window` / `unlink-window`): one window in
  multiple sessions. Done on the same window-actor foundation — a `link-window`
  appends another session's window actor into this session's winlink slice;
  the shared actor sizes itself from all its viewers' votes (see
  aggressive-resize above). Exercised by `TestSharedWindowSmallest` /
  `TestAggressiveResize`.
- [ ] **Custom views** (gtmux-native, past session groups): a synthetic
  session that *gathers specific panes* mirrored from other sessions' windows
  (e.g. "all my Claude panes in one view"), not whole windows like groups/link.
  **Model agreed:** mirror + temporary-own. The pane keeps running in its home
  window; the view also renders it. Whoever is focused wins the size vote, the
  PTY reflows to them, other viewers see that size (dot-fill/crop the slack);
  switch back to the home session and it re-votes and reclaims — revert-on-
  refocus is free. This is exactly the `window-size latest` / aggressive-resize
  vote machinery, applied per-pane instead of per-window, so the hard part
  (votes, reflow-on-focus, notify-other-viewers) already exists and transfers.
  **The fork:** today the owner-goroutine is the *window*; for this the owner
  has to be the *pane* (pane→actor, same shape as the window→actor refactor).
  It would subsume windows/groups/link — all become "a layout referencing
  pane-actors." **Lazy path first:** if the target panes are each *alone in
  their window* (one Claude per window, common), a custom view is already
  achievable as a session assembled from `link-window` of those windows —
  focus-owns-size + revert already work, so build only a thin "gather" command,
  no pane-actor refactor. The refactor is only needed to pull *one* pane out of
  a window that has *several*, leaving its siblings behind. Open question before
  building: real setup = one-pane-per-window or multi-pane windows? Decides
  thin-command vs pane-actor fork.
- [x] `destroy-unattached` / `detach-on-destroy` (session lifecycle, server
  options). destroy-unattached: after a client leaves (detach or disconnect),
  if no attachments remain the session ends (`done = true`). detach-on-destroy
  (default on): off makes a killed session hand its clients to another session
  via `SwitchSession` (the proven switch-session idiom) instead of closing their
  conns. Tests: `TestDestroyUnattached`, `TestDetachOnDestroy`.

## Tier 7: monitoring & alerts

- [x] **Activity/bell detection**: output on a non-current window sets its
  `window.activity` (any output) or `window.bell` (a BEL byte) flag, exposed as
  `#{window_activity_flag}` / `#{window_bell_flag}` and cleared when the window
  becomes current (`switchToWindow`). `monitor-activity` / `monitor-bell` gate it
  (session defaults + per-window `setw` override via the `opts` store); the
  `alert-activity` / `alert-bell` hooks fire. Tests: `TestMonitorActivity`.
- [x] **Silence detection** (`monitor-silence` → `window_silence_flag` +
  `alert-silence`). Each window carries a `silenceTmr`: any output clears the
  silence flag and rearms `time.AfterFunc(interval)`; when it lapses it posts a
  `silenceEvent`, and if the window is still non-current the `~` flag + hook (+
  visual message) fire. Cleared/stopped on becoming current and on teardown.
  `monitor-silence` is an integer (seconds, 0=off), per-window overridable.
  Test: `TestMonitorSilence`.
- [x] Activity/bell flags in the **status-bar window list** + `visual-bell` /
  `visual-activity`. `WindowInfo` carries `Activity`/`Bell`; the client folds them
  into `#{window_flags}` (`#` activity, `!` bell) so the non-current window shows
  the flag in the bar. `visual-activity`/`visual-bell` (server options, default
  off) flash a "activity/bell in window N" status message on the alert. Test:
  `TestMonitorActivity` (extended to assert the bar flag + visual message).

## Tier 8: integration protocol (scope call)

- [ ] **Control mode** (`-C` / `-CC`): the line-based machine protocol iTerm2 and
  tooling drive tmux with (`%output`, `%layout-change`, `%begin/%end`, notifications).
  **Deferred: flagged fork, out of POC scope for now.** A whole second client
  protocol parallel to the render path; no external tool targets gtmux yet, so
  high-effort/low-immediate-payoff. Its own project if/when tooling needs it.
- [x] `wait-for` (`-L`/`-U`/`-S` lock/signal channels for script synchronization).
  A registry-level channel manager (`waitCh` under its own `waitMu`) with two
  modes: signal/wait (`-S` closes all bare waiters' channels) and lock/unlock
  (`-L` acquires or queues; `-U` releases one queued waiter: which inherits the
  lock: or clears it). Intercepted in the connection goroutine (server.go's
  `msg.Command` handler) *before* session routing, so the block never touches a
  session goroutine. Test: `TestWaitFor` (signal/wait + lock/unlock, `-race` clean).
- [x] `capture-pane -e` (keep SGR escapes) / `-J` (join wrapped lines). The SGR
  encoder (`WriteLine`/`RenderLine`) moved from the client into `emu` so the
  screen renderer and capture-pane share one implementation; `-J` rejoins rows
  whose last cell carries the `AttrWrap` flag. Test: `TestCapturePaneDepth`.
- [x] **`allow-passthrough`** (server option, off by default): an app in a pane
  emits `ESC Ptmux;<payload>ESC \` (inner ESCs doubled); gtmux un-doubles and
  forwards the raw payload to the client's real terminal (e.g. OSC 52 clipboard).
  Built as a **byte-level pre-scanner** in front of emu (`passthrough.go`
  `ptScanner`), not emu DCS callbacks — go-vte aborts a DCS on the first payload
  ESC, so the callbacks never see the ESC-laden payload. Always strips the wrapper
  from emu input (an un-stripped payload's inner OSC would execute on gtmux's own
  emu); the forward is double-gated — the actor sets `hostOut` only for a view
  that sees the emitting pane (same per-view visibility as content, so a
  background/zoom-hidden pane is dropped, like tmux), AND `handleRender` rechecks
  the current window (a render queued just before a `select-window` must not land
  on the new window). It goes to writable clients only (`sendPassthrough` skips
  `attach -r` observers), and the per-pane scanner state is reset on respawn (a
  mid-passthrough respawn can't carry a half-parsed wrapper into new output).
  Reusable server→client raw channel (`renderMsg.hostOut` → `ServerMsg.Passthrough`
  → client stdout, bounded by a 1 MiB payload cap). Tests: `passthrough_test.go`
  unit (single-walk terminator, cross-chunk split, non-tmux DCS pass-through,
  runaway-cap bail) + `TestAllowPassthrough`/`-Off`/`-BackgroundDropped`/`-ReadOnly`
  e2e via a raw client-output tap.
- [x] **`extended-keys`** (client option, off by default): while a pane app speaks
  the kitty keyboard protocol, the client negotiates it with its outer terminal so
  disambiguated keys (Ctrl+I vs Tab, Shift+Enter) reach the app. **Divergence:**
  gtmux does kitty-protocol passthrough (the emu only detects kitty `CSI > flags
  u`), keeping the tmux option *name* but not tmux's modifyOtherKeys wire format.
  Mouse-flip propagation pattern: `pane.keyFlags()` → `PaneRect.KeyFlags` → actor
  `keyMode` flip → client `negotiateKitty` pushes/pops the same `CSI > N u` on its
  outer terminal (`extendedkeys.go`). No decode/collapse codec: the client input
  loop already forwards unrecognized bytes, so the outer terminal's CSI-u keystrokes
  pass straight to the app. Ceiling: works under the disambiguate flag (0x1, prefix
  stays legacy); under report-all (0x8) the prefix would arrive as CSI-u and prefix
  detection wouldn't fire. Renegotiation rides every Layout — a runtime enable
  reconciles via the Layout the option change produces. Tests: `TestKittyNegotiate`
  unit + `TestExtendedKeysNegotiate`/`-Off`/`-RuntimeEnable` e2e via the raw tap.

## Cross-cutting

- [x] Hook fire-points for the events gtmux already handles. Added
  `session-created` (session up), `session-closed` (teardown, state still intact),
  `after-kill-pane` / `after-kill-window` (guarded on `!done`), `after-resize-pane`,
  and `client-session-changed` (switch-session): on top of the existing
  after-new/split/select-window, after-select-pane, after-rename-window,
  pane-exited, session-renamed, client-attached/detached, and alert-*. Tests:
  `TestHookSessionCreated`, `TestHookAfterKillWindow`. Still deferred: hooks for
  events that don't exist yet (`window-linked`/`unlinked`: the session-groups
  fork; `pane-mode-changed`/`pane-focus-in/out`: focus-reporting nuance).
- [x] `kill-session` as a session command (the keybind / command-prompt path;
  the CLI kill already existed): flips the same `killed`+`done` flags as
  killEvent, so `detach-on-destroy` applies to both. Test: `TestKillSessionCommand`.
- [x] Hooks that run **client-side commands** (command-prompt / confirm-before /
  display-menu from a hook). `fireHook` now detects a client-owned command
  (`clientSideCmd`) and routes it to the attached clients via a new
  `ServerMsg.ClientAction`, which the client's decode loop dispatches locally
  (opening the overlay) instead of it no-opping server-side. Test:
  `TestHookClientCommand`.
