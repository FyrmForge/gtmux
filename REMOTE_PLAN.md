# Remote sessions over ssh — plan (2026-08-29)

## Decisions (agreed)
1. Transport: `ssh host gtmux proxy` subprocess; its stdin/stdout is the conn.
2. Naming: `[user@]host:session` (scp-style); ssh_config is the alias layer.
3. Liveness: gob Ping/Pong every 2s (RTT shown, >1s = "lagging", >10s = reconnect)
   + ssh `-o ServerAliveInterval=5 -o ServerAliveCountMax=3`.
4. Outage UX: keep last frame, status shows "reconnecting host… Ns", backoff 1→10s
   forever until detach key, typed input dropped with a hint. Local dial keeps failing fast.
5. Mixing: one daemon at a time; switch-session target re-resolves transport per loop.
6. Bootstrap: remote install required; `gtmux deploy host [--config]` scp's local
   binary (+server.lua). Proto handshake line `gtmux-proto <N>` before gob.
7. Attach: Cwd empty for remote; Env comes from proxy's own env (first gob msg);
   ssh spawned with -A.
8. Bandwidth: ssh -C; existing diffs/coalescing are enough.
9. ssh flags fixed in code (-A -C -T ServerAlive*); anything else via ~/.ssh/config.
10. Tests: GTMUX_SSH=<cmd> overrides the ssh binary; e2e uses a script that execs
    `gtmux proxy` locally.

## Changes by file
- internal/proto/proto.go
  - `const ProtoVersion = 1`
  - `ClientMsg.Ping *Ping{Seq}`, `ServerMsg.Pong *Pong{Seq}`
  - `ClientMsg.ProxyEnv map[string]string`
  - `ParseTarget(s) (host, session string)` — split on first ':' unless no '@'/'.' heuristics; plain names stay local.
- internal/client/transport.go (new)
  - `dial(target) (io.ReadWriteCloser, error)`: local → unix socket (+ensureServer);
    remote → exec `$GTMUX_SSH|ssh -A -C -T -o ServerAlive… host gtmux proxy`,
    read+check handshake line, return pipe wrapper whose Close kills ssh.
- internal/client/client.go
  - reconnect loop uses `dial(target)`; remote: backoff retry, overlay message,
    drop input while disconnected, Cwd/Env omitted.
  - ping ticker + RTT tracking; expose `#{client_latency}` for status formats.
- internal/server/server.go
  - handle Ping → Pong; ProxyEnv → stash as update-environment source for the
    following Attach on that conn.
- cmd/gtmux/main.go
  - `proxy`: ensureServer(), write handshake line, send ProxyEnv, io.Copy both ways.
  - `deploy host [--config]`: scp self → ~/.local/bin/gtmux (+ server.lua).
  - attach/new/switch accept host:session.
- internal/e2e/remote_test.go (new): fake-ssh script, attach/reconnect/ping tests.

## Skipped (add when it bites)
- merged cross-host session picker
- auto self-deploy / cross-compile
- gtmux-side host aliases
