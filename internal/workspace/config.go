// Package workspace is gtmux's first-party project manager (WORKSPACE_PLAN.md):
// GitHub users/orgs as accounts, workspaces as folders of projects under one
// root, and layouts that build a session per project. Config comes from the
// server Lua VM (gtmux.workspace.*); a per-workspace actor keeps the remote repo
// list fresh in the background. No server/client imports: the server package
// executes the SessionSpec this package produces.
package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// Config is everything gtmux.workspace.* declared in server.lua.
type Config struct {
	Root       string
	Accounts   map[string]*Account
	Workspaces []*Workspace // declaration order: the first is the default unless one sets default=true
	Temp       *Workspace   // gtmux.workspace.temp{}; nil = defaults (see temp())
	Folders    []*Folder    // gtmux.workspace.folder{}: the "folders" pseudo-workspace

	// vm keeps the server Lua state alive for function layouts, which run at
	// open time. ponytail: one mutex around the whole VM — opens are rare and
	// human-paced; per-call VMs if a layout ever does real work.
	vmMu sync.Mutex
	vm   *lua.LState
	// frozen is set by Finish: the declarators then refuse (a layout function
	// calling ws.add at open time would race readers on other goroutines).
	frozen bool
}

// Account is one GitHub user or org (provider/host leave room for GHE etc.).
type Account struct {
	Name, Kind, Login string // Kind: "user" | "org"
	Provider, Host    string // default "github" / "github.com"
	SSHHost           string // clone host alias (~/.ssh/config picks the key); default Host
	Auth              Auth
	Refresh           time.Duration
	apiURL            string // tests point this at httptest; "" = derived from Host
}

// Auth is a token source resolved in Go on the actor's goroutine (never via
// the Lua VM: a slow `pass` must not stall anything else).
type Auth struct {
	Kind string // "ghcli" | "env" | "cmd"
	Arg  string // gh user / env var name / shell command
	Host string // ghcli --hostname (optional)
}

// Workspace is one gtmux.workspace.add{}.
type Workspace struct {
	Name, Prefix, Account, Path string
	Home                        string // "" none, "root" = Path itself, else a folder under Path
	Default                     bool
	Env                         map[string]string
	Layout                      Layout
	Projects                    map[string]*Project
	Templates                   []Template // `gtmux workspace new` scaffolds, in order
}

// Template is one `new` scaffold: init(p) returns shell commands typed into the
// new project's first pane (so interactive scaffolders work). nil init = none.
type Template struct {
	Name string
	init *lua.LFunction
}

// Folder is one gtmux.workspace.folder{}: a single folder opened as one
// session named after it (notes, dotfiles) — no project subfolders.
type Folder struct {
	Name, Path string
	Env        map[string]string
	Layout     Layout
}

// Project is a per-project override inside a workspace.
type Project struct {
	Layout  Layout
	Sisters []Sister
}

// Sister is a related repo opened as extra window(s) in the project's session.
type Sister struct {
	Name, Label string
	Layout      Layout
}

// Layout is either a static window list or a Lua function returning one.
type Layout struct {
	Windows []Window
	fn      *lua.LFunction
}

func (l Layout) empty() bool { return l.Windows == nil && l.fn == nil }

// Window/Pane are the session shape the server builds with its own commands.
type Window struct {
	Name, Dir    string
	SelectLayout string // optional select-layout name (e.g. "even-horizontal")
	Panes        []Pane
}

type Pane struct {
	Cmd   string // typed into the shell (send-keys), so quitting it leaves a shell
	Split string // "h" side-by-side, "v" (default) stacked
	Size  int    // percent of the window, 0 = leave as split
}

