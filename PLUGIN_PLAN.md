# Plugin system + workspacer integration — plan (2026-08-31)

Context: workspacer (JamesTiberiusKirk/workspacer) already drives gtmux via its
`SessionBackend` CLI shim (`workspacer/gtmux_backend.go`), but its Attach nests a
second client when launched from inside gtmux. Goal: an in-gtmux project picker
that switch-clients properly, plus daemon-side metadata — delivered through a
minimal plugin system rather than baking workspacer into gtmux core.

## Decisions (agreed)

1. Shape: workspacer integration is Lua, not Go. Two halves: a client module
   (picker UI) and a server module (metadata). gtmux core stays domain-free.
2. Server advantage: the daemon owns metadata. Usage tracking via existing hooks
   (session-created, client-session-changed, client-attached); git/github info
   refreshed in the background on an interval. Writes workspacer's existing
   `.workspacer-cache.json` format so the standalone CLI and the gtmux picker
   share identical data — no migration.
3. Config authority: workspacer's yaml stays the source of truth. The Lua picker
   is dumb UI — it shells out to the `workspacer` binary for the actual session
   create (needs a small create-without-attach flag in workspacer), then
   `gtmux.switch_session`. No preset duplication in Lua.
4. Plugin system scope: `package.path` + convention, nothing more.
   - Add `~/.config/gtmux/plugins` (or the sync dir below) to `package.path`
     (`?.lua` and `?/init.lua`) in both client and server Lua states.
   - Convention = the sidebar pattern: a plugin module returns a setup function
     taking an opts table, using the public `gtmux.*` API.
   - YAGNI until a second out-of-tree plugin exists: API stability guarantees,
     registry, sandboxing, dependency resolution, lockfiles.
5. Distribution: tpm-style git repos.
   - `gtmux.plugin("owner/repo", { ref = "v0.1" })` in config registers the repo
     and adds its checkout to `package.path`.
   - `gtmux plugin sync` subcommand: system git clone/fetch into
     `~/.local/share/gtmux/plugins/<owner>--<repo>`, checkout the pinned ref.
     ~100 lines of Go.
   - No auto-clone at startup (no surprise network I/O in the daemon); missing
     plugin = one-line warning pointing at `plugin sync`.
   - Security story = the `ref` pin (updating is a deliberate config edit).
     Plugins are arbitrary Lua with io/os open — same trust level as one's own
     config; sandboxing is a non-goal.
6. Client/server + ssh remote (see REMOTE_PLAN.md): each Lua state loads
   plugins from its own filesystem.
   - A plugin repo may ship both halves; client config requires the client
     module, server config the server module.
   - `gtmux deploy host --config` extends to scp the server-declared plugin
     checkouts alongside server.lua — remote hosts need no git/network, pinning
     survives.
   - Server-plugin side effects (run_shell, io.popen) run on the daemon's host —
     correct for metadata (scan projects where they live).

## Open questions

- Remote + workspacer picker: a client-side picker shelling to the local
  `workspacer` binary scans local dirs even when attached to a remote daemon.
  Workspacer-design question (run the engine server-side? proxy through
  run_shell?), not a plugin-system one.
- How `gtmux.plugin()` declarations map to what `plugin sync` reads (parse both
  configs? a shared plugins.lua?).
- Whether the sync dir vs a manual `~/.config/gtmux/plugins` dir are both on
  `package.path`, or only the sync dir.
- workspacer's create-without-attach flag: exact CLI shape.

## Rough order

1. package.path wiring + convention doc (plugin system usable with manual clones).
2. `gtmux.plugin()` + `gtmux plugin sync`.
3. workspacer server module (hooks → cache json, background refresh).
4. workspacer client picker (+ create-without-attach flag in workspacer).
5. deploy --config carries server plugins (when remote lands).
