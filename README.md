# gtmux

A Go port of tmux's client-server architecture. One binary: `gtmux server`
runs a persistent daemon, `gtmux attach` connects over a Unix socket,
`gtmux run` scripts it. Config is Lua (`server.lua` / `client.lua`), not a
tmux.conf dialect.

Architecture in one line: a per-session owner goroutine holds all state
(window/pane tree, terminal grids); PTY-reader and client-connection
goroutines feed it via channels — gob-encoded messages, no mutexes. The
client owns all input interpretation (prefix, keybinds, copy-mode, overlays,
mouse); the server exposes actions and holds state.

## Docs

- **PARITY.md** — feature status vs real tmux, tier by tier.
- **TODO.md** — what's next: remaining parity gaps + gtmux's own features.
- **HISTORY.md** — completed design efforts (input-ownership refactor,
  runtime options, e2e harness build-out).
- **WINDOW_ACTORS.md** — in-progress redesign: windows as actor goroutines
  (unlocks linked windows / session groups / aggressive-resize).
- **FORMATS.md** — the `#{...}` format language, including the one deliberate
  tmux divergence (`#client()` / `#server()` shell sides).
- **E2E_HARNESS.md** — the full-stack pty test harness and its backends.

## Build & test

```
make install    # go install ./cmd/gtmux
go test ./...   # unit tests
go test -tags=e2e ./internal/e2e/   # full-stack e2e
```