// Register installs gtmux.workspace on the server VM's gtmux table and returns
// the Config it fills as the file runs. Call Finish after the file has run.
func Register(L *lua.LState, gtmux *lua.LTable) *Config {
	cfg := &Config{Accounts: map[string]*Account{}, vm: L}
	ws := L.NewTable()
	// decl wraps a declarator so it refuses after Finish (checked in the real
	// function: aliases like `local add = gtmux.workspace.add` are covered).
	decl := func(fn lua.LGFunction) *lua.LFunction {
		return L.NewFunction(func(l *lua.LState) int {
			if cfg.frozen {
				l.RaiseError("gtmux.workspace.* declarations are load-time only (not from a layout or template function)")
			}
			return fn(l)
		})
	}
	authHelper := func(kind string) *lua.LFunction {
		return L.NewFunction(func(l *lua.LState) int {
			a := Auth{Kind: kind}
			if t, ok := l.Get(1).(*lua.LTable); ok { // ghcli_auth{user=, host=}
				a.Arg = lstr(t, "user")
				a.Host = lstr(t, "host")
			} else {
				a.Arg = l.OptString(1, "")
			}
			if kind != "ghcli" && a.Arg == "" {
				l.ArgError(1, kind+"_auth needs an argument")
			}
			ud := l.NewUserData()
			ud.Value = a
			l.Push(ud)
			return 1
		})
	}
	L.SetField(ws, "ghcli_auth", authHelper("ghcli"))
	L.SetField(ws, "env_auth", authHelper("env"))
	L.SetField(ws, "cmd_auth", authHelper("cmd"))
	L.SetField(ws, "account", decl(func(l *lua.LState) int {
		t := l.CheckTable(1)
		a := &Account{
			Name: lstr(t, "name"), Kind: lstr(t, "kind"), Login: lstr(t, "login"),
			Provider: lstr(t, "provider"), Host: lstr(t, "host"), SSHHost: lstr(t, "ssh_host"),
			Refresh: 15 * time.Minute,
		}
		if a.Name == "" || a.Login == "" {
			l.ArgError(1, "account needs name and login")
		}
		if a.Kind == "" {
			a.Kind = "user"
		}
		if a.Kind != "user" && a.Kind != "org" {
			l.ArgError(1, "account kind must be \"user\" or \"org\"")
		}
		if a.Provider == "" {
			a.Provider = "github"
		}
		if a.Provider != "github" {
			l.ArgError(1, "only provider \"github\" is supported")
		}
		if a.Host == "" {
			a.Host = "github.com"
		}
		if a.SSHHost == "" {
			a.SSHHost = a.Host
		}
		if n, ok := t.RawGetString("refresh").(lua.LNumber); ok && n > 0 {
			// Float seconds; floor at 1s (a 0 ticker would panic the daemon).
			a.Refresh = max(time.Second, time.Duration(float64(n)*float64(time.Second)))
		}
		switch v := t.RawGetString("auth").(type) {
		case *lua.LUserData:
			au, ok := v.Value.(Auth)
			if !ok {
				l.ArgError(1, "auth must come from ghcli_auth/env_auth/cmd_auth")
			}
			a.Auth = au
		case *lua.LNilType:
			a.Auth = Auth{Kind: "ghcli"}
		default:
			l.ArgError(1, "auth must come from ghcli_auth/env_auth/cmd_auth")
		}
		cfg.Accounts[a.Name] = a
		return 0
	}))
	L.SetField(ws, "add", decl(func(l *lua.LState) int {
		t := l.CheckTable(1)
		w := &Workspace{
			Name: lstr(t, "name"), Prefix: lstr(t, "prefix"), Account: lstr(t, "account"),
			Path: lstr(t, "path"), Env: map[string]string{}, Projects: map[string]*Project{},
		}
		if w.Name == "" {
			l.ArgError(1, "workspace needs a name")
		}
		if w.Name == "." || strings.Contains(w.Name, "..") || strings.ContainsAny(w.Name, "/\\") {
			l.ArgError(1, "workspace name must not contain /, \\ or ..")
		}
		if w.Name == "refresh" || w.Name == "new" || w.Name == "temp" || w.Name == "folders" || w.Name == "home" {
			l.ArgError(1, "\"refresh\", \"new\", \"temp\", \"folders\" and \"home\" are reserved")
		}
		if w.Prefix == "" {
			w.Prefix = w.Name
		}
		if strings.HasSuffix(w.Prefix, "/") {
			l.ArgError(1, "prefix must not end in / (sessions are <prefix>/<project>)")
		}
		if w.Prefix == "tmp" || strings.HasPrefix(w.Prefix, "tmp/") {
			l.ArgError(1, "prefix \"tmp\" is taken by temp workspaces (tmp/<name> sessions)")
		}
		w.Default = lua.LVAsBool(t.RawGetString("default"))
		if w.Home = lstr(t, "home"); w.Home != "" && w.Home != "root" && !validName.MatchString(strings.TrimPrefix(w.Home, ".")) {
			l.ArgError(1, "home must be \"root\" or a folder name (letters, digits, . _ -; may start with .)")
		}
		if et, ok := t.RawGetString("env").(*lua.LTable); ok {
			et.ForEach(func(k, v lua.LValue) { w.Env[k.String()] = v.String() })
		}
		w.Layout = toLayout(l, t.RawGetString("layout"))
		if tt, ok := t.RawGetString("templates").(*lua.LTable); ok {
			tt.ForEach(func(_, v lua.LValue) {
				vt, ok := v.(*lua.LTable)
				if !ok || lstr(vt, "name") == "" {
					l.ArgError(1, "templates entries need a name")
				}
				tp := Template{Name: lstr(vt, "name")}
				for _, o := range w.Templates {
					if o.Name == tp.Name {
						l.ArgError(1, "duplicate template name "+tp.Name)
					}
				}
				switch f := vt.RawGetString("init").(type) {
				case *lua.LFunction:
					tp.init = f
				case *lua.LNilType:
				default:
					l.ArgError(1, "template init must be a function")
				}
				w.Templates = append(w.Templates, tp)
			})
		}
		if pt, ok := t.RawGetString("projects").(*lua.LTable); ok {
			pt.ForEach(func(k, v lua.LValue) {
				p := &Project{}
				if vt, ok := v.(*lua.LTable); ok {
					p.Layout = toLayout(l, vt.RawGetString("layout"))
					if st, ok := vt.RawGetString("sisters").(*lua.LTable); ok {
						st.ForEach(func(_, sv lua.LValue) {
							switch s := sv.(type) {
							case lua.LString:
								p.Sisters = append(p.Sisters, Sister{Name: string(s)})
							case *lua.LTable:
								p.Sisters = append(p.Sisters, Sister{Name: lstr(s, "name"), Label: lstr(s, "label"),
									Layout: toLayout(l, s.RawGetString("layout"))})
							}
						})
					}
				}
				w.Projects[k.String()] = p
			})
		}
		for i, o := range cfg.Workspaces {
			if o.Name == w.Name { // re-adding replaces (config reloads layer files)
				cfg.Workspaces[i] = w
				return 0
			}
		}
		cfg.Workspaces = append(cfg.Workspaces, w)
		return 0
	}))
	// gtmux.workspace.temp{ layout = ..., env = {...} }: how temp sessions look.
	L.SetField(ws, "temp", decl(func(l *lua.LState) int {
		t := l.CheckTable(1)
		cfg.Temp = nil // a second call replaces, like ws.add
		w := cfg.temp()
		if et, ok := t.RawGetString("env").(*lua.LTable); ok {
			et.ForEach(func(k, v lua.LValue) { w.Env[k.String()] = v.String() })
		}
		w.Layout = toLayout(l, t.RawGetString("layout"))
		cfg.Temp = w
		return 0
	}))
	// gtmux.workspace.folder{ name = "dots", path = "~/.dots", layout = ..., env = {...} }
	L.SetField(ws, "folder", decl(func(l *lua.LState) int {
		t := l.CheckTable(1)
		f := &Folder{Name: lstr(t, "name"), Path: lstr(t, "path"), Env: map[string]string{}}
		if !validName.MatchString(f.Name) {
			l.ArgError(1, "folder needs a name (letters, digits, . _ -; no leading dot or dash)")
		}
		if f.Path == "" {
			l.ArgError(1, "folder needs a path")
		}
		if et, ok := t.RawGetString("env").(*lua.LTable); ok {
			et.ForEach(func(k, v lua.LValue) { f.Env[k.String()] = v.String() })
		}
		f.Layout = toLayout(l, t.RawGetString("layout"))
		for i, o := range cfg.Folders {
			if o.Name == f.Name { // re-declaring replaces, like ws.add
				cfg.Folders[i] = f
				return 0
			}
		}
		cfg.Folders = append(cfg.Folders, f)
		return 0
	}))
	L.SetField(gtmux, "workspace", ws)
	return cfg
}

