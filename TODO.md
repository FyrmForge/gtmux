# TODO

Two tracks: **A** closes remaining gaps vs real tmux (extends PARITY.md);
**B** is gtmux's own features on top. Work like the PARITY waves: pick an
item, agree approach, implement + test, tick it.

## A: tmux parity

### Commands

- [x] **Accurate active-border highlight**: fixed: border color is now
  decided per cell, not per segment. Client `buildRow` (internal/client/
  compositor.go) colors each divider cell via `onPaneRing` (the pane's
  one-cell outset outline, corners included) against the active/marked rects
  already in `layout.Panes`, so a full-length divider only lights along the
  active pane's own edge. Server-side `activeBorder` + the `ActiveAdjacent`
  proto field are deleted (client computes it). Marked-border shares the same
  per-cell helper (had the identical whole-segment bug). Tests:
  `TestCompositorActiveBorderPerCell`, existing `TestCompositorMarkedBorder`.
- [x] `clear-history [-t]`: wipes the target pane's scrollback, keeping the
  visible screen. Added `ClearHistory()` to the emu `Terminal` (locks like
  `Resize`, drops `history`/`altHistory` + offsets); dispatch resolves the pane
  via `resolveTarget` and calls it — the emu's own lock makes it safe alongside
  the PTY reader, no actor handshake. Tests: `emu.TestClearHistory` (primitive:
  history emptied, screen unchanged), `TestClearHistory` (e2e via
  `capture-pane -S -`).
