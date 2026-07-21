# Bugs

> NOTE: the two paste items below turned out to be the STALE INSTALLED binary
> (`~/go/bin/gtmux` @ 958e866, no paste fixes). The working tree handles both —
> verified: paste-buffer wraps correctly when 2004 is on, payload newlines
> survive the client FSM. Reinstall (`go install ./cmd/gtmux`) + restart the
> server to clear them.

- [x] **`ctrl+shift+v` strips newlines / `prefix + ]` char-by-char + Enter** —
  both were the STALE INSTALLED binary (see note above), not a working-tree bug.
  The working tree wraps paste-buffer and preserves payload newlines through the
  client FSM — verified. Clears on reinstall + server restart.


- [x] **`ctrl+shift+v` pasted into the wrong pane** — the paste landed in the
  pane the text was copied *from*, not the focused one.
  - Cause: not a routing bug — a client focus bug. A mouse drag-select yank
    returns `exit:true`, but the mouse handler ignored it and left `comp.copy`
    set (the keyboard path clears it, `compositor.copyFeed`). The stale copy-mode
    overlay then swallowed the click meant to focus another pane, so the server's
    active pane never moved off the copy-from pane, and the paste — correctly
    written to the active pane — landed there.
  - Fix: clear `comp.copy` on `res.exit` in the mouse path
    (`internal/client/client.go`), mirroring the keyboard path.

- [x] **Text clipped across the border in the left dock** — wide runes
  (CJK/emoji) in dock/widget text bled past the dock border.
  - Cause: `paintStrip`/`paintRow` (`internal/client/widget.go`) laid text one
    rune per column, ignoring display width and never emitting the `Char==0`
    spacer cell after a wide glyph. `emu.WriteLine` keeps column alignment solely
    via those spacers and advances by each glyph's own width, so a wide rune with
    no spacer shifted the rest of the strip one column right, over the border.
  - Fix: a shared width-aware `layText` helper lays runes with spacers and clips
    to display columns, dropping (not spilling) a wide rune that would straddle
    the boundary. Used by both paint paths. Covered by
    `internal/client/laytext_test.go`.
  - Investigated the click hit-test too: the feared `lineText` NUL-corruption
    doesn't exist — the widget canvas uses a space base, never a `Char==0`
    spacer (`config/client.go:688`). The only residual is that the canvas is
    1-cell-per-rune by design, so a widget that draws wide runes AND places
    clickable regions after them can be a column off. That's a
    canvas-coordinate-model limitation, not the dock clipping bug; not worth a
    model rewrite for the ASCII docks in use.

- [x] **Session picker now preselects the active session** — it opened on the
  top item instead of the attached session.
  - Fix: added `Sel` to `proto.OpenPicker`, set to the attached session's index
    in `chooseSession` (`internal/server/session.go`), applied in `newPicker`
    (`internal/client/prompt.go`). choose-tree/client/buffer pass 0 (their
    header-row + filter layout makes preselect fiddly; top is fine).

- [x] **Nested sessions on the same host are now refused** — attaching to a
  gtmux session from inside one of its own panes rendered a session into itself.
  - Fix: `RunGroup` (`internal/client/client.go`, the choke point for both
    `attach` and interactive `new`; `-d`/`run` bypass it) refuses when `$GTMUX`'s
    socket field equals `proto.SockPath()`, with a tmux-style "unset $GTMUX to
    force" message. Compares only the socket, so attaching to a different server
    still works. Verified: same-socket refuses, different-socket and unset pass.

