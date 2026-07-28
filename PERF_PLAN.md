# gtmux performance fix plan

Handoff plan. Symptoms reported: full-screen TUIs (codex, vim) get slow while
scrolling or resizing; occasional general sluggishness even in a bare zsh
prompt; codex visibly "auto-scrolls" when its pane resizes (does not happen in
tmux or a plain terminal).

Architecture primer: per-session owner goroutine (`internal/server/session.go`
`run()`) holds all session state. Each window has a `windowActor` goroutine
(`internal/server/actor.go`) that owns its grids; PTY readers send 4KB
`outputMsg` chunks to the actor, which applies them to the emu, computes a
per-pane row diff (`dirtyContent`, `internal/server/pane.go:461`), and hands a
`renderMsg` back to the session, which gob-encodes it to each attached client.
The client (`internal/client/client.go` decode loop → `compositor.apply` in
`internal/client/compositor.go:816`) applies diffs and repaints dirty rows.

Implement fixes in this order. Build with `go build ./...`, test each fix
before the next (interactive test setup is in the project CLAUDE.md — tmux
session `ff-gtmux`, pane `%31`). Do NOT commit anything; leave all changes
unstaged.

## Fix 1 — stop stamping status onto every message (biggest win)

Problem: `send()` (`internal/server/session.go:712`) calls `stampStatus` →
`currentStatus()` (session.go:659) on EVERY outgoing message — including one
`PaneContent` per 4KB PTY chunk (session.go:1630). `currentStatus` does, per
call: a `TIOCGPGRP` ioctl + `/proc/<pgid>/comm` read + `/proc/<pid>/cwd`
readlink (`internal/server/pane.go:271,298`), `shellRunner.run`, and — when
widget snapshots are active — `buildSnapSession()` (session.go:619), which
repeats those /proc syscalls for every pane; all of it gob-encoded per
message. Client side, every message therefore has `msg.Status != nil`, so
`compositor.apply` (compositor.go:875) re-expands every Lua format widget and
dock, re-renders any open modal, and marks all status rows dirty — per 4KB
chunk. Scrolling vim = hundreds of full status pipelines per second.

Change:
- `send()` and `sendTo()` no longer stamp status.
- Add `sendStatus()` (stamps `currentStatus()` onto a `&proto.ServerMsg{}`
  and broadcasts) — the 1s tick (session.go:3441) uses it.
- Audit the ~50 `send(`/`sendTo(` call sites in session.go: any path that
  changes status-visible state must ALSO deliver fresh status (either stamp
  that message or call `sendStatus()` after). Status-visible state = active
  window index, window list membership/names, session name, zoom flag,
  activity/bell/silence flags, `statusMsg` (`showMessage`, session.go:1613),
  prompt label/text, user options (`withUserOpts`), pane title/command/path
  shown in formats. Window switch and `showMessage` must be instant; anything
  missed self-heals within 1s via the tick, so bias toward NOT stamping when
  unsure.
- Client: zero changes (`msg.Status != nil` gate already exists).

Verify: attach with a Lua status config; scroll vim — status bar still ticks
1/s, window switch updates the bar immediately, CPU on both binaries drops
sharply during scroll.

## Fix 2 — no subprocess spawns on the session goroutine

Problem: two synchronous fork+execs run on the session owner goroutine, so
keystrokes queue behind them:
- `shellRunner.run()` (`internal/server/status.go:44`) spawns `sh -c` inline
  when its 15s cache expires — called from `currentStatus()`. A slow
  `#server()` command freezes the whole session, input included.
- The 1s tick calls `refreshStatus()` (session.go:584→3437) which runs
  `gitBranch()` (status.go:15) — a synchronous `git rev-parse` spawn every
  second.

Change: make `currentStatus()` read-only w.r.t. subprocesses. On the tick,
fire ONE goroutine that computes `gitBranch(...)` and the expired
`shellRunner` commands, then posts the results back via `s.events` (new event
type, handler just updates `cachedGitBranch` / the shell cache). Skip
launching a new goroutine if the previous one hasn't returned (a slow command
must not pile up goroutines). `pane.cwd()` for the git dir must be captured
before spawning the goroutine (it touches pane state).