// TempRoot is where temp workspaces live: per-user, under the OS temp dir
// (wiped at reboot on most systems — that's the point).
func TempRoot() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("gtmux-%d", os.Getuid()))
}

// temp is the built-in "temp" workspace: the declared one, else defaults
// (one plain window). Nil-safe: temp works with no config at all.
func (c *Config) temp() *Workspace {
	if c != nil && c.Temp != nil {
		return c.Temp
	}
	return &Workspace{Name: "temp", Prefix: "tmp", Path: TempRoot(), Env: map[string]string{}, Projects: map[string]*Project{}}
}

// Finish reads ws.root (a plain field assignment), resolves workspace paths
// and checks account references. Returns nil when nothing was declared, so a
// daemon without workspaces keeps no VM and runs no actors.
func (c *Config) Finish(gtmux *lua.LTable) (*Config, error) {
	c.frozen = true
	if len(c.Workspaces) == 0 && c.Temp == nil && len(c.Folders) == 0 {
		return nil, nil
	}
	c.Root = "~/Projects"
	if t, ok := c.vm.GetField(gtmux, "workspace").(*lua.LTable); ok {
		if r := lstr(t, "root"); r != "" {
			c.Root = r
		}
	}
	c.Root = expandHome(c.Root)
	for _, w := range c.Workspaces {
		if w.Path == "" {
			w.Path = filepath.Join(c.Root, w.Name)
		}
		if w.Path = expandHome(w.Path); !filepath.IsAbs(w.Path) {
			w.Path = filepath.Join(c.Root, w.Path) // relative = under ws.root, not the daemon's cwd
		}
		if w.Account != "" && c.Accounts[w.Account] == nil {
			return nil, fmt.Errorf("workspace %q: unknown account %q", w.Name, w.Account)
		}
	}
	for _, f := range c.Folders {
		f.Path = expandHome(f.Path)
	}
	return c, nil
}

