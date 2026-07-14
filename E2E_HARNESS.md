# Full-stack PTY e2e test harness

Drives a real `gtmux attach` client and asserts on its rendered screen,
deterministically and in isolation. Asserts on **observable behavior**
(keystroke → screen), not internal structure, so it survives architecture
refactors. (Build-out history in HISTORY.md.)

**Layer:** full-stack. The real `gtmux server` + `gtmux attach` run as
subprocesses; keystrokes go to the client's pty stdin; its stdout is parsed
into an `emu.Terminal` and assertions run against the rendered grid.

## Isolation

- `proto.SockPath()` honors a `GTMUX_SOCK` env override (mirrors tmux's
  `-S`). Both server and client call `SockPath()`, so both honor it.
- Each test gets its own temp socket under `t.TempDir()`; server + client are
  spawned with `GTMUX_SOCK` set to it.
- Subprocesses (not in-process) because the client needs a real pty **and**
  `kill-server` calls `os.Exit(0)` — in-process that would kill the test runner.
- Result: tests can't touch the real daemon, run in parallel, self-clean.
- The harness injects client/server Lua via an isolated `XDG_CONFIG_HOME`, so
  config-dependent features are testable too.

## Synchronization

- Client spawned on a pty of known size (default 80×24). A reader goroutine
  does `pty → emu.Terminal.Write()` under a mutex, maintaining a live grid.
  Reuses `emu` — no ANSI re-parsing. emu cells carry FG/BG, so **color** is
  assertable too (dot-fill, active-window highlight).
- `WaitFor(pred, timeout)` polls the grid every ~5ms until `pred` holds, else
  `t.Fatal` **dumping the current screen** (debuggable, unlike blind sleeps).
- Convention: every action is followed by a `WaitFor` on its visible effect.
  No naked sleeps.
- `WaitFor` handles appearance **and** disappearance, so time-based behavior
  works (run-shell auto-clear = wait present, then wait absent).
- Default timeout ~2s, overridable per-wait. Negative assertions ("nothing
  there") are avoided in favor of a positive marker; genuine disappearance
  uses `WaitFor` absent with a generous timeout.

## Test-author API

Flat model — common case is single-client.

- `Start(t) *Client` — server + one default client on an 80×24 pty; all
  cleanup via `t.Cleanup`.
- Input (encapsulates the finicky SGR/CSI encodings): `Type`, `TypeLine`,
  `Key(b)`, `Ctrl(b)`, `Prefix(keys…)`, `PrefixKey(Arrow/CtrlArrow…)`,
  `Mouse(cb,x,y,press)`.
- Observe: `Screen()` → `*Screen` with `Row(n)`, `Status()`, `Cell(r,c)`
  (incl. FG/BG), `String()`; plus `WaitFor`, `WaitForText`, `WaitForStatus`.
- Multi-client / CLI: `NewPeer(Size)` (second client on the same server, own
  pty+grid), `Resize(cols,rows)` (pty resize + SIGWINCH), `Run(session, args…)`
  (drives `gtmux run`/`list` subprocess, returns output).

Example:

```go
func TestSplitAndCopyMode(t *testing.T) {
    c := harness.Start(t)
    c.WaitForText("$")
    c.Prefix("c")
    c.WaitForStatus("2:")
    c.TypeLine("seq 1 100")
    c.Prefix("[")
    c.WaitFor(func(s *harness.Screen) bool { return s.Status().Has("104/104") })
}
```

## Build + layout

- The binary is built **once** into a temp path in `TestMain` (guarded by
  `sync.Once`); tests spawn that — current source, not a stale install.
- `internal/harness/` — helper package (`Start`, `Client`, `Screen`, input +
  observe). Plain, no build tag.
- `internal/e2e/` — the `*_test.go` scenarios, behind `//go:build e2e`. Plain
  `go test ./...` stays unit-only/fast; e2e runs explicitly:
  `go test -tags=e2e ./internal/e2e/`.

## Backends

The byte-in/grid-out seam is pluggable (`backend` interface in
`internal/harness/harness.go`): input funnels through one `write([]byte)`,
observation through one `snapshot()`. All scenarios run unchanged on every
backend, selected by test-binary flags (after the package path):

```
go test -tags=e2e ./internal/e2e/                          # pty (default)
go test -tags=e2e ./internal/e2e/ -backend=tmux            # tmux, headless
go test -tags=e2e ./internal/e2e/ -tmux-session ff-gtmux   # tmux, headed
go test -tags=e2e ./internal/e2e/ -run TestCopyMode \
    -tmux-window ff-gtmux:2 -slowmo 200ms -start-wait 2s   # tmux, takeover
```

- **pty** (default, `internal/harness/ptybackend.go`) — client on a
  `creack/pty` pty, output parsed by a live `emu.Terminal`. Fastest, fully
  hermetic.
- **`-backend=tmux`** (`internal/harness/tmuxbackend.go`) — client inside a
  real tmux pane on a private per-test tmux server (own socket in
  `t.TempDir()`), so a *foreign* terminal emulator sits between gtmux and the
  assertions — catches rendering bugs the emu-only path would round-trip.
  Input via `send-keys -H` (raw hex bytes, bypasses tmux key/mouse
  interpretation); grid via `capture-pane -e` re-parsed through a fresh
  `emu.Terminal`, so color assertions still work.
- **`-tmux-session <name>`** — headed: uses the *running* tmux server; each
  client becomes a new auto-focused window (forced to the test's exact size)
  in that session, killed when the test ends. Watch tests render live.
- **`-tmux-window <session:window>`** — headed, stable: takes over one
  designated window instead of creating them. While in use it's renamed
  `E2E ▷ <TestName>` and resized; on cleanup the pane gets its shell back and
  name/sizing options are restored exactly. `remain-on-exit` guards the window
  during the run so a dying client can't kill it. Extra clients in
  multi-client tests fall back to temporary windows in the same session.

Pacing flags (any backend, Go durations): `-slowmo` pauses after every input
action (Playwright-style), `-start-wait` pauses once per run, after the first
test's client attaches and before it acts — time to find the window.

Known wart: `set -g window-size manual` in a config file crashes the tmux
3.6a server at startup, so the private-server backend sets it per-session
after `new-session` instead.

## Live resize

`Client.Resize(cols, rows)` is the third backend method: pty delivers a real
SIGWINCH via `pty.Setsize` (and resizes the observing emu grid); tmux uses
`resize-window`. Scenarios live in `internal/e2e/resize_test.go` (kept out of
`matrix_test.go` on purpose — parallel work lands there): grow/shrink redraw,
proportional split re-fit, live multi-client renegotiation (dot-fill clears
when the acting peer grows), tiny-terminal (20×6) survival, and split math at
odd sizes (79×23). Deliberately not a size sweep — beyond the tiny/odd cases,
more sizes re-run the same code paths; add a specific size only when a real
bug names one.
