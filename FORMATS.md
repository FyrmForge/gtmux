# Format language

gtmux's status/info formats are tmux's `#{...}` language, with **one deliberate
divergence**: shell substitution declares its side. Everything else aims at
faithful tmux parity.

## The shared core — `internal/format`

`format.Expand(str, vars)` owns the pure, side-effect-free subset, used by both
the client status bar and the server's info commands (`display-message`,
`list-panes`, `list-windows`, …):

| form | meaning |
|------|---------|
| `##` | a literal `#` |
| `#{var}` | a variable from the vars map (`""` if absent) |
| `#{?var,a,b}` | `a` if `var` is non-empty else `b`; branches may nest `#{...}` |
| `#{b:var}` | basename of `var` |
| `#{d:var}` | dirname of `var` |
| `#{=N:var}` | truncate to first `N` chars; `#{=-N:var}` last `N` — modifiers stack (`#{=10:b:var}`) |

This package **runs no shell, by design**. Unknown `#x(...)` sequences are left
untouched for the caller's own expander (see below). Truncate is by byte, not
display width — fine while vars are ASCII.

## The divergence — shell substitution declares its side

tmux is a single process, so its `#()` always runs the shell wherever the tmux
server is. gtmux is **two processes** (a client attaching to a persistent
daemon — potentially over SSH, on a *different host*). "Run a shell command in a
format" is therefore ambiguous: the client's shell/cwd/env, or the server's?

gtmux resolves it by making the side explicit:

| form | runs where | supplied by |
|------|-----------|-------------|
| `#client(cmd)` | on the **attached client** (`sh -c cmd`), first line, cached | `internal/client/statusexpander.go` |
| `#server(cmd)` | on the **server host**, first line, cached | `internal/server/status.go` |
| `#(cmd)` | bare form — **no side declared, ignored** (body consumed) | — |

The bare `#()` is intentionally a no-op rather than a guessed default: a config
that means "run on the server" and one that means "run on the client" must not
collapse to the same string. tmux users porting a config get a visible blank,
not a silently-wrong host.

## Data flow for `#server()`

The client owns the status formats (they're a client option); the server never
sees the format strings. So the two sides cooperate:

1. On attach, the client scans its `status_left`/`status_right` for `#server(cmd)`
   bodies (`extractServerCmds`) and sends the deduplicated list to the server.
2. Each status tick the server runs those commands (`serverShell.run`), caching
   each result for the status interval, and streams the `cmd → output` map back
   in `StatusInfo.ServerShell`.
3. The client expands its formats locally, looking `#server(cmd)` up in that map
   (`statusExpander.expand`). `#client(cmd)` it runs itself, cached the same way.

As more clients attach, the server's `#server()` command set grows as a union
(deduplicated), so N clients asking for the same command run it once.

## Caching

Both sides cache each command's first line for the status interval (the
`#server()` cache lives server-side, the `#client()` cache client-side). Client
shell runs are synchronous on the decode goroutine — fine for subsecond
commands; would need to go async if a slow one stalls redraws.

## Deferred

- `#{t:...}` formats a unix-seconds var (`session_created` is the first such
  var) with a fixed ANSIC layout — tmux's `#{t:...;strftime-spec}` custom layout
  is not parsed.
- Truncate now counts runes, not bytes; full wcwidth (double-width CJK) is still
  approximated as one column per rune.
- `#client()`/`#server()` outside the status bar (info commands use only the
  pure core today).