// Get resolves a workspace by name; "" picks default=true, else the first,
// else temp (which always exists).
func (c *Config) Get(name string) (*Workspace, error) {
	if name == "temp" || (name == "" && (c == nil || len(c.Workspaces) == 0)) {
		return c.temp(), nil
	}
	if name == "folders" && c != nil && len(c.Folders) > 0 {
		return &Workspace{Name: "folders", Env: map[string]string{}, Projects: map[string]*Project{}}, nil
	}
	if c == nil {
		return nil, fmt.Errorf("no such workspace: %s (have: temp)", name)
	}
	if name == "" {
		for _, w := range c.Workspaces {
			if w.Default {
				return w, nil
			}
		}
		return c.Workspaces[0], nil
	}
	for _, w := range c.Workspaces {
		if w.Name == name {
			return w, nil
		}
	}
	return nil, fmt.Errorf("no such workspace: %s (have: %s)", name, strings.Join(c.Names(), ", "))
}

// Names lists workspaces in declaration order, then "folders" (if any), then
// "temp".
func (c *Config) Names() []string {
	var out []string
	if c != nil {
		for _, w := range c.Workspaces {
			out = append(out, w.Name)
		}
		if len(c.Folders) > 0 {
			out = append(out, "folders")
		}
	}
	return append(out, "temp")
}

// toLayout converts a layout value at load time: tables become Go windows now;
// functions are kept and called per open (see Config.windows).
func toLayout(l *lua.LState, v lua.LValue) Layout {
	switch t := v.(type) {
	case *lua.LFunction:
		return Layout{fn: t}
	case *lua.LTable:
		return Layout{Windows: toWindows(t)}
	case *lua.LNilType:
		return Layout{}
	}
	l.RaiseError("layout must be a table or a function, got %s", v.Type())
	return Layout{}
}