Verify: put `#server(sleep 2; echo hi)` in a status format — typing in zsh
stays smooth; the widget shows "hi" ~2s later.

## Fix 3 — coalesce PTY output in the window actor

Problem: `windowActor.run()` (actor.go:117) processes one 4KB `outputMsg` at
a time: apply → full diff (`dirtyContent` rebuilds every dirty row) → gob
encode → client repaint. A fast-scrolling app produces hundreds of these per
second; most of the diff work is redone on rows that change again in the next
chunk. tmux batches all readable output before redrawing.

Change: in the `outputMsg` case, after applying the first chunk, drain
further immediately-available events non-blockingly:

    for more := true; more; {
        select {
        case e2 := <-wa.events:
            if o2, ok := e2.(outputMsg); ok && sameApplicable(o2) {
                apply o2 (accumulate bell/cmdExits/clipboards/modeFlip)
            } else {
                handle-or-requeue: process doMsg/stopMsg cases as today,
                or break out and handle normally
            }
        default:
            more = false
        }
    }
    then compute ONE dirtyContent diff / renderMsg fan-out.

Careful points: only coalesce chunks for the SAME pane with matching gen and
no relay entry (other panes' chunks: just apply them too and include their
panes in the fan-out — a per-pane "dirty panes this batch" set is the clean
shape). `doMsg` ordering matters (actorDo callers block on it): when a doMsg
is pulled during the drain, stop draining, emit the batched render first,
then run it. `bell`, `modeFlip`, `cmdExits`, `clipboards`, `hostOut` must be
accumulated across the batch, not taken from the last chunk only.

Verify: `yes | head -c 10M` in a pane; server CPU and client repaint rate
drop; output still correct (compare final screen with tmux running the same).

## Fix 4 — alt-screen resize must not reflow (fixes codex auto-scroll)

Problem: `State.resize()` (`internal/emu/state.go:318`) reflows whichever
screen is active — including the ALT screen (state.go:380–435): it rewraps
lines, pushes overflow rows into `altHistory`, and moves the cursor. There is
a `TODO(cfoust): what about when we're on the alt screen?` at state.go:339.
tmux/xterm never reflow the alt screen — content is truncated/padded in
place, cursor clamped; the app repaints itself on SIGWINCH. The reflow shifts
content before the app's own redraw arrives → the visible "auto-scroll" in
codex on pane resize. Note the unwrap loop at state.go:340 also runs
regardless of mode; it must not disturb the saved MAIN screen state while the
alt screen is active.

Change: in `resize()`, when `IsAltMode(t.mode)`: skip reflow — copy the old
alt-screen rows row-by-row into the new grid (truncate cols / drop bottom
rows if smaller, pad if larger), clamp `t.curSaved` (and the live cursor)
into bounds, never touch `altHistory`. Main-screen path stays as is. Check
existing emu tests still pass (`go test ./internal/emu/`), add one: resize
while in alt mode preserves top-left content and pushes nothing to history.

Verify: run codex (or `vim` then `:term`), drag-resize the wezterm window /
resize the pane repeatedly — content must not scroll or shift beyond what the
app itself redraws. Compare against tmux side by side.

## Fix 5 — resize debounce (ONLY if still needed)

After fixes 1–4, re-test interactive drag-resize. Each SIGWINCH currently
does: client sends `Resize` (client.go:1011) → server `applySize` → grid
resize + reflow of every pane → `fullSync()` (full content of all panes,
session.go:3699) → apps get SIGWINCH and fully repaint. If still laggy,
debounce in the client WINCH goroutine (client.go:1004): coalesce bursts with
a short timer (~50ms) before sending `Resize`, always sending the final size.
Do not debounce the local `comp.setPhysical`/`redraw` (chrome should track
the drag). Measure before writing this one — it may be unnecessary.

## Constraints

- No git commits, no staging — leave everything unstaged for review.
- Match tmux behavior when in doubt; tmux is the reference implementation.
- Keep diffs minimal; no new dependencies, no new abstractions.
- After each fix, build + run the interactive smoke test from CLAUDE.md
  before moving on.