- [x] `switch-client -t/-l/-n/-p`: retarget the client's session. `-n`/`-p`
  walk the session list (`adjacentSession`), `-t` jumps by name, `-l` returns to
  the last *switched* session (registry `lastSession`, recorded by
  `switchToSession` — a fresh `gtmux new` attach doesn't count, matching tmux).
  Test: `TestSwitchClient`. Verified live: `-n` and `-l` both flip the attached
  client's status bar. Skipped: `-r` (read-only toggle) — ties to `attach -r`.
- [x] Real `join-pane -h/-v -s/-t` / `move-pane`. `joinPaneOp` (session.go) is
  the move primitive — remove pane from its window's tree, relay-handoff its
  output, drop the source window via the canonical `removeWindowAt` if it was
  the last pane, then `joinPaneAt(p, at, dir)` into the target. `join-pane`/
  `move-pane` parse `-s`/`-t` (via the extracted `resolvePaneStr`, shared with
  `resolveTarget`) and `-h`/`-v`; the mark(M)+join(g) keybind now delegates to
  the same primitive. Test: `TestJoinPane`. Within-session only — a cross-session
  `-s` resolves to nothing and no-ops (needs the pane→actor fork; note in
  PARITY custom-views). Skipped: choose-window `%%` picker templates (the old
  approximation's UI) — `-s`/`-t` supersede it; add if a picker-driven join is
  still wanted. `-b`/`-d` accepted, not modeled.
- [x] `choose-tree`: session→window tree picker with type-to-filter and
  window-granular switching. Server-originated (only the server sees every
  session): `chooseTree` (session.go) flattens sessions into header rows +
  indented window rows — other sessions' window names come from a cross-session
  `other.command("list-windows -F …")` (their own goroutine, no self-deadlock),
  ours from local state. Window rows target `switch-session '<name>' <idx>`
  (other sessions) or `select-window -t <idx>` (self); the extended
  `switchToSession(name, winIdx)` routes a `select-window` to the target session
  *before* the handoff so the reattaching client lands on the chosen window.
  Filter: `OpenPicker.Filter` flips the client picker (prompt.go) to
  type-to-filter — printable keys narrow live, arrows navigate, backspace edits;
  the plain j/k pickers are untouched. Rebound prefix+w → choose-tree (tmux
  default), moved choose-window to prefix+W. Tests: `TestChooseTree` (cross-
  session filter + window switch), updated `TestChooseWindowDefault`/
  `TestDefaultBindsResolve`. Skipped: `choose-client` — tmux lists server-wide
  *named* clients with detach-client targets, but gtmux clients are anonymous
  epochs with no persistent identity; needs a client-naming concept first.
  ponytail: the picker doesn't scroll, so a tree taller than the screen clips
  (pre-existing picker limitation); collapse/expand and sort not modeled.
- [x] `choose-client`: picker over connected clients (target = detach-client).
  Built: `gatherClients` (session.go) extracted and shared by `list-clients` +
  `choose-client`; `detachEpoch(ep)` extracted from `detach`; new commands
  `detach-client [-t client-<epoch>@<session>]` (self → detachEpoch; peer →
  peer.command) and `choose-client` (server-originated OpenPicker, no default
  keybind — tmux has none). Test: `TestChooseClient` e2e (detach a specific
  client by size-identified epoch, assert it leaves list-clients and the acting
  client survives; discrimination = wrong-epoch target → target still listed).
- [x] `new-window`/`split-window <command>` and `-c <start-dir>` (Axis A of
  the keystone). `parseSpawn` (pane.go) pulls `-c <dir>` / `-h` / `-v` out of
  the args; the leftover tokens joined are the command, threaded
  `createWindow`/`splitPane` → `newWindow`/`w.split` → `spawnPane(win,r,dir,cmd)`,
  which runs `shell -c cmd` (same as spawnPopup/respawn): pane exits when the
  command does, no remain-on-exit. `-c` defaults to the active pane's cwd.
  Tests: `TestNewWindowCommandAndCwd`, `TestSplitWindowCommandAndCwd`.
  ponytail: trailing tokens are space-joined, so shell quoting inside the
  command is lost; fine for `new-window npm run dev`. `-d` accepted/ignored.
  `new-window -n <name>` names the window at creation (`parseSpawn` returns it,
  `createWindow` calls `win.rename`); split-window ignores `-n`. Test:
  `TestNewWindowName`.
- [x] **Detached create** (`gtmux new -d`, tmux's new-session -d): create a
  session without attaching, so it can be built via `run` and attached later.
  One-shot `proto.NewSessionRequest` → server `reg.resolveGroup(create)` (no
  attach), client `NewDetached` (ensureServer + one-shot). Cwd seeds the first
  pane (via the caller's working dir). Unblocks a tmux-style build-then-attach
  driver (e.g. a workspacer gtmux backend). Test: `TestNewDetached`.
- [x] **`resize-pane -x <N|N%>` / `-y <N|N%>`** (+ `resizep` alias): absolute
  pane width/height, not just directional nudges. `window.resizePaneTo` sets the
  nearest ancestor split's `frac` so the active pane's side hits the target
  (reusing `popupDim` for the N/N% parse); exact for a simple split. `-t` ignored
  (select-pane first). Test: `TestResizePaneAbsolute` (absolute + percent).
- [x] Multi-arg `command-prompt` (`-p "a,b"`, `%2`/`%3`): Axis B of the
  keystone. The `prompt` overlay (client/prompt.go) went multi-stage: `-p` is
  comma-split into `labels`, `openFlowPrompt` seeds them; on Enter `advance`
  records the answer and, if more labels remain, clears the buffer and keeps the
  overlay open (label steps to the next), else commits. `substituteAnswers`
  expands `%1..%9` (highest-first so `%1` can't clobber `%1x`) and `%%`=first
  answer into the template. Tests: `TestPromptMultiStage` (unit),
  `TestCommandPromptMultiPrompt` (e2e). With Axis A this fully clears the
  Workspacer prompt-bind dependency.
- [x] `previous-layout`: cycles the preset layouts backward. `nextLayout`
  became `cycleLayout(step, ...)` (window.go) — `((i+step)%n+n)%n` on a match,
  else 0; `next-layout` passes +1, `previous-layout` -1. Test:
  `TestPreviousLayout`. Verified live: next→next→previous restores the prior
  geometry.
- [x] `load-buffer [-b name] path`: reads a file into a buffer (named or new)
  via the existing `addBuffer` — inverse of `save-buffer`. Test: `TestLoadBuffer`.
  Skipped: `-` (stdin) path, no stdin in the command context.
- [x] `show-messages` / `showmsgs` / `server-messages`: prints the per-session
  message log, newest first. Every transient message already funnels through
  `showMessage` (session.go) — the single choke point — so logging there catches
  run-shell output, command errors, and activity/bell alerts with one append.
  Ring is capped at `message-limit` (new config option, tmux default 1000,
  seeded like `display-time`: config parse + registry field + runtime
  `set-option`/`show-options`). Each line is `HH:MM:SS <text>`. Test:
  `TestShowMessages` (run-shell → WaitForStatus → show-messages contains it).
  Skipped: `-J`/`-T` (job/terminal listings) — introspection nothing's config
  touches. ponytail: log is per-session (tmux's is per-client); fine since
  gtmux messages are session-owned.
- [x] Runtime `bind-key` / `unbind-key` / `list-keys`. Binds live client-side
  in a Lua VM; runtime changes go through mutex-guarded override maps
  (`oBinds`/`oRoot`) in `ClientBinds` that shadow the Lua binds (nil-present =
  unbound). Server records each bind in the registry (`runtimeBinds`) for
  list-keys and broadcasts the change via the existing `ClientAction` push; the
  client's `applyBindKey` installs it. Test: `TestRuntimeBindKey`. Skipped/
  ceilinged: `-r` (repeat) and `-T` (custom tables) consumed but config-only
  (would need the `Repeat`-map race handled); runtime binds don't survive a
  `source-file` reload (fresh VM); `list-keys` shows only runtime binds — config
  Lua binds are opaque closures; bind applies to the current session's clients.
- [x] `attach -r`: read-only client. `proto.Attach.ReadOnly` → the server
  attachment's `readOnly` flag → the `clientInput` case (session.go) drops the
  input when set. Tmux's model: only raw pane keystrokes are blocked; prefix
  binds still work (they arrive as `Action`s, a separate path), so a read-only
  client can still detach/navigate. The flag rides every Attach the client
  sends, so it persists across `switch-client` reconnects. `gtmux attach -r`
  parses the flag; `client.Attach` threads it. Test: `TestAttachReadOnly`
  (RO peer's input never reaches the pane, RW peer's does; verified to fail
  without the guard). Skipped: `switch-client -r`/`-R` runtime toggle — would
  need a control message to flip the live attachment; add if wanted.

### Behavior / options

- [x] `synchronize-panes`: mirrors client input to every pane in the window.
  Session-wide default (config `synchronize-panes`, off) + per-window `setw`
  override via the `w.opts` store, resolved in `handleInput` (session.go): when
  on, snapshot `wa.panes` under the actor handshake (a concurrent split could
  grow it), then write the keystrokes to each pane's pty outside the handshake.
  Runtime `set-option`/`setw` + `show-options` wired like `monitor-activity`.
  Test: `TestSynchronizePanes` (e2e: split, enable, capture both panes).
- [x] Copy-mode `copy-pipe` via the `copy-command` option: a copy-mode yank
  pipes the selection (stdin) to `copy-command` server-side (run-shell path),
  on top of the existing OSC 52 clipboard + paste-buffer set. Config default +
  runtime `set-option copy-command` + `show-options`, plumbed like
  `remain-on-exit`; `SetPasteBuffer.Pipe` flags the yank. Test: `TestCopyCommand`
  (e2e: `copy-command = cat > file`, yank, assert file gets the selection).
  Skipped: full per-key rebindable `copy-mode-vi`/`-emacs` tables + server
  `send-keys -X` routing — the keytable stays hardcoded. OSC 52 already sets the
  clipboard, so the only real gap was arbitrary-command piping, which this
  covers. Add the rebindable tables if anyone actually needs to remap movement.
- [x] Copy-mode `f/F/t/T` char-motions + numeric count prefix (client): `f`/`t`
  jump forward onto/just-before a char on the current line, `F`/`T` backward;
  digits build a count (`3j`, `2fc`) that carries across the find operator to its
  target, and count-`G` goes to that line (lone `0` stays line-start). New
  `pending`/`count` fields + `charMotion`/`takeCount` in `copymode.go`. Test:
  `TestCopyModeCharMotionAndCount` (drives the real `feed` byte path). Live tmux
  verification caught a panic: `charMotion` searched the `lineRunes` (trailing-
  blank-trimmed) view while `$`/`clamp` leave `cx` in the raw padded line, so a
  backward find with `cx` past the trimmed length indexed out of range and
  crashed the client — fixed by searching the raw padded glyph line (cursor's
  own coordinate space); regression case in the same test. Skipped: `;`/`,`
  repeat-last-find (add when wanted); in emacs mode plain digits also build a
  count (vi-oriented, shadows no emacs binding).
- [x] `remain-on-exit` (`off`/`on`/`failed`): a window option (config default +
  per-window `setw`, mirroring synchronize-panes) resolved at pane-exit time in
  the `ptyOutput` error handler. When it keeps the pane, `pane.markDead` reaps
  the process (no kill), closes the pty, and sets `dead` — so `closePane`/
  `destroyWindow` are skipped and the pane stays frozen with its final screen
  plus a `[pane dead: exit N]` marker (written on the actor). `failed` keeps it
  only on a non-zero exit (the reaped code). `Close` early-returns on a dead
  pane; `respawn` clears `dead`, so respawn-pane revives it (fresh pty/grid,
  relay left intact). Tests: `TestRemainOnExit` (on → frozen → respawn revives),
  `TestRemainOnExitFailed` (non-zero exit kept with code); both verified to fail
  without the keep branch. ponytail: the dead marker is a plain appended line,
  not tmux's styled "Pane is dead" overlay; input to a dead pane silently
  no-ops (pty closed).
- [x] Configurable `update-environment` list + unset semantics. Was a hardcoded
  `updateEnvNames` slice; now a whitespace-separated server option (config
  default = tmux's built-in list, `DefaultUpdateEnvironment`) plumbed like the
  others (registry → session var → `set-option`/`show-options`). Attach-time
  refresh now also *unsets* listed vars absent from the client (tmux behavior),
  not just sets present ones. Test: `TestUpdateEnvironment` (e2e: set the list
  to a custom var, attach a peer carrying it, assert a window spawned after the
  refresh inherits it — the var lives only in the peer's env, not the server's).
- [x] `focus-events` (default off): server option; on active-pane change
  (select-pane + window switch, funnelled through `switchToWindow`/`refocus`)
  a pane that requested focus reporting (DECSET 1004, queried via
  `pane.term.Mode()&emu.ModeFocus`) gets ESC[O then ESC[I. Plumbed like the
  other options. Test: `TestFocusEvents` (e2e: pane captures raw input to a
  file, switch away+back, assert ESC[I/ESC[O delivered).
- [x] `allow-passthrough` — an app inside a pane wraps an outer-terminal escape
  (OSC 52, sixel, …) in `ESC Ptmux;…ESC \` (inner ESCs doubled); gtmux strips it,
  un-doubles, and forwards the raw payload to the client terminal. Built with a
  **byte-level pre-scanner** (`passthrough.go` `ptScanner`), NOT emu DCS callbacks:
  the probe proved go-vte aborts a DCS on the first payload ESC (and passthrough
  payloads are ESC-laden), so the callbacks never see the payload. The scanner
  runs in `applyOutput` in front of emu (per-pane residual for cross-PTY-read
  splits), always stripping (an un-stripped payload's inner OSC would execute on
  gtmux's own emu). Reusable server→client raw channel: option (server config +
  set/show-option) → `renderMsg.hostOut` → session forwards `proto.ServerMsg{
  Passthrough}` → client `os.Stdout.Write`. Off by default. Forward is
  double-gated: the actor sets `hostOut` only for a view that sees the emitting
  pane (per-view visibility incl. zoom), AND `handleRender` rechecks the current
  window (a render queued just before a `select-window`). Writable clients only
  (`sendPassthrough` skips `attach -r`); scanner state reset on respawn; open-state
  buffer capped at 1 MiB (runaway/unterminated wrapper bails). Tests:
  `passthrough_test.go` unit (single-walk terminator vs naive-Index early-terminate,
  cross-chunk split, non-tmux DCS, runaway-cap bail) + `TestAllowPassthrough`/`-Off`/
  `-BackgroundDropped`/`-ReadOnly` e2e via a raw client-output tap (grid-bypassing);
  discrimination = neuter un-double, forward gate, visibility gate, read-only skip
  (all fail). App-emitted OSC 52 clipboard can later ride this same channel.
- [x] `extended-keys` — apps in a pane get disambiguated keys (Ctrl+I vs Tab,
  Shift+Enter). **Chosen shape: kitty-protocol passthrough, keeping the tmux
  option name `extended-keys`** (NOT tmux's actual modifyOtherKeys/fixterms wire
  format — the emu only detects kitty `CSI > flags u`, so faithful modifyOtherKeys
  would need new emu detection; deliberate divergence, user-approved). Built on
  the mouse-flip propagation pattern: `pane.keyFlags()` → `PaneRect.KeyFlags` →
  actor `keyMode` flip resends Layout → client `negotiateKitty` pushes/pops the
  same `CSI > N u` on its outer terminal (client option `extended-keys`, off by
  default; `extendedkeys.go`). No decode/collapse codec: the client input loop
  already forwards unrecognized bytes verbatim, so the outer terminal's CSI-u
  keystrokes pass straight to the app. **Ceiling:** works when the app requests
  the disambiguate flag (0x1) — the prefix stays a legacy byte. Under report-all
  (0x8) the prefix itself would arrive as CSI-u and the client's prefix detection
  wouldn't fire (`// ponytail` in extendedkeys.go). Renegotiation rides every
  Layout (a runtime enable reconciles via the Layout the option change produces —
  no separate reload hook needed, `// ponytail` in `reload`). Tests:
  `TestKittyNegotiate` unit (push/pop state machine incl.
  option-off) + `TestExtendedKeysNegotiate`/`-Off`/`-RuntimeEnable` e2e via the raw
  client-output tap (app enables kitty → client emits `CSI>1u` to outer terminal);
  discrimination = neuter `keyFlags` propagation / Layout-path negotiate (e2e fails).
  Input-decode e2e isn't possible (harness pty can't inject CSI-u keystrokes) — but
  we do opaque forward, so there's no decode logic to test.
- [x] **Client-side mouse purity**: all mouse gesture recognition is now
  client-side. `PaneRect.WantsMouse` (emu ModeMouseMask) rides the Layout and
  is re-pushed when a pane's app toggles mouse tracking (actor detects the flip
  around `applyOutput`, `handleRender` resends Layout). The client's
  `mouseAction` (compositor.go) recognizes: status-label click, **border-drag**
  (→ new `ResizeBorder{Index,Pos}` msg; server maps the border index back to its
  split node and sets `frac`), **focus-click** (→ `select-pane -t %id`, which
  routes through the server's existing `refocus` so focus-events fire on click —
  the NOTE below is discharged), **wheel→copy-mode** (→ select-pane + copy-mode),
  and **drag-to-copy** (→ new `CopyDrag{PaneID,Row,Col}` msg; only the server
  holds scrollback, so it builds the snapshot and replies CopyModeEnter). Server
  `handleMouseEvent` shrank to app-forward-only for tracking panes. Tests:
  `TestMouseBorderDrag`, `TestMouseDragCopy` (e2e, discrimination-checked), plus
  the existing `TestMouseFocus`/`TestMouseSelectWindow`/`TestMouseWheelEntersCopyMode`
  as regression guards. ponytail: the client's WantsMouse view can lag a
  mode-flip by one Layout push; a mis-routed event self-heals on the next push
  (server re-checks live emu mode before forwarding).
- [x] Option-scope leftovers — the real gap: `set-option -u` (unset). Was
  stripped and silently ignored; now removes a map-backed override (`@foo` user
  option, or a per-window `setw -u` override) so it falls back to the default.
  Test: `TestSetOptionUnset` (e2e: set `@greet`, read via display-message,
  unset, reads empty). Deferred/skipped as dead-option-risk or already-covered:
  `set -p` pane-scoped options — no gtmux option resolves at pane scope, so a
  pane store would be a lying no-op (would need per-pane `@foo` format plumbing
  for real value); `show-options -A` — gtmux's show-options already prints
  effective values (no set-here/inherited distinction to expose) and client
  options are deliberately not server-visible; session-scalar `-u` reset-to-
  config-default (each option is a plain Go var — add per-option if it matters).
- [x] `exit-empty` (tmux default **on**, flipped): the daemon exits once its
  last session closes, via `registry.remove` (mirrors kill-server cleanup).
  Server-global; runtime-settable (`reg.setExitEmpty`). Test: `TestExitEmpty`
  (e2e: kill the only session, `list` can no longer reach the daemon).
- [x] `default-shell` / `default-command` (tmux spawn path): config-time package
  vars (like history-limit), used by every `spawnPane`/respawn. default-shell
  overrides $SHELL→/bin/sh; default-command runs via `shell -c` for a pane with
  no explicit command. Test: `TestDefaultCommand`.
  - Fixed a **pre-existing crash** surfaced by default-command: `handleRender` is
    a closure assigned partway through `run()`, but `actorDo`'s render pump could
    call it during the initial `setActive` — a pane emitting output the instant
    it spawns (any `gtmux new 'echo x; sleep 100'`, not just default-command)
    panicked the session on a nil handleRender, and with exit-empty on that killed
    the whole daemon. Fixed at the pump site (`pumpRender` nil-guards; dropping a
    pre-attach render is safe — no client, grid state fullSyncs on attach).
- [x] `bell-action` / `activity-action` (alert scope: any/none/current/other).
  Session options plumbed like the rest; `handleRender` now gates the
  monitor-activity/-bell alert through `alertFires(action, isCurrent)` instead of
  a hardcoded "other". tmux defaults matched: bell **any** (was wrongly "other",
  so a bell in the current window never alerted), activity **other**. Test:
  `TestActivityActionNone` (e2e: action none suppresses the alert the default
  would raise; switch-back proves the output happened).
- [x] `word-separators` (client copy-mode `w/b/e`): was space-only (the
  "no punctuation classes" note); now whitespace **or** any word-separators char
  bounds a word, via `copyMode.isBoundary`. Client option (copy-mode is
  client-owned), default = tmux's ASCII-punctuation set, fixed at copy-mode entry
  like mode-keys. Test: `TestCopyModeWordSeparators` (unit: with/without seps the
  same line yields different word landings).
- [x] `message-style` (client): transient status messages + the command prompt
  now have their own fg/bg/attr (tmux default black-on-yellow), consumed by
  `renderPromptLine`. Was reusing status-style colors. Test:
  `TestLoadClientStyleOptions`.
- [x] `status-left-length` / `status-right-length` (client): `renderBar` caps
  each segment via `truncCells` (0 = unlimited). Default 0 diverges from tmux's
  10/40 on purpose — gtmux's default status-left is longer than 10, so tmux's cap
  would truncate the default bar. Test: `TestCompositorStatusLeftLength`.
- Renderer check done (this is real work, not lying no-ops — the renderer
  consults styles throughout). Already-existing, not re-added: `status-style`
  (`status_style`), copy-mode selection style (`copy_selection_fg/bg`).
- [x] **Loader provenance** (unblocks the aliases below): the client loader ran
  all `gtmux.options.X` entries from one merged table via `opts.ForEach` (random
  order), so two option names writing the same field raced non-deterministically.
  Fixed in `LoadClientWith`: `gtmux.options` is swapped to a fresh table before
  the user file runs, and default-file opts apply first, then user-file opts —
  so a user's aliasing option deterministically wins over a default seeding the
  same field. Guarded by `TestLoadClientOverrides` (user `mouse=false` /
  `status_style=fg=red` beat the bundled defaults). ponytail: file-level
  provenance only; two aliases in the *same* file still race (needs an ordered
  replay via a `__newindex` metatable) — nobody aliases within one file.
- [ ] Option families — remaining, deferred with reasons:
  - `mode-style` — **now unblocked** by the loader-provenance fix above. It
    aliases the existing `copy_selection_fg/bg` (seeded by `default_client.lua`);
    a user `mode-style` now wins deterministically. Wire it (alias → the copy-
    selection style fields) when wanted — the loader wall is gone.
  - `pane-border-style` — **done (inactive).** New `InactiveBorderFG/BG/Attr`
    (config/client.go) via the `applyStyle` helper; the compositor's border
    rendering (both the divider ring and the pane-border-status label) now reads
    them instead of a hardcoded `DarkGrey`. Default keeps `DarkGrey` so it's a
    no-op until set. Unique fields → no loader-alias trap. Tests:
    `TestCompositorPaneBorderStyle` (render, discrimination-checked),
    `TestLoadClientStyleOptions` (loader round-trip). `pane-active-border-style`
    (full fg/bg/attr) — aliases the existing `active_border_fg`; **now unblocked**
    by the loader-provenance fix. Wire it (alias → active-border style fields)
    when the active-border bg/attr is wanted.
  - multi-line status (`status 2..5`) — **done.** `status` N reserves N rows.
    The count rides in Attach/Resize (`proto.Attach/Resize.StatusLines`), so the
    server sizes the window grid `rows - statusLines` (`winRows`, tracked via a
    session `statusLines` var updated in `applySize`) instead of a hardcoded −1.
    Client: `StatusLines` + `ExtraStatusFormats[4]` (config), row-math
    (`statusLines`/`totalRows`/`contentOffset`/`statusRowKind`) treats the status
    block as a range with the main bar at the screen edge (tmux status-format[0])
    and lines 2..5 stacking inward, each an expanded full-width format
    (`status_format_2..5` → `renderExtraStatus`). Tests: `TestMultiLineStatus`
    (e2e — asserts the extra line renders AND `#{pane_height}` = rows − N, so it
    discriminates the server sizing; discrimination-checked), `TestCompositorMultiLineStatus`
    (render), `TestLoadClientMultiLineStatus` (loader). `status off` now hides
    the bar (see Cheap three below); runtime `set status N` takes effect on
    reattach (read at attach), not live.

### Config-parity wave (gaps found cross-referencing ~/.tmux.conf)

Seven `.tmux.conf` features gtmux couldn't express, closed together:

- [x] **`run-shell` bindable in Lua** (`gtmux.run_shell(cmd)`): emits a
  `run-shell` Action (cmd stays one arg → `runCommand` `rest` keeps spaces).
  Was: no way to bind an arbitrary shell command in config.
- [x] **`choose-tree -f <format-expr>`**: server evaluates the expression per
  session via `internal/format` (`format.Expand` truthy), keeping only matches
  — e.g. workspacer's `#{m:pfx-*,#{session_name}}`. `choose_tree` Lua now passes
  args through. Test: `TestChooseTreeFilterExpr`. Integration gap left: gtmux
  config is global, so the dynamic workspace prefix must come from a user option
  the workspacer gtmux-backend sets (`@workspace_prefix`).
- [x] **command-prompt template quoting**: config split the template with
  `strings.Fields`, shredding quoted nested commands (`new-window 'foo %1'`).
  Replaced with a quote-aware `splitFields` so a quoted command stays one
  template field; per-field `%N` substitution then preserves it. Test:
  `TestSplitFieldsQuoting`. Unblocks the workspacer `P` bind.
- [x] **copy-mode mouse options** (client): `copy-wheel-lines` (scroll amount,
  was hardcoded 3) and `copy-drag-finish` (release-yanks-and-exits, tmux default
  on; off = selection persists — tmux's `unbind MouseDragEnd1Pane`). gtmux has
  no copy-mode keytable, so these are options not `send -X` binds. Test:
  `TestCopyDragFinishOption`.
- [x] **`set-clipboard`** (client): gates the OSC 52-on-yank emit; `off`
  suppresses it (internal paste buffer still set). Default `external` (tmux).
- [x] **`status-keys`** (client) + prompt line editing: `emacs` (default) adds
  C-u (kill line) / C-w (kill word) to the command prompt; `vi` is plain — ESC
  cancels the prompt so gtmux can't do modal editing (stated ceiling). Tests:
  `TestPromptEmacsEditing`, `TestNewClientOptions`.
- [x] **Picker pane preview** (tmux choose-tree's visual selector): tmux-style
  two-column bordered box — list left, the highlighted session's live-colored
  pane content right, titles on the top border, selection highlighted.
  `OpenPicker.Previews` carries `[][]emu.Line` (styled cells, real SGR colors):
  self from the local active pane, peers via a `previewEvent` cross-session
  query (mirrors `info()`); `previewSnap` trims trailing blank rows + caps 12.
  Client `overlayRowSplit` blits each preview glyph with its own fg/bg. Wired for
  choose-session + choose-tree. Tests: `TestPickerPreview` (layout + color
  survives). Verified live via grim (blue `ls` dirs + colored prompt in the
  preview). ponytail: frozen at open, not live (tmux's follows keystrokes); a
  live version needs per-move round-trips.

### Cheap three (further gaps)

- [x] **`window-status-style`**: inactive window entries in the status bar take
  their own fg/bg/attr instead of inheriting `status-style`. New
  `WindowStatusFG/BG/Attr` + `WindowStatusStyleSet` (config/client.go, via
  `applyStyle`); `renderBar` uses them for non-active entries, keeps
  active-window colors for the current one. No-op until set → no loader-alias
  trap (unique fields). `window-status-current-style` (aliases `active_window_*`)
  is now unblocked by the loader-provenance fix — wire it when wanted.
- [x] **`status off`**: `status off`/`0` hides the bar (`StatusLines` can be 0),
  `status N` (1..5) sets line count. `statusLines()` returns 0 when off; the
  compositor overlays copy-mode help / prompt / transient messages on the last
  content row so they stay reachable with no bar. Tests: loader + `renderPromptLine`.
- [x] **`new-session` (runtime)**: `new-session [-d] [-s name] [-c dir]` creates
  a detached shell session via `reg.resolveGroup` (auto-names on empty, errors
  on dupes). Always detached (can't move the acting client mid-command). Test:
  `TestNewSession` (e2e). Skipped: `[command]` arg + `-A` attach-or-create — add
  when spawn-with-command or one-shot attach is wanted.

### Keybind refactor (multi-byte keys)

- [x] **Binds keyed on a canonical key string, not a single `byte`**: unlocks
  Meta (`M-h`), function keys (`F1`–`F12`), and named keys (`Up`/`Home`/`PgUp`/…)
  as bindable keys, both prefix and root (`bind -n`). `parseKey` split into
  `parseKeyByte` (prefix — always one byte) and `parseKeyName` (canonical token:
  `"C-b"`,`"M-h"`,`"F5"`,`"Up"`,`" "`, folding aliases `Tab`/`Enter`/`Space`/`BSpace`).
  All four bind maps (`Binds`/`RootBinds`/`Repeat`/`Tables`) + overrides went
  `map[byte]`→`map[string]`; `Resolve*`/`SetOverride`/`ParseKey` sigs follow.
  Client input machine: a unified ESC collector (`ESC [`=CSI / `ESC O`=SS3 /
  printable=Meta) canonicalizes bytes to the same token, routed through the same
  tables; `byteKey`/`csiKeyName`/`ss3KeyName` mirror `parseKeyName` exactly
  (pinned by `TestReaderParserAgree`). Control folding stays byte-identical to
  before, incl. non-letters `C-\`/`C-]`/`C-^`/`C-_` (0x1c–0x1f) — a live user
  bind (`bind_root C-\`) that a naive a–z guard would have silently dropped.
  Tests: `TestParseKeyName`, `TestReaderParserAgree`. Live-verified via tmux:
  root `M-n` fires, prefix `F5` fires, and an unbound `Home` forwards its raw
  bytes to the focused app (the one trap of adding a root escape path). The
  hardcoded prefix+arrow/resize/PgUp fallbacks are unchanged (a user prefix-bind
  for the same named key now overrides them). Excluded: full modifier matrix
  (`C-S-x` / CSI-u — terminal-dependent); `C-[`==ESC unbindable (escape lead);
  `Ctrl`+digit (terminals emit no distinct byte). ponytail ceiling: root escape
  sequences are recognized only within one read chunk (terminals deliver them
  atomically) — a bind only misses if its sequence splits across reads;
  passthrough stays correct either way. Real disambiguation of a lone `ESC` vs an
  `ESC`-prefixed sequence needs tmux's `escape-time` timer — the knob to add if
  split sequences ever bite.

### Client hardening

- [x] **Terminal restore on goroutine panic**: raw-mode/mouse cleanup lived only
  in the main goroutine's `defer`s, which the runtime skips when a *spawned*
  goroutine panics — so a client crash left the pane wedged (raw mode + mouse
  reporting on). Fixed in `RunGroup`: an idempotent (`sync.Once`) `restoreTerm`
  (disable mouse, leave raw mode) called from both the exit defer and a package-
  level `guardPanic(restore)` deferred at the top of each spawned goroutine
  (input + SIGWINCH) — it restores, then re-raises so the crash still surfaces.
  Tests: `TestGuardPanicRestoresThenReraises` (restore-before-reraise, panic
  value preserved); live-verified by crashing the input goroutine via an
  env-gated trigger and confirming the pane returned to a cooked shell
  (`stty` icanon/echo). Defense-in-depth — the `charMotion` panic that first hit
  this is already fixed. Title/kitty restore stays main-only (cosmetic).

Skipped deliberately: `customize-mode`, `lock-server`, `server-access`,
`list-commands`, `terminal-features`/`terminal-overrides`: introspection/
tooling nobody's config touches; add if something breaks without them.

Not duplicated here: still tracked where they were noted: the small
deferred scraps in PARITY.md (popup mouse/resize/styling flags,
`refresh-client -S/-C` depth, `list-clients -t` filter, `send-keys -N/-R`,
real lock password, remaining ~30 hook names). The window-actor fork is done
(session groups, linked windows, aggressive-resize: WINDOW_ACTORS.md +
PARITY Tier 6). Still forked out: control mode `-C` (PARITY Tier 8).

## B: customizations on top (the meat & potatoes)

Details to be fleshed out per item before work starts.

- [ ] **Richer Lua API**: expose more gtmux internals to config Lua
  (surface TBD).
- [ ] **Pluggable client frontend**: fully customizable status bar:
  user-defined items/widgets, multiple status bars, plugin-style extension
  points. Pluggable as fuck.
- [ ] **Theming**: theme system: colors/styles as a swappable set, beyond
  the current per-option fg/bg. Likely two built-in themes in ONE client (not
  two binaries): tmux-classic vs a modern lipgloss-powered look — a value/glyph
  swap, not a second render path. Absorbs the deferred tmux style aliases
  (`mode-style`, `pane-active-border-style`): keep their *parsing* for tmux-
  config compat, but point them at the theme struct, not standalone ClientConfig
  fields — doing them standalone now would just get rewritten by this.
- [ ] **Program-aware Lua hooks**: detect the foreground program in a pane
  (cli/tui, e.g. Claude Code) and fire a Lua callback with real context on
  program change. Grows into a general hook/callback system for config Lua:
  richer than tmux-style `set-hook` (fixed events, command strings only).
- [ ] **Frontend rewrite exploration**: evaluate moving client rendering to
  the lipgloss/Charm ecosystem for a properly polished look.
- [ ] **Session persistence across reboots**: save/restore sessions
  (window/pane tree, layouts, working dirs, running commands; scrollback
  TBD) so a reboot doesn't lose the workspace. tmux needs plugins for this
  (resurrect/continuum): built-in here.
- [ ] **Workspacer replacement**: cover
  github.com/JamesTiberiusKirk/workspacer either via integration or by
  absorbing its features (project/workspace-driven session creation,
  pickers, prompts), through first-party features and/or the Lua API.
  Depends on: multi-arg `command-prompt`, `new-window <command>` (A),
  choose-tree filtering, richer Lua API (B1).
- [ ] **Session preview + fzf-style pickers**: live pane preview in the
  session/window/tree pickers (like tmux choose-tree's preview, but nicer),
  and fuzzy-find-as-you-type filtering across the pickers generally
  (choose-session/window/buffer/tree, workspacer flows).
- [ ] **GO Client SDK:** golang exported sdk for custom clients to talk to the server.
- [ ] **Temp shared panes:** smth that will let us make quake style terminals. They can be either shared across sessions, workspaces or not at all
  - tho i think we already inherited smth similar from tmux itself
- [ ] **Custom views (pane-mirror)**: synthetic session that gathers specific
  panes mirrored from other sessions (e.g. all Claude panes). Mirror +
  temporary-own: focused view wins the size vote, PTY reflows, reverts when the
  home session refocuses (reuses the aggressive-resize vote machinery per-pane).
  Lazy path if panes are one-per-window = a "gather" command over `link-window`,
  no refactor; the pane→actor fork is only needed to pull one pane out of a
  multi-pane window. Full plan in PARITY.md Tier 6 (under Custom views). Related
  to the Temp-shared-panes item above.
