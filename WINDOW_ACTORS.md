# Window-ownership redesign (Option A: window actors)

Match tmux's real model: **windows are first-class shared objects, not owned by
sessions.** A session references windows through winlinks and holds only
per-session state (current window, clients, options). This unlocks the three
features that are otherwise impossible under the current per-session-owns-its-tree
design: **linked windows** (one window in several sessions), **session groups**,
and **aggressive-resize** (a window sizes to its actual viewers).

tmux gets this for free because it's single-threaded — one event loop owns
everything. gtmux chose per-session owner goroutines (no mutexes), so sharing a
window means giving the *window* its own owner goroutine.

## The invariant being preserved

Not "who owns a window" but **"one goroutine owns a set of shared state, no
mutexes."** Option A keeps it verbatim — it just makes the window (not the
session) the owner of pane/grid state. PTY output is still applied by exactly one
goroutine; nothing is locked.

## Target shape

- **Window actor** — a goroutine + event channel owning its pane tree, emu grids,
  and PTY readers. Mutated only via its channel. PTY readers post to the
  *window's* channel. On output: apply to the grid **once**, then fan out
  `PaneContent` to every *view* currently displaying the window.
- **View** — one (session, client) pair displaying a window; has an inbox the
  actor renders to (like a client's output queue today).
- **Session** — winlink list (window-actor refs) + per-session current window +
  clients + options. Routes window/pane commands to the relevant actor;
  subscribes/unsubscribes its clients as the current window changes.
- **Synchronous reads** — every `windows[active].active.term…` in `session.run`
  becomes a query round-trip to the window actor. This is the pervasive cost.

## Phase 0 — spike (DONE, `.tmp/spike.go`)

Validated the one hard seam in isolation: 8 concurrent producers → 4000 outputs,
each applied exactly once by the actor and fanned to **two views on the same
window**, which both rendered the identical `1..4000` stream; 4 concurrent
readers doing window-state query round-trips; a transient view joining/leaving
mid-flight. **`-race` clean, repeatably** (plain and `-race`).

**Finding that changes the migration:** view teardown must be **actor-coordinated**.
Closing a view's inbox while the actor might still fan out to it panics
(send-on-closed-channel). Fix: `unsub` carries a `done` channel the actor closes
*after* dropping the view; only then may the view tear down its inbox. Because the
actor is serial, once it processes the unsub no later output can target that view.
The same barrier trick (a query processed after all queued outputs) is how a
session knows the actor has finished applying before it reads/tears down.

Conclusion: **the seam holds — the rest is mechanical (large) threading.**

## Phase 1+ — migration (incremental, one seam at a time)

Order chosen so the tree keeps building/passing after each step:

1. **Extract `windowActor`** (DONE — `internal/server/actor.go`). It *embeds*
   `*window` so every `wa.panes`/`wa.reflow()`/… access promotes unchanged, plus
   an `events chan any` for later phases. `session.run`'s `windows` slice,
   `activeWindow()`, the window-taking closures (`windowName`, `paneVars`, …), and
   `resolveTarget` now use `*windowActor`; `pane.win` stays `*window` (the embedded
   pointer), so a handful of identity checks compare against `activeWindow().window`
   and `indexOfWindow` matches on `ww.window`. No goroutine yet — full suite green,
   `-race` clean, behavior identical. (Surfaced and fixed a latent clock-dependent
   test bug in `TestCustomKeyTable` en route — unrelated to the wrap.)
2. **Output application → actor** (DONE). `wa.applyOutput(p, data)` (grid write +
   pipe-pane tee) is now the actor's, called from the session's ptyOutput handler;
   added `window.actor` back-ref (via `newWindowActor`, reached as `pane.win.actor`)
   — the routing plumbing Phase 3 needs. Alert detection + client fan-out stayed
   session-side deliberately: they read "is this the *current* window," which is
   per-view under Option A (Phase 4). Behavior identical; e2e + `-race` green.
   (PTY readers still post to `s.events`; they move to the actor's channel in
   Phase 3 alongside its goroutine — routing to an undrained channel first would
   be pointless.)
3. **Give the actor its own goroutine.** THE CLIFF — grid ownership is
   all-or-nothing: once the actor mutates grids on its own goroutine, every
   session-side grid read races unless it also goes through the actor. `-race` is
   the guardrail (a racy partial fails it immediately, so there's no unsafe
   checkpoint).

   **3a — deadlock spike (DONE, `.tmp/spike3.go`).** Phase 0 proved fan-out but
   not the session⇄actor *bidirectional* dependency: the session does blocking
   query round-trips to the actor while the actor pushes renders back. Naively
   this deadlocks — and the spike reproduced it. **Finding:** the fix isn't just
   "pump renders while waiting for the reply"; the session also blocks *enqueuing*
   the query onto the actor's output-flooded inbox, so **every session→actor send
   must pump the render channel too**, not only the wait. With that, 6000 outputs
   + 3000 blocking queries under a buffer-1 render channel run deadlock-free,
   `-race` clean, repeatably.

   **Implication for 3b (the cutover):** the query/command helpers used in
   `session.run` cannot be plain blocking calls — they must be woven into the
   session's fan-out loop (service renders during both the send and the wait).
   That is the real cost, on top of converting every window read/mutation to a
   message. Not mechanical; a genuine concurrency-design step.

   **3b — racing-surface analysis (DONE).** `emu.State` is *partially*
   self-locked: `Title()`/`Mode()`/`Cursor()`/`CursorVisible()`/`Screen()`/
   `History()` take its `RWMutex`, so those reads are already safe to run
   concurrent with the actor's `term.Write` — **no conversion needed** (status
   build's `Title`/`Mode`, capture-pane's `Screen`/`History`, and `currentCommand`/
   `cwd` which read /proc, all stay put). What is NOT locked and therefore must
   route through the actor:
   - **`Cell(x,y)`** (unlocked): so `dirtyContent`/`fullContent`/`copySnapshot`
     → the fan-out `dirtyContent` already runs on the actor; `fullSync` content
     and copy-mode's `copySnapshot` become `query()`.
   - **the pane tree** (`w.panes`/`w.root`/`w.active`/`p.rect`, plain fields):
     `layout()` reads them → `query()`; the ~12 mutators (split, close, reflow,
     resize, resize-pane, select-layout, next-layout, swap, rotate, break-pane,
     join, respawn, zoom) → `do()`. The actor also reads `w.active`/`w.zoomed`
     when deciding to render, so those mutations must serialize through it.

   **Actor infrastructure landed** (`actor.go`): the goroutine run loop
   (`outputMsg` → apply + render-if-current, `setActiveMsg`, `doMsg`), plus
   `start(renders)`. Dormant — nothing calls `start()` yet, so the tree is still
   Phase-2 behavior, green + `-race` clean.

   **DEFINITIVE FINDING — there is no clean grid/tree partition; the flip is the
   full inversion.** I tried to scope the flip to "grid on the actor, tree on the
   session" and proved it impossible:
   - `dirtyContent`/`content` read `p.rect` (Rows/Cols) — a **tree** field the
     session's `reflow` writes. Grid rendering depends on tree state.
   - `reflow` calls `p.resize` → `term.Resize` — a **grid** op. Tree layout
     depends on grid state.
   - `emu.Cell(x,y)` is **unlocked**, so it races any concurrent `term.Write`
     regardless of which goroutine each runs on; and `p.rect`, `w.active`,
     `w.panes` are plain fields read all over `session.run` (input routing does
     `activeWindow().active.pty…` on the hot path) and mutated by split/close/
     select-pane.

   Every candidate boundary leaks a shared field across it. The only `-race`-clean
   design is **single ownership of ALL window state — grid and pane tree — on the
   actor goroutine**, with the session touching it exclusively through `do`/`query`.
   That is the full god-function inversion of `session.run`: ~20+ read/mutation
   sites plus the deadlock-free helpers, landed as one coordinated change that
   `-race` will reject if any site is missed — i.e. not safely single-passable and
   with no valid intermediate checkpoint.

   **Recommendation:** bank at this waypoint (design fully validated by both
   spikes, actor infra in place, the flip's true scope proven), and do the
   inversion as a dedicated effort on a branch — converting one command-family at
   a time behind the `do`/`query` helpers, `-race` after each — rather than a
   single tail-of-session dump. The alternative (locking `emu.Cell` + the tree
   fields) buys a smaller diff but abandons the no-mutex invariant that is the
   whole point.

   **3b conversion — in progress (inline `actorDo`).** The approach that makes
   this safe: `actorDo(wa, fn)` currently runs `fn` inline (actor dormant), so
   every conversion is behavior-identical and `-race` clean; the final flip swaps
   the body to route onto the goroutine. Families converted so far, each verified
   green:
   - **grid-read**: `fullSync`, all `Layout` sends (`sendLayout`), `copySnapshot`.
     (capture-pane exempt — `Screen`/`History` are emu-locked; popups exempt —
     windowless, session-owned.)
   - **tree-mutation**: split, close, navigate, select-last-pane, zoom, swap,
     break-pane, join-marked (cross-actor), resize-pane, layouts, rotate, respawn,
     applySize, resize-window, plus the `showNumbers`/`paneBase`/`marked` window-
     field writes and border reflows.
   - **tree-read → COLLAPSED.** Key finding: `actorDo` is a *synchronous
     handshake* (the session blocks until the actor runs `fn`), so tree *writes*
     never overlap session code, and the only concurrent actor work (`outputMsg`)
     merely *reads* the tree (`p.rect` in `dirtyContent`) — read/read-safe. So the
     pervasive `activeWindow().active` / `resolveTarget` reads need NO conversion.
     The single genuine session⇄actor write conflict is `p.pipeW` (both
     `applyOutput` and pipe-pane touch it) — routed through the actor.

   **THE FLIP — DONE.** Concurrency is on: each window runs its actor goroutine,
   the sole owner of its grid + pane tree. Output flows reader → `wa.events`
   (`outputMsg`) → `applyOutput` + render → `renderCh` → the session's
   `handleRender` (fan-out + activity/bell/silence). `actorDo` routes onto the
   goroutine with the deadlock-free render pump (3a). `setActive` mirrors the
   current window into the actors; `switchToWindow` range-guards the stale-`active`
   case from `removeWindowAt` (a bug the flip surfaced). Popups stay session-owned
   (no actor). **Full e2e passes under `-race` with zero data races; all unit +
   `-race` green.** The no-mutex invariant holds — each window and the session are
   each owned by exactly one goroutine, coordinating only through channels.

## Phase 4+ — what the inversion unlocks (next)

The hard part (the inversion) is done. Remaining, now tractable on top of it:
- **Pane migration** (break-pane / join-marked) — **DONE.** The problem: a pane's
  PTY reader is parked in a blocking `Read` on a *live* fd (unlike respawn, the
  shell survives the move), so it can't be paused or fenced to retarget it. Any
  reader retargeting hits post-swap stragglers or channel reordering (corruption).
  Fix: make the **session the single ordered router** — every reader posts to
  `s.events` (stable target), and the session forwards each chunk to
  `p.win.actor` via `forwardOutput` (pumps renders while enqueuing, 3a). Because
  break/join mutate `p.win` under an `actorDo` handshake on that *same serial
  goroutine*, a moved pane's chunks switch to the new actor with a clean
  happens-before: the old actor is drained by the migration's own
  `actorDo(old, closePane)` (FIFO) before the new actor touches the pane, so
  there's no overlap and no reorder. Cost: pane output takes one session hop
  before its actor (walks back part of the flip's direct-post) — acceptable, and
  the only shape that's correct given an unpausable reader. Covered by
  `TestBreakPaneUnderFlood` (`-race`, breaks a flooding pane and asserts it
  renders in its new window).
- **Teardown** — split by hazard profile (`stopActor` = `close(events)` + a
  `done` chan the actor closes on exit, drained on `renderCh` so a final render
  can't deadlock the wait; stop the actor *before* Close-ing panes, since both
  `applyOutput` and `p.Close` touch `p.pipeW`):
  - **Session-end (scope A) — DONE.** The teardown loop stops every actor still
    in `windows` before closing its panes. Clean because the event loop has
    already exited: no late reader event can reach a stopped actor, so renders
    are just discarded. Guarded by `TestKillSessionUnderFlood` (kill mid-flood →
    no deadlock/panic; the server runs as a plain subprocess so e2e observes
    crashes/hangs, not `-race` — server races are covered by
    `go test -race ./internal/server/`). Does NOT retroactively clean windows
    killed mid-session.
  - **Mid-session (scope B) — DONE.** `removeWindowAt` stops the removed window's
    actor (covering close-pane / pane-exit paths); `killWindow` routes through it
    *before* closing panes — which fixes the latent `pipeW` race (Close vs the
    actor's applyOutput); join-marked stops its drained, now-empty source window's
    actor. The two hazards resolved: (1) a late reader output/exit event for a
    just-killed window is dropped by a `wa.stopped` guard in the ptyOutput handler
    (set before `close(events)`, checked before any actorDo/forwardOutput — all on
    the session goroutine, so no sync), preventing a send-on-closed panic; (2) the
    stop-drain **discards** renders rather than `handleRender`-ing them, which
    dodges reading the mid-adjustment `active` entirely — a dropped render
    self-heals (fullSync on the window switch, or the next output chunk), and
    mid-session the actor is quiescent so `<-done` is near-instant (the discard is
    a rarely-hit deadlock backstop). Guarded by `TestKillWindowUnderFlood`.
- **Winlinks / linked windows — IN PROGRESS.** A session references windows by a
  winlink slice (one window can appear in several sessions) instead of owning
  `[]*windowActor`. Phasing (each keeps the tree green + `-race` clean):
  - **P0 — deadlock spike (DONE, `.tmp/spike4.go`).** Gate: a SHARED actor fans
    each render to N sessions, each doing blocking `actorDo`; naive `viewCh <- rm`
    per session deadlocks (a full view stalls the actor, so every other session's
    query hangs — strictly harder than spike3's single-session case). **Finding:
    the actor must never block on any one view — non-blocking send per view, DROP
    the content frame on a full channel.** 4 sessions × 6 producers, view buffers
    of 1 (forcing ~21k drops of 24k): no deadlock, actor applied every output,
    every session recovered the true final state via a final reliable query
    (models fullSync). `-race` clean ×3. **Design viable.**
    - **Refinement P1 must handle:** `renderMsg` carries `bell`; `handleRender`
      does activity/bell/silence off it. Content diffs drop freely, but an *alert*
      must not — split the droppable content-diff from the must-deliver alert/state
      (separate reliable path, or fold alert detection into the actor).
  - **P1 — actor fans out to a *set* of views — DONE.** `windowActor.renders` +
    `isActive` became `views []*view{renders, isActive}`; `run()` loops the set
    gating content per-view; `start()` subscribes the owning session's view;
    `setActive` toggles `views[0]` (the session's only view today). Dead
    `setActiveMsg` removed. Kept behavior-identical by holding the set at size 1:
    the send stays blocking and `dirtyContent` is taken per-view (both safe with
    one view). The two size>1 changes spike4 identified — non-blocking send +
    drop-content-on-full, and compute-the-diff-once with reliable bell delivery —
    are documented at the `run()` fan-out loop and deferred to P3 (they can't be
    enabled without a second view existing). Green: unit `-race`, full e2e.
  - **P2 — mechanical rename — DONE.** The session's `windows []*windowActor`
    became `windows []winlink{actor, view}`, relocating each session's per-window
    view handle from `actor.views[0]` into `winlink.view` — the handle that will
    disambiguate "this session's view" once an actor has several (P3). Slice
    position stays the display index (no explicit/sparse `idx` yet — additive
    later, no re-churn). `activeWindow()` still returns `*windowActor`, absorbing
    most call sites; `setActive` toggles `winlink.view`. Scoped to the rename on
    purpose: refcount + actor-coordinated sub/unsub were **left out** — at one
    view per actor they're dormant code `-race` can't exercise, so they land in
    P3 against a live second view. Green: unit `-race`, full e2e.
  - **P3 — the shared-window step** (registry window table; `link-window` /
    `unlink-window` = drop *this* session's winlink vs `kill` = destroy
    server-wide; `new-session -t` session groups; aggressive-resize). Brings the
    machinery the earlier phases deferred, each now exercised by a real second
    viewer: actor-coordinated view sub/unsub + refcount-aware teardown (spike.go
    pattern); non-blocking-drop fan-out + share-one-diff + reliable bell (spike4).
    - **P3.1 — reader rewire + origin-relay — DONE.** `watchPane` now posts a
      pane's output straight to its window actor (session-independent — no freeze),
      not the creating session's `s.events`; the session-routing added for the
      earlier migration fix is retired (`forwardOutput` gone). break/join set
      `origin.relay[p]` (the pane's birth actor forwards its output to the new
      window actor — one ordered path reader→origin→dest); `adoptWindow` no longer
      reflows so the origin stays p's sole writer until the relay handoff (reflow
      moves onto the destination actor). Relay lifetime: `stopActor` on an actor
      still relaying becomes a `stopping` zombie; `dropRelay` (pane exit)
      finishes it once its last relayed pane closes. Guarded by
      `TestBreakThenKillOrigin` (break a flooding pane out, kill its origin window
      → the moved pane keeps flowing to millions, no freeze). Green: unit `-race`,
      full e2e. Known bounded leak: a zombie whose session ends before its relayed
      pane exits (only migrated panes, like the reader-goroutine leak).
    - **P3.2 — fan-out hardening — DONE.** `run()` now fans out **non-blocking**
      (drop on a full view) and computes the destructive diff **once**, sharing the
      read-only pointer across current views (spike4/spike5). Behaviour-identical at
      one view: a live session drains continuously so its 256-buffer never fills →
      no drops; only a hung/dying view drops (self-heals), which is what makes a
      second viewer freeze-safe. Alerts are best-effort (a bell on a dropped frame
      is missed only under sustained backpressure to a live session — doesn't
      happen). Green: unit `-race`, e2e subset.
    - **P3.3 — link-window + refcount teardown — DONE.** Two sessions display the
      same live window:
      - **`link-window -s <session>:<window>`** obtains the source window's actor
        via a session-to-session handoff (a `linkRequest` on the source's event
        channel — no global table) and subscribes a new view (`subscribeView`, an
        actor-coordinated `views` append). Output fans to both sessions.
      - **Refcount teardown** (`releaseWindow`): every teardown path (session-end,
        kill-window/removeWindowAt, join-marked's src-drop) drops *this* session's
        view via `unsubscribeView`; the actor stops + panes close only on the last
        viewer. At one viewer (the common case) last is always true → identical to
        the old unconditional stop.
      - **Cross-session destroy** (`destroyWindow` + `winlinkGone`): kill-window and
        a window whose last pane exits are gone for everyone — every other viewer is
        told (via its event channel) to drop its winlink; the last unsubscribe stops
        the actor. `unlink-window` = drop just this session's view (survives
        elsewhere).
      - **Per-view alert state**: `activity`/`bell`/`silence`/`silenceTmr` moved from
        the window onto the `view` (tmux's per-winlink flags) — else two sessions'
        `handleRender` would race them on a shared window. Mutated only on the
        viewing session's goroutine.
      - Guarded by `TestLinkWindow` (shared live counter advances in both sessions;
        unlink survives in the owner) and `TestLinkWindowKillPropagates` (kill in the
        owner drops the borrower's winlink, which stays responsive). Green: full unit
        `-race`, full e2e. Known rare edge: two sessions linking each other at the
        same instant → both link commands time out (2s) rather than deadlock.
    - **Remaining P3:** `new-session -t` (session groups — subscribe views to all of
      a target's windows at creation) and aggressive-resize (the actor sizes to its
      current viewer set). Both now straightforward on the winlink + view-set base.
    **P3.0 — reader-ownership spike (DONE, `.tmp/spike5.go`).** Gate: `watchPane`
    posts a pane's output to its *creating* session's `s.events`; once session B
    shares a window A created, the readers still route through A, so A ending
    freezes the window for B. **Finding — the window actor owns its readers**
    (reader posts to the actor, no session in the path), so flow is
    session-independent. Two scenarios, `-race` clean ×3:
    - *Freeze gate:* a viewing session dies mid-flood (channel fills, then
      unsubscribes); the reader never blocks — non-blocking fan-out (spike4) drops
      the dead view's frames, the survivor gets every output. No freeze.
    - *Migration via origin-relay:* the reader always posts to its pane's **origin**
      actor; on break/join the origin forwards that pane's output to the new actor,
      coordinated on the origin's goroutine (FIFO drain). One ordered path
      reader→O→Y → strictly monotonic at the target: no loss, no reorder. This
      **replaces** the session-routing added for the migration fix (which froze on
      a shared window) with a window-lifetime-bound relay.
    - *Constraint for the build:* an origin actor must stay alive while it still
      relays a pane that migrated away, even if its own window is torn down
      (bounded — only migrated panes; teardown tracks "still relaying?").
- **Session groups** (`new-session -t`) — **DONE.** The new session borrows the
  target's window actors as a *snapshot* at creation: `groupJoinRequest` asks the
  target for its actor list, the joiner `subscribeView`s each (no own window:
  `first == nil`, so startup skips its own `newWindow`/`watchPane`/`initWindowBorder`).
  Same live actors → output in one shows in both. ponytail: snapshot only — tmux
  keeps the window *list* synced (new-window in one appears in all); that needs a
  shared winlink set, deferred. `new-session -t <nonexistent>` degrades to a normal
  session. Test: `e2e/group_test.go` `TestSessionGroup`.
- **aggressive-resize** — **DONE.** The window actor now owns its grid size,
  computed from its viewers instead of each session unilaterally resizing it (which
  made shared windows fight, last-writer-wins). Each `view` carries its session's
  size vote (its `effectiveSize`) + `aggressive` flag + a `seq` stamp; the actor's
  `recomputeSize` combines the qualifying votes per `window-size`
  (smallest/largest/latest/manual) and resizes, telling the OTHER viewers via
  `windowResized` (the initiator full-syncs itself). `aggressive-resize on` drops a
  view from the combine when its window isn't current in that session — so
  switching the current window elsewhere can resize a shared window (tmux's rule).
  Config: `aggressive-resize` (bool, default off) + runtime `set-option`. Sessions
  cache each window's real grid (`winGrid`) so `#{window_width}` reads it lock-free
  (an actorDo per status tick stalled on a flooding actor). ponytail ceilings: the
  actor's combine policy is last-writer across sessions with differing `window-size`;
  cross-session `latest` uses receipt-order recency. Tests: `e2e/resize_shared_test.go`
  `TestSharedWindowSmallest`, `TestAggressiveResize`.

  Pre-existing: `TestKill{Window,Session}UnderFlood` flake under `-count=8` stress
  (kill command on `s.events` starved by the 200k-line flood on `renderCh`); both
  pass at `-count=1`. Not from this work — the sibling I didn't touch flakes too.
4. **Session references windows by winlink** (a slice of actor refs + per-session
   current index), not by owning `[]*window`. A window can now appear in >1
   session's winlinks → **linked windows**.
5. **Session groups** (`new-session -t`): the new session shares the target's
   winlink set; each keeps its own current window + clients.
6. **aggressive-resize**: the actor knows its viewer set (spike's `wViewers`), so
   it sizes to the clients actually displaying it.

Teardown discipline (Phase 0 finding) is threaded throughout: clients
sub/unsub views with actor-confirmed removal; window destruction drains readers
via a barrier before closing.
