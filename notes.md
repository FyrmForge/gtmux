
pane resizing
  3 panes in a stack, top middle bottom
  focus is on top
  middle pane is min high that it can be
  bottom pane still has space
  whiles focused on top pane i cannot resise i to become taller like we can on tmux



# kill-pane focus restore

Focus should return to the previously focused pane, not the adjacent one.
Today `closePaneAt` (internal/server/session.go) delegates to
`windowActor.closePane` which picks an adjacent pane; `w.lastActive` is a
single slot (only serves `select-pane -l`) so history dies with the pane.

- Per-window focus **stack** (MRU), replacing `lastActive` (top-1 entry keeps
  `select-pane -l` behavior).
- Updated on every `setActive`; dead panes pruned lazily on pop.
- On kill: pop to most recent still-alive pane; adjacency as fallback when
  the stack is empty.

# other clankers

Current support: the Lua `agent = "claude codex opencode aider"` bell list in
internal/config/default_client.lua + `program-changed`/`command-exited` hooks.

- First-class agent awareness: per-pane state (busy / waiting-for-input /
  done), not just bell edges. Detection: foreground command match + bell +
  OSC title patterns per agent (Claude Code, Codex, opencode, aider,
  gemini-cli, amp).
- Exposed as a format var (`#{pane_agent_state}`) so status bar, borders and
  Lua hooks all see it.
- Config: Lua table of agent definitions (name-match, done-signal) instead of
  the flat string.

# focusable docks

Docks are client-side widgets (`textBox.dock` in internal/client/widget.go),
pure overlays today. Make them focus targets:

- Focusability is **per-dock config** on the dock definition: navigation,
  bind, both, or neither (default neither).
  - navigation: pane-nav at the window edge steps into the dock; same key
    steps back out.
  - bind: `gtmux.focus_dock(name)` toggle.
- While focused, keys route to the dock's `on_key` Lua handler instead of the
  pane; the dock handles its own internal focus/selection after that (e.g.
  the left dock moving through its session list).
- Purely client-side — server never learns about docks. Focused dock gets a
  border/style change.

# pane gaps → client-owned geometry

Guiding principle (agreed): **server = store of the arrangement tree + PTY
host; client = watches the tree, computes ALL geometry, reports sizes back.**
Gaps then fall out as one parameter of the client's tree walk.

## Model

- Server keeps the split tree (`layoutNode`: dir, frac, pane leaves) because
  arrangements must survive detach. It stops computing rects entirely.
- Protocol: server pushes the tree (with **stable node IDs**) instead of
  `Layout{Panes, Borders}`. Client walks it against its own terminal size —
  gaps, outer margin, border-status rows, zoom, dividers all client-side.
- Client sends back:
  - **sizes**: "pane %5 content is 78x22" → server resizes PTY + emu. Pure
    consequence of the walk.
  - **mutations** (already exist): split/kill/zoom/swap; border-drag becomes
    "set node <id> frac to 0.6" (replaces `ResizeBorder.Index`, which indexes
    a server border list that won't exist anymore).
- Mouse: client resolves everything, sends pane ID + pane-local coords for
  app-forwarding; server's `handleMouseEvent` rect math goes away.

## Rules decided

- Multi-client size fight (one PTY, one size): **last input wins** — the
  client you're typing in owns sizes.
- Detached session: server keeps last-reported sizes.

## Migration stages

1. Tree on the wire: add node IDs, push tree alongside existing Layout
   (client ignores it). Nothing breaks.
2. Client walk: port `layoutNode.apply` to the client; render from its own
   rects; keep sending nothing new (sizes still match server's since walk is
   identical). Verify pixel-identical.
3. Size report-back: client sends per-pane sizes; server's `apply` stops
   calling `pane.resize`, resizes only on report. Gate detached/multi-client
   rules here.
4. Kill server rects: drop `Layout{Panes, Borders}` push, `borderSeg`,
   border-status/zoom rect logic server-side; border-drag → node-ID frac.
   Mouse translation moves client-side.
5. **Gaps land here**: gap N + outer margin as client config in the walk.
   Also unlocks: per-client layouts, client-side layout presets.

Open ends: e2e harness drives rendering via server rects today — stage 2/4
touches it; copy-mode geometry (client already has rects, should follow).