- [x] **Pasted bytes ran through the keystroke FSM** — a pasted prefix byte or
  bind key was interpreted as a keystroke.
  - Fix: a `pasting` state (`internal/client/client.go`), set/cleared by the
    ESC[200~/ESC[201~ markers, routes the payload straight to `fwd`, bypassing
    the prefix/bind machine; the mouse pre-scan is skipped while pasting too.
    Verified live: a paste containing `C-b` + `c` passed through as data and
    created no window.
  - The mouse pre-scan now uses its own raw-byte paste matcher (`advancePaste`),
    independent of processInput's state, so a paste is detected WITHIN the
    opener's own read chunk and across reads — a pasted SGR-mouse sequence can no
    longer be eaten, even in the first chunk. Covered by `TestAdvancePaste`.

- [x] **`set -g status-style reverse` now works** — fixed as a side effect of
  the reverse-video fix. Config sets `AttrReverse` (`config/client.go:461`),
  `styleRun` builds the glyph directly (`statusrender.go:14`), and `sgr` now
  emits `;7` for a directly-built glyph. Guarded by `TestReverseVideoOnDirectGlyph`
  in `internal/emu/reverse_test.go`.

- [x] **Couldn't click to focus a mouse-tracking pane** — clicking into a pane
  running a mouse-tracking app (nvim, less, Claude Code) didn't switch focus.
  - Cause: `mouseAction` (`internal/client/compositor.go`) returned
    `forward: true` for a `WantsMouse` pane and returned early, never emitting
    the `select-pane` that focuses it. Plain panes (zsh) focus fine — which is
    why it looked intermittent. tmux focuses on click even for tracking apps.
  - Fix: on a left-press of an unfocused tracking pane, emit `select-pane` AND
    still forward the event. Covered by `TestClickToFocusTrackingPane`; verified
    live (click into a mouse-tracking pane moved focus to it).

- [x] **Server crashed (`panic: send on closed channel`), killing every
  session** — a window teardown while a pane still had pty output in flight
  panicked, and a panic in any goroutine takes the whole process down (that is
  how `dots` died mid-use). Pre-existing, not from the paste work.
  - Cause: the pane reader goroutine (`session.go` watchPane) sends `outputMsg`
    straight to `origin.events`, bypassing the `s.events` path where the
    `wa.stopped` guard lives. `finishStop` did `close(wa.events)`; a straggler
    read landing after the close sent on a closed channel. Closing a channel
    from the receiver side while senders are still alive is never safe in Go.
  - Fix: `finishStop` enqueues a `stopMsg` sentinel that ends `run()` instead
    of closing the channel. FIFO drains everything already queued first; a late
    reader send just sits unread in the 256-buffer. Covered by
    `internal/server/actor_test.go`; race detector clean; no panic across 40
    chatty window kills.

- [x] **Random cursor appearing everywhere on some screen refreshes** — a stray
  cursor flickered at arbitrary screen positions during redraws, most visibly
  when scrolling or moving the cursor in vim.
  - Cause: `emit()` in `internal/client/compositor.go` painted its dirty rows
    with the cursor still visible, emitting `?25h` only at the very end, so the
    terminal's real cursor tracked the write head for the whole paint. One
    `Write` syscall doesn't make the frame atomic — the terminal parses
    incrementally, and a large repaint (a vim scroll dirties the entire scroll
    region) leaves ample time to render mid-frame.
  - Fix: bracket the frame in `?25l` / `?25h`, plus DECSET 2026 (synchronized
    update) so terminals that support it swap the whole frame at once.

- [x] **No highlight on shell tab-completion with multiple options** — the
  selected entry in a completion menu rendered as plain text.
  - Cause: `setChar` (`internal/emu/state.go:265`) baked reverse video into the
    cell by swapping FG/BG at write time, while *also* leaving the `attrReverse`
    bit set — so `render.go` suppressed `;7` to avoid a double swap. For default
    colors the swap is invisible: `DefaultFG`/`DefaultBG` both lack the
    `colorSet` bit, so `writeColor` emits nothing for either. A reversed default
    cell serialized to a bare `\x1b[0m`. That is exactly zsh's default `ma`
    style. Glyphs built directly (never through `setChar`) were broken too — no
    swap *and* no `;7`.
  - Fix: dropped the swap; the `attrReverse` bit is now the single source of
    truth and `sgr` emits `;7`, so the terminal does the swap. Copy-mode
    overlays clear the bit where they force explicit colors
    (`internal/client/compositor.go:1511`). Covered by
    `internal/emu/reverse_test.go`.

- [x] **Faint/dim (SGR 2) was dropped entirely** — dim text rendered at normal
  intensity, e.g. Claude Code's tab-to-complete suggestion showed as normal
  white instead of gray.
  - Cause: `setAttr` (`internal/emu/state.go:757`) had no `case 2`. The param
    fell through the switch and was silently discarded — there was no dim bit
    anywhere in the emulator, so `render.go` had nothing to emit.
  - Fix: added `attrDim`/`AttrDim`, set on `case 2`, cleared by `case 0` and
    `case 22` (which previously cleared only bold — SGR 22 is "normal
    intensity" and clears faint too), emitted as `;2` from `sgr`. Bold's FG+8
    brightening in `setChar` is now suppressed when dim is also set.
  - Also fixed a latent hazard found on the way: the attr iota blocks in
    `state.go:18` and `module.go:13` are separate declarations that must agree
    bit-for-bit, and had already drifted (`attrOpaque` existed in only one). A
    mismatch is silent. `TestAttrConstantsAligned` now guards it.

- [x] **`ctrl+shift+v` paste was slow and auto-entered** — took seconds, and
  newlines executed instead of being inserted.
  - Cause: one bug, not two. The client advertised mouse DECSETs but never
    `\x1b[?2004h`, so the outer terminal saw an app with no bracketed-paste
    support and dumped the clipboard raw, `\r` bytes included. The shell
    executed each line — that is the auto-enter, and the slowness is downstream
    of it: N pasted lines meant N real command executions, N prompt redraws, N
    rounds of pty output. The input path itself was never the bottleneck (a
    500-byte paste is 1 read, 1 gob message, 1 pty write, zero round-trips).
  - Fix: advertise `?2004h` at attach, disable it in `restoreTerm`. The
    `200~`/`201~` markers have no `csiKeyName` entry, so `finishEsc` already
    forwards them verbatim to the pane and the shell handles the paste natively.
  - Verified live on a throwaway `GTMUX_SOCK` server: `od -c` confirmed the
    markers reach the pane byte-identical to a no-gtmux control, and a pasted
    `echo AAA\recho BBB` sat on the zsh prompt as two unexecuted lines, running
    only on Enter. Not verified: that wezterm itself sends the markers — the
    test injected them, and under a nested tmux the client's `?2004h` goes to
    the outer tmux rather than wezterm.

- [x] **Bracketed-paste gating: an app that never enabled 2004 saw literal
  `200~`/`201~`** (sudo prompt, minimal TUIs) — and the first fix for it wrongly
  coupled the client to the server.
  - First attempt (now replaced): pushed the app's 2004 state to the client as
    `PaneRect.WantsPaste`, refreshed via a Layout resend on every mode flip, and
    let the CLIENT strip the markers. That put per-pane terminal state on the
    client — against the client/server decoupling — and made a client-only fix
    silently need a server restart (a new client on an old server saw
    `WantsPaste=false` and stripped every paste → multi-line executed).
  - Final design (server-side gate): the client forwards the `200~`/`201~`
    markers unconditionally; the SERVER strips them in `pane.filterPaste` when
    the target pane's app hasn't enabled 2004. State (the emulator's live 2004
    flag) and action (writing to the pty) are colocated — the client tracks no
    per-pane mode. Applied at every `handleInput` write site (active, popup,
    synchronize-panes per pane). `paste-buffer` keeps its own server-side wrap.
  - Dropped `PaneRect.WantsPaste`, `compositor.activeWantsPaste`, and the
    2004→Layout-resend (zsh toggles 2004 every prompt, so that was needless
    traffic). `filterPaste` only rewrites when data carries ESC and 2004 is off.
    Covered by `TestFilterPaste`; verified live both ways (zsh holds a multi-line
    paste; `cat` receives the content with markers stripped).
  - Inherent, not a bug: correct gating needs the live 2004 state, which is
    server-side, so a *current server* is required regardless of design — no
    arrangement makes correct multi-line paste work against a stale server.
