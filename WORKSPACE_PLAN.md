# Workspaces (workspacer absorbed into gtmux) — plan (2026-11-06)

Supersedes the workspacer half of PLUGIN_PLAN.md (Lua plugin + external
workspacer binary). Workspaces are now first-party: a `gtmux workspace`
subcommand backed by a daemon-side actor. GitHub users/orgs are first-class
"accounts"; other providers later.

## Decisions (agreed)

1. Native Go in gtmux core. No Charm, go-github, githubv4 or oauth2 deps:
   GitHub via stdlib `net/http` (REST), pickers via gtmux's own Lua UI.
2. Server side: one actor goroutine per workspace in the daemon, same pattern
   as `windowActor` (internal/server/actor.go): sole owner of its state, fed via
   channels, no mutexes. Owns the remote repo list and per-project git info.
3. Background refresh, low resource:
   - Warm at daemon boot: load cache file, then refresh in the background.
   - Ticker always on (default 15 min, per account), attached clients or not.
   - Incremental: repos sorted by pushed desc, stop paging at the first
     unchanged page; ETag on every page (304 = free, no rate-limit hit).
   - Full sweep every few hours to catch deletes/renames.
   - Git branch/status lazy: only for open sessions and visible picker rows.
   - Cache file written only on change. It's a warm-start snapshot; daemon
     memory is the source of truth. New format, no compat with
     `.workspacer-cache.json`. Tokens never written to it.
4. Config lives in the server Lua config, all under `gtmux.workspace.*`.
   Layouts are plain Lua values (no preset directive, no name registry); a
   layout may also be a function taking the project and returning a table.
5. Auth per account via built-in helpers that return Go-side values (the
   actor resolves tokens on its own goroutine, never calls into the Lua
   state — a slow `pass`/pinentry must not stall hooks/status):
   - `ws.ghcli_auth{ user = ... }` → `gh auth token [--user U] [--hostname H]`
   - `ws.env_auth("VAR")` → daemon env (fixed at daemon start)
   - `ws.cmd_auth("shell cmd")` → stdout is the token
   - No literal tokens in config. Token cached in memory, re-resolved on 401.
6. Accounts carry optional `provider` (default "github") and `host`
   (default "github.com") so GHE / other providers fit later.
7. Cloning: SSH, like workspacer (`git@<host>:<login>/<repo>.git`). Optional
   per-account `ssh_host` alias (`git@github-work:...`) so `~/.ssh/config`
   picks the key for multi-identity setups.
8. Session names are `<prefix>/<project>` (workspacer used `-`). Workspace membership
   is stored as session metadata, never parsed from the name.