func toWindows(t *lua.LTable) []Window {
	wins := []Window{}
	t.ForEach(func(_, v lua.LValue) {
		wt, ok := v.(*lua.LTable)
		if !ok {
			return
		}
		w := Window{Name: lstr(wt, "name"), Dir: lstr(wt, "dir"), SelectLayout: lstr(wt, "select_layout")}
		if pt, ok := wt.RawGetString("panes").(*lua.LTable); ok {
			pt.ForEach(func(_, pv lua.LValue) {
				p := Pane{}
				if ptt, ok := pv.(*lua.LTable); ok {
					p.Cmd, p.Split = lstr(ptt, "cmd"), lstr(ptt, "split")
					if n, ok := ptt.RawGetString("size").(lua.LNumber); ok {
						p.Size = int(n)
					}
				}
				w.Panes = append(w.Panes, p)
			})
		}
		wins = append(wins, w)
	})
	return wins
}

// windows materializes a layout for one project (calls a function layout on
// the kept VM, serialized).
func (c *Config) windows(l Layout, ws *Workspace, project, dir string) ([]Window, error) {
	if l.fn == nil {
		return l.Windows, nil
	}
	c.vmMu.Lock()
	defer c.vmMu.Unlock()
	arg := c.vm.NewTable()
	arg.RawSetString("name", lua.LString(project))
	arg.RawSetString("path", lua.LString(dir))
	arg.RawSetString("workspace", lua.LString(ws.Name))
	if err := c.vm.CallByParam(lua.P{Fn: l.fn, NRet: 1, Protect: true}, arg); err != nil {
		return nil, fmt.Errorf("layout function: %w", err)
	}
	ret := c.vm.Get(-1)
	c.vm.Pop(1)
	t, ok := ret.(*lua.LTable)
	if !ok {
		return nil, fmt.Errorf("layout function must return a table, got %s", ret.Type())
	}
	return toWindows(t), nil
}

// initCmds runs a template's init function for a new project (serialized on
// the kept VM, like function layouts). p = {name, path, workspace, login}.
func (c *Config) initCmds(tp Template, ws *Workspace, login, project, dir string) ([]string, error) {
	if tp.init == nil {
		return nil, nil
	}
	c.vmMu.Lock()
	defer c.vmMu.Unlock()
	arg := c.vm.NewTable()
	arg.RawSetString("name", lua.LString(project))
	arg.RawSetString("path", lua.LString(dir))
	arg.RawSetString("workspace", lua.LString(ws.Name))
	arg.RawSetString("login", lua.LString(login))
	if err := c.vm.CallByParam(lua.P{Fn: tp.init, NRet: 1, Protect: true}, arg); err != nil {
		return nil, fmt.Errorf("template %s: %w", tp.Name, err)
	}
	ret := c.vm.Get(-1)
	c.vm.Pop(1)
	t, ok := ret.(*lua.LTable)
	if !ok {
		return nil, fmt.Errorf("template %s: init must return a list of commands, got %s", tp.Name, ret.Type())
	}
	// Each entry is one command, chained with && (one fails → the rest and
	// the commit don't run). Empty entries are dropped; a newline would end
	// the typed line early, so it's refused.
	var cmds []string
	var bad error
	t.ForEach(func(_, v lua.LValue) {
		c := strings.TrimSpace(v.String())
		switch {
		case c == "":
		case strings.ContainsAny(c, "\r\n"):
			bad = fmt.Errorf("template %s: one command per entry, no newlines: %q", tp.Name, c)
		default:
			cmds = append(cmds, c)
		}
	})
	return cmds, bad
}

func lstr(t *lua.LTable, k string) string {
	if v, ok := t.RawGetString(k).(lua.LString); ok {
		return string(v)
	}
	return ""
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[1:])
		}
	}
	return p
}