9. CLI (alias `gtmux ws`):
   ```
   gtmux workspace                     # picker, current/default workspace
   gtmux workspace <ws>                # picker for <ws>
   gtmux workspace <ws> <project>      # open/switch (clone if remote-only)
   gtmux workspace refresh [<ws>]      # force refresh now
   ```
   "Current" comes from the session you're in (replaces `-W=current`);
   otherwise the workspace with `default = true`, else the first `ws.add`.
   `refresh` is a reserved workspace name. Inside gtmux: switch-client;
   outside: attach (fixes workspacer's nested-client bug).
10. Picker: a Lua component in default_client.lua like `choose_tree` /
    `find_session`, user-overridable. Data from `gtmux.workspace.projects(ws)`
    (served from the actor's cache), updates live as pages arrive, fuzzy
    filter replaces workspacer's `search`. Order: open sessions first, then
    A-Z (changed 2026-11-20 from push date, which read as random when
    scanning names; temp stays newest first).
11. Outside gtmux: client draws a blank canvas in raw mode, shows the picker
    on it, and on Enter hands off to a normal attach in the same process
    (creating the session first). Esc returns to the shell, nothing created.
12. Folder layout: `<root>/<workspace>/<project>`. Global `ws.root`
    (default `~/Projects`); a workspace's folder is `<root>/<name>`, with an
    optional per-workspace `path` override. Moving existing repos is manual.
13. Remote daemon (REMOTE_PLAN.md, not built yet): auth helpers and clones run
    on the daemon's host — projects live where the daemon lives.

## Config shape

```lua
local ws = gtmux.workspace

ws.root = "~/Projects"

local dev = {
  {
    name = "edit",
    panes = {
      { cmd = "nvim" },
    },
  },
  {
    name = "shell",
    panes = {
      {},
      {
        split = "h",
        size = 30,
      },
    },
  },
}

ws.account{
  name = "me",
  kind = "user",
  login = "darthvader",
  auth = ws.ghcli_auth{
    user = "darthvader",
  },
}

ws.account{
  name = "work",
  kind = "org",
  login = "acme-corp",
  auth = ws.cmd_auth("pass show work/gh"),
  ssh_host = "github-work",
}

ws.add{
  name = "work",
  account = "work",
  prefix = "work",
  layout = dev,              -- folder: ~/Projects/work (path = ... to override)
  env = {
    GIT_AUTHOR_EMAIL = "me@acme.com",
  },
  projects = {
    api = {
      layout = dev,
      sisters = {
        "api-protos",
      },
    },
  },
}
```

## Feature scope vs workspacer

- Port: project picker (local + remote, clone on pick), layouts, sister repos.
- Folded: cache → daemon actor; `search` → picker fuzzy filter;
  `.workspace.env` files → `env` field on the workspace.
- Deferred: `new` wizard (write-side API/auth), `actions` (later as a status
  format variable), HTTPS clone with account token, custom Lua functions as
  `auth`, named layouts / `--layout` (a `layouts = {...}` table would do).
- Dropped: usage tracking, `stats`.

## Parked (user has design thoughts — revisit in this order)

1. ~~Temp workspaces~~ — built, see "Temp workspaces" below.
2. Time tracking (hours/day). Old usage approach is broken by Claude RC
   sessions; needs a rethink.

## Build order

1. Investigate client/session coupling: can the compositor + Lua component API
   (`ui:*`, `gtmux.*`) run with no attached session (decision 11)? Small
   pre-attach loop vs splitting overlay code from session code.
2. Config: `gtmux.workspace.{account,add,ghcli_auth,env_auth,cmd_auth}` in the
   server Lua state → validated Go specs (internal/config/server.go).
3. Server: workspace actor — cache load, ticker, incremental GitHub fetch with
   ETags, full sweep, cache write on change, lazy git info.
4. Proto: list projects (+ live updates), refresh, open (clone if needed,
   create session with layout/sisters/env, tag workspace metadata).
5. CLI: `gtmux workspace` / `ws` in cmd/gtmux/main.go.
6. Client picker component (in-session overlay).
7. Outside-gtmux blank-canvas picker → attach handoff.
8. Mark the "Workspacer replacement" TODO.md item as tracked here.

## Status (2026-11-06): built

Code: `internal/workspace/` (config + Lua API, GitHub fetch, actor, cache,
spec/clone), `internal/server/workspace.go` (session build, one-shot handler),
`workspace-open` command (session.go), client Lua `gtmux.workspace.*`
(config/client.go), picker in default_client.lua (prefix+P),
`internal/client/workspace.go` (CLI + standalone picker), `gtmux workspace|ws`.
Tests: `internal/workspace/workspace_test.go`, `internal/e2e/workspace_test.go`.

Deviations / additions vs the decisions above:
- No server push stream for live rows: the picker re-queries the daemon's
  cache on each render (modals re-render on the 1s status tick).
- Inside gtmux, `gtmux ws` draws the standalone picker in the pane, then the
  daemon builds + switches the acting client (`workspace-open`). prefix+P is
  the overlay version.
- Session build reuses server commands (workspacer's sequence): env via
  set-environment + respawn of the first pane; pane `cmd`s are typed
  (send-keys), so quitting them leaves a shell.
- `respawn-pane` now applies the session/global env like a fresh spawn (it
  ignored set-environment before; tmux applies it).
- Clone runs in the daemon: BatchMode ssh, no terminal prompts, 5 min
  timeout, requester's SSH_AUTH_SOCK passed through.
- Incremental refresh stops on push time vs a committed high-water mark (not
  "first unchanged page"); ETag/HWM only commit on a clean finish.
- `kind = "user"` lists via /user/repos?affiliation=owner: private repos
  need that user's own token.
- Window fields: `name`, `dir` (relative = under the project), `panes`,
  `select_layout`. Pane fields: `cmd`, `split = "h"|"v"`, `size` (percent).
  Sisters: `"name"` or `{ name, label, layout }`. Account `refresh` (seconds).
- Deferred: per-project git branch/status in the picker; picker "root" entry.

Known limits (review round, 2026-11-06):
- A repo that appears with an OLD push date (transferred in, newly shared)
  sits below the high-water mark: incremental refreshes miss it until the
  next full sweep (≤6h). `gtmux ws refresh` / Ctrl-R in the picker forces one.
- Layout functions must only return a table: `gtmux.set_option/set_hook`
  raise when called after load (they'd race the live daemon's config).
- `respawn-pane` in a shared (linked) window uses the creating session's
  env — same as split-window already does there.

## `new` wizard — plan (2026-11-07, agreed point by point)

Decisions:
1. Built-in scaffold is only: folder, `git init`. Anything else comes from
   per-workspace `templates` (plain Lua list, shared via locals):
   `{ name = "rust", init = function(p) return { "cargo init" } end }`.
   `p` = layout-function fields + `login`. Commands run inside the new folder
   (use in-place forms; tools that insist on their own subfolder move files up
   themselves).
2. GitHub repo created via the account's existing `auth` (no write_auth).
   POST /orgs/{login}/repos (org) or /user/repos (user), empty
   (auto_init=false). 403/404 → "token for account X can't create repos",
   stop. Order: remote first (fail = nothing local), then folder + git init +
   remote add. Runs in the daemon. New repo injected into the actor's cache.
   No account → GitHub step skipped, local only.
3. Template commands run in the new session's FIRST PANE (typed, like layout
   cmds), chained with `&&`: `<init cmds> && git add -A && git commit -m
   "Initial commit" [&& git push -u origin HEAD]`. Interactive scaffolders and
   their own wizards (hamr new) work; cancel/fail = nothing committed; push
   uses the user's shell SSH agent.
4. Plain `git init` (user's init.defaultBranch), push `-u origin HEAD`.
5. Wizard = Lua component (overlay inside gtmux, blank canvas outside), steps:
   name → template (≥2 only) → GitHub? (account only) → private? (GitHub
   only) → summary. Esc = back (closes on step 1), C-c closes. Name check:
   [A-Za-z0-9._-], not an existing folder, not in the cached remote list.
6. Entry: picker C-n; picker Enter on a filter with no match offers
   "create <text>"; `gtmux ws new [<ws>] [<name>]` (prefilled wizard);
   `gtmux ws new <ws> <name> --template T --gh --private` (no wizard).
   `new` joins `refresh` as a reserved workspace name.

Small calls (taken without asking):
- With no template commands (or no templates), the daemon writes
  `README.md` (`# <name>`) so the first commit isn't empty; templates own
  their files (create-react-app refuses non-empty dirs).
- Commit message "Initial commit".

Build:
1. config.go: parse `templates` (name + optional init fn), reserve `new`.
2. github.go: createRepo (POST, 201 → Repo; 403/404/422 → clear errors).
3. manager.go: `Create(ws, name, template, gh, private, env)` → validate,
   remote, mkdir, git init, remote add, README if no cmds, inject into
   actor cache (new `add` channel), return SessionSpec with the chain typed
   into window 1 pane 1; that pane's own layout cmd (e.g. nvim) is appended
   with `;` so it still starts after the scaffold, pass or fail.
4. proto + server/workspace.go: op "new"; reply carries templates + whether
   the workspace has an account; `workspace-new` session command (async,
   epoch handoff like workspace-open).
5. client: `gtmux.workspace.new(...)` BindOp; projects() reply exposes
   `templates` and `account`; wizard component + picker hooks in
   default_client.lua; CLI `ws new` with flags in internal/client/workspace.go.
6. Tests: unit (template parse, createRepo against httptest, Create local
   path + README rule), e2e (wizard local-only with a template that echoes,
   commit present).

Status (2026-11-07): `new` built. `Manager.Create` (manager.go), `createRepo`
(github.go), `templates` (config.go), op "new" + `workspace-new` command,
`gtmux.workspace.create`, wizard inside the picker component
(default_client.lua; picker key C-o — C-n stays "down"), `gtmux ws new`.
Tests: TestCreateLocal, TestCreateGitHub, e2e TestWorkspaceNew.
- `--template` omitted = the workspace's first template (declare an
  `{ name = "empty" }` first to default to nothing).
- A repo created mid-sweep is marked seen, so the sweep can't delete it.
- `kind = "user"`: GET /user must match `login` before the POST (the repo is
  created under the token's owner).

Review round (2026-11-07): every local check (name, template, layout,
existing session) runs before the remote is created; a later local failure
removes the folder and names the remote (never deleted — needs delete_repo).
Names: GitHub charset, no leading dot, ≤100. Template names unique; init
entries: empty dropped, newlines refused, `;`/`#` documented. CLI: flags need
<ws> and <name>, `--private` needs `--gh`, extra args refused.

## Temp workspaces — plan (2026-11-07, agreed point by point)

Decisions:
1. `gtmux temp [<name>]` (alias `gtmux t`). Folder `$TMPDIR/gtmux-<uid>/<name>`
   (0700; refuse if the root isn't ours or is a symlink). Session
   `tmp-<name>`, `@workspace = temp`. No name = lowest free number (1, 2, …).
   Existing: switch, or rebuild the session if only the folder is left.
   Name rules = project names.
2. Lifetime: no auto-delete; folder lives until /tmp is wiped (reboot).
   `gtmux temp rm <name>` (refused while its session is open). No rm-all.
3. Picker: "temp" is a built-in pseudo-workspace in the existing picker (Tab
   cycles to it; `gtmux ws temp` opens on it). Rows = temp folders, ● open,
   newest first. Enter opens; Enter on a no-match filter creates a temp with
   that name; C-o = next numbered temp; C-d = delete (confirm dialog, refused
   while open). `temp` is a reserved workspace name. Works with no
   workspaces configured (temp is the only entry).
4. Optional `gtmux.workspace.temp{ layout = ..., env = {...} }` in server.lua;
   default one plain window. Layout functions get workspace = "temp".
5. No promotion (mv + `gtmux ws <ws> <name>` by hand).

Build:
1. workspace/config.go: `ws.temp{}` → Config.Temp; reserve "temp"; Finish
   keeps a config alive when only temp is declared.
2. manager.go: Manager always has a temp workspace (nil config ok); Get
   ("temp"); Projects lists temp dirs by mtime; Prepare mkdirs a missing temp
   (no clone); NextTemp (lowest free); RemoveTemp; root safety check.
3. server: Names() includes "temp"; workspace-open temp "" = next number
   (under a lock); `workspace-temp-rm <name>` command + op "temp-rm"
   (refused while open).
4. client: `gtmux.workspace.remove_temp(name)` (confirm in Lua via
   gtmux.confirm_before); picker temp mode keys; CLI `temp`/`t`/`temp rm`.
5. Tests: unit (temp dirs, next number, rm, root check), e2e (CLI temp,
   picker C-o, rm refused while open).

Status (2026-11-07): temp built. config.go (`ws.temp{}`, TempRoot, temp(),
Get/Names), manager.go (temp branch in Prepare, ensureTempRoot, NextTemp,
RemoveTemp, mtime rows), server (next number under a lock, removeTemp, op
"temp-rm", `workspace-temp-rm`), client (`gtmux.workspace.remove_temp`,
picker temp keys, `gtmux temp|t [rm]`). Tests: TestTemp, e2e TestWorkspaceTemp.
- Delete confirm is a y/n line inside the picker (not confirm_before): it has
  to work on the blank-screen picker outside gtmux too.
- "Open" check for rm = a session named tmp-<name> exists (any tag).

Review round (Opus + Codex, 2026-11-08), all fixed: `ws new` on temp refused
(daemon panic with no config); temp-rm takes the open lock; "open" also means
any session tagged `@workspace=temp` + `@project=<name>` (renames), and
`workspace-temp-rm` went async (it queries every session); NextTemp skips
numbers with an open session; names can't start with "-"; workspace prefix
"tmp"/"tmp-*" rejected; a second `ws.temp{}` replaces; existing non-dir temp
entry refused; picker Down on an empty list no longer selects row 0. Not done:
symlink-swap of the temp root itself (needs a non-sticky shared $TMPDIR).

## Folders (2026-11-17, agreed)

`gtmux.workspace.folder{ name, path, layout?, env? }`: a single folder opened
as one session named `<name>` (notes, dotfiles), tagged `@workspace=folders`.
Picker: built-in `folders` entry in the Tab cycle (before `temp`), one row per
folder; no create/wizard. CLI: `gtmux ws folders <name>`. `folders` is a
reserved workspace name. Missing path = error (no mkdir).

Review round (2026-11-18), all fixed: opening the project you're in is a
no-op (finishSwitch ignores a self-switch); an already-open session is
switched to before Prepare (a moved folder or broken layout no longer blocks
it); sessions are created with their env (no set-environment + respawn), and
respawn-pane honours default-command (shared paneCmd); load-time-only setters
checked inside the real closures (aliases covered); `gtmux ws`/`temp` find
their session by $GTMUX_PANE after a rename; dead `workspace-temp-rm` command
removed; workspaceQuery reuses oneShot; repo-list actors are per account
(cache: ~/.cache/gtmux/accounts/<account>.json).

## Home session (2026-11-19, agreed)

`ws.add{ home = "root" | "<folder>" }`: a pinned "⌂ home" picker row (project
token `@home`), session `<prefix>-home`, in the workspace folder itself or a
folder under it (cloned if only on the remote, else created; hidden from the
normal rows). Uses the workspace layout. CLI `gtmux ws home [<ws>]`; `home` is
a reserved workspace name. Claude Code access to sibling repos from a folder
home is the user's `.claude/settings.json` (`additionalDirectories`), not
gtmux's. Reusing an untagged session (hand-made, workspacer) tags it.
