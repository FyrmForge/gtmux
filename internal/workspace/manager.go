package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// fullSweepEvery bounds how stale deletes/renames can get: incremental
// refreshes only see newly pushed repos.
const fullSweepEvery = 6 * time.Hour

// Manager owns one actor per account (workspaces on the same account share its
// repo list, token and cache file).
type Manager struct {
	cfg    *Config
	actors map[string]*actor // by account name
}

// Start loads each workspace's cache and starts its background refresh.
// A nil cfg (no workspaces declared) yields a Manager that errors politely.
func Start(cfg *Config) *Manager {
	m := &Manager{cfg: cfg, actors: map[string]*actor{}}
	if cfg == nil {
		return m
	}
	for _, w := range cfg.Workspaces {
		if a := cfg.Accounts[w.Account]; a != nil && m.actors[a.Name] == nil {
			ac := &actor{acct: a, get: make(chan chan snapshot), kick: make(chan bool, 1), add: make(chan Repo)}
			m.actors[a.Name] = ac
			go ac.run(cachePath(a.Name))
		}
	}
	return m
}

func (m *Manager) Config() *Config { return m.cfg }

// Refresh asks a workspace's actor for an immediate full sweep.
func (m *Manager) Refresh(name string) error {
	w, err := m.cfg.Get(name)
	if err != nil {
		return err
	}
	if a := m.actors[w.Account]; a != nil {
		select {
		case a.kick <- true:
		default: // one already queued
		}
	}
	return nil
}

// Row is one picker entry.
type Row struct {
	Name, Description string
	Local, Remote     bool
	Open              bool
	PushedAt          time.Time
}

// Projects lists a workspace: local folders merged with the cached remote
// list. Order: open sessions first, then most recently pushed, then
// local-only folders alphabetically. open reports whether a session exists.
// status is the last refresh error ("" = fine), for the picker to show.
func (m *Manager) Projects(name string, open func(session string) bool) (w *Workspace, rows []Row, status string, err error) {
	if w, err = m.cfg.Get(name); err != nil {
		return nil, nil, "", err
	}
	byName := map[string]*Row{}
	if w.Name == "folders" {
		for _, f := range m.cfg.Folders {
			byName[f.Name] = &Row{Name: f.Name, Description: f.Path, Local: true}
		}
	} else if ents, err := os.ReadDir(w.Path); err == nil {
		for _, e := range ents {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				r := &Row{Name: e.Name(), Local: true}
				if w.Name == "temp" { // newest first: mtime stands in for push time
					if fi, err := e.Info(); err == nil {
						r.PushedAt = fi.ModTime()
					}
				}
				byName[e.Name()] = r
			}
		}
	}
	if a := m.actors[w.Account]; a != nil {
		snap := a.snapshot()
		status = snap.err
		for _, r := range snap.repos {
			row := byName[r.Name]
			if row == nil {
				if r.Archived {
					continue // archived and not cloned: noise
				}
				row = &Row{Name: r.Name}
				byName[r.Name] = row
			}
			row.Remote, row.Description, row.PushedAt = true, r.Description, r.PushedAt
		}
	}
	if hd := homeDir(w); hd != "" {
		if w.Home != "root" {
			delete(byName, w.Home) // shown as the home row instead
		}
		byName[HomeProject] = &Row{Name: HomeProject, Description: hd, Local: true}
	}
	for _, r := range byName {
		r.Open = open != nil && open(SessionName(w, r.Name))
		rows = append(rows, *r)
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if (a.Name == HomeProject) != (b.Name == HomeProject) { // home pinned on top
			return a.Name == HomeProject
		}
		if a.Open != b.Open {
			return a.Open
		}
		if !a.PushedAt.Equal(b.PushedAt) {
			return a.PushedAt.After(b.PushedAt) // zero (local-only) sorts last
		}
		return a.Name < b.Name
	})
	return w, rows, status, nil
}

// HomeProject is the project token for a workspace's home session (the "⌂
// home" picker row, `gtmux ws home <ws>`). validName can't produce it.
const HomeProject = "@home"

// homeDir is where w's home session runs ("" = no home).
func homeDir(w *Workspace) string {
	switch w.Home {
	case "":
		return ""
	case "root":
		return w.Path
	}
	return filepath.Join(w.Path, w.Home)
}

// SessionName is workspacer's <prefix>-<project>; a folder's session is just
// its name; a home session is <prefix>-home.
func SessionName(w *Workspace, project string) string {
	if w.Name == "folders" {
		return project
	}
	if project == HomeProject {
		return w.Prefix + "-home"
	}
	return w.Prefix + "-" + project
}

// SessionSpec is the session the server builds with its own commands.
type SessionSpec struct {
	Name, Dir, Workspace, Project string
	Env                  map[string]string
	Windows              []Window
}

// Prepare resolves a project to a SessionSpec, cloning it first when the folder
// is missing. env is the requester's environment (SSH_AUTH_SOCK etc.) for git;
// the daemon's own env is frozen at startup. Blocking (a clone can take a
// while): never call it on a session's owner goroutine.
func (m *Manager) Prepare(name, project string, env map[string]string) (*SessionSpec, error) {
	w, err := m.cfg.Get(name)
	if err != nil {
		return nil, err
	}
	if project == HomeProject {
		return m.prepareHome(w, env)
	}
	if project == "home" && w.Home != "" {
		return nil, homeClash(w)
	}
	if project == "" || project == "." || project == ".." || strings.ContainsAny(project, "/\\") {
		return nil, fmt.Errorf("bad project name %q", project)
	}
	if w.Name == "folders" {
		for _, f := range m.cfg.Folders {
			if f.Name == project {
				if fi, err := os.Stat(f.Path); err != nil || !fi.IsDir() {
					return nil, fmt.Errorf("folder %s: %s is not a directory", f.Name, f.Path)
				}
				// The folder's own env/layout stand in for the workspace's.
				fw := &Workspace{Name: w.Name, Env: f.Env, Layout: f.Layout, Projects: w.Projects}
				return m.spec(fw, project, f.Path)
			}
		}
		return nil, fmt.Errorf("no folder %q", project)
	}
	dir := filepath.Join(w.Path, project)
	if w.Name == "temp" {
		if !validName.MatchString(project) {
			return nil, fmt.Errorf("bad temp name %q (letters, digits, . _ -; no leading dot or dash)", project)
		}
		if err := ensureTempRoot(w.Path); err != nil {
			return nil, err
		}
		if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
			return nil, err
		}
		if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("%s: not a directory", dir)
		}
		return m.spec(w, project, dir)
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		a := m.cfg.Accounts[w.Account]
		if a == nil {
			return nil, fmt.Errorf("%s: no such project in %s (and no account to clone from)", project, w.Path)
		}
		if err := clone(a, project, dir, env); err != nil {
			return nil, err
		}
	}
	return m.spec(w, project, dir)
}

// spec builds a project's SessionSpec from its layout and sisters (no I/O but
// sister stats and function layouts).
func (m *Manager) spec(w *Workspace, project, dir string) (*SessionSpec, error) {
	spec := &SessionSpec{Name: SessionName(w, project), Dir: dir, Workspace: w.Name, Project: project, Env: w.Env}
	label := project
	if project == HomeProject {
		label = "home"
	}
	p := w.Projects[project]
	lay := w.Layout
	if p != nil && !p.Layout.empty() {
		lay = p.Layout
	}
	wins, err := m.cfg.windows(lay, w, label, dir)
	if err != nil {
		return nil, err
	}
	spec.Windows = rooted(wins, dir, label)
	if p != nil {
		for _, s := range p.Sisters {
			sdir := filepath.Join(w.Path, s.Name)
			if _, err := os.Stat(sdir); err != nil {
				continue // not cloned: skip, like workspacer
			}
			label := s.Label
			if label == "" {
				label = s.Name
			}
			swins, err := m.cfg.windows(s.Layout, w, s.Name, sdir)
			if err != nil {
				return nil, err
			}
			spec.Windows = append(spec.Windows, rooted(swins, sdir, label)...)
		}
	}
	return spec, nil
}

// homeClash: a project named "home" would share <prefix>-home with the home
// session, so it's refused while the workspace declares a home.
func homeClash(w *Workspace) error {
	return fmt.Errorf("project \"home\" clashes with workspace %s's home session (%s-home); rename the repo or drop home", w.Name, w.Prefix)
}

// prepareHome resolves w's home session: the workspace folder itself (root),
// or a folder under it — cloned when it's only on the remote, else created.
func (m *Manager) prepareHome(w *Workspace, env map[string]string) (*SessionSpec, error) {
	dir := homeDir(w)
	if dir == "" {
		return nil, fmt.Errorf("workspace %s has no home (set home = \"root\" or a folder in ws.add)", w.Name)
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		remote := false
		if a := m.actors[w.Account]; a != nil && w.Home != "root" {
			for _, r := range a.snapshot().repos {
				remote = remote || r.Name == w.Home
			}
		}
		if remote {
			err = clone(m.cfg.Accounts[w.Account], w.Home, dir, env)
		} else {
			err = os.MkdirAll(dir, 0o755)
		}
		if err != nil {
			return nil, err
		}
	}
	return m.spec(w, HomeProject, dir)
}

// validName: GitHub's repo-name charset and length, minus a leading "." (the
// picker hides dot-folders) or "-" (`gtmux temp -h` would read as a name).
var validName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,99}$`)

// NewOpts is one `gtmux workspace new` request.
type NewOpts struct {
	Name, Template  string // Template "" = the workspace's first template, if any
	GitHub, Private bool
	Env             map[string]string // requester's env (SSH_AUTH_SOCK) for git
}

// Create scaffolds a new project and returns its SessionSpec. Everything that
// can fail locally (name, template, layout) is checked BEFORE the remote is
// created; if the folder/git steps still fail after it, the folder is removed
// and the error says the remote exists (never deleted: needs delete_repo).
// The template's commands, the first commit and the push are typed into the
// first pane (interactive scaffolders work; a failure stops the chain, nothing
// committed). Blocking, like Prepare.
func (m *Manager) Create(ws string, o NewOpts) (spec *SessionSpec, err error) {
	w, err := m.cfg.Get(ws)
	if err != nil {
		return nil, err
	}
	if w.Name == "temp" {
		return nil, fmt.Errorf("temp has no new-project wizard; use: gtmux temp <name>")
	}
	if w.Name == "folders" {
		return nil, fmt.Errorf("folders are declared in server.lua (gtmux.workspace.folder{}), not created")
	}
	if !validName.MatchString(o.Name) {
		return nil, fmt.Errorf("bad project name %q (letters, digits, . _ -; no leading dot or dash; max 100)", o.Name)
	}
	if o.Name == "home" && w.Home != "" {
		return nil, homeClash(w)
	}
	dir := filepath.Join(w.Path, o.Name)
	if _, err := os.Stat(dir); err == nil {
		return nil, fmt.Errorf("%s already exists", dir)
	}
	a := m.cfg.Accounts[w.Account]
	if o.GitHub && a == nil {
		return nil, fmt.Errorf("workspace %s has no account to create %s on", w.Name, o.Name)
	}
	if o.Private && !o.GitHub {
		return nil, fmt.Errorf("private needs github")
	}
	ac := m.actors[w.Account]
	if ac != nil {
		for _, r := range ac.snapshot().repos {
			if strings.EqualFold(r.Name, o.Name) {
				return nil, fmt.Errorf("%s already exists on GitHub (open it from the picker)", o.Name)
			}
		}
	}
	var tp Template
	if o.Template != "" {
		found := false
		for _, t := range w.Templates {
			if t.Name == o.Template {
				tp, found = t, true
			}
		}
		if !found {
			return nil, fmt.Errorf("workspace %s has no template %q", w.Name, o.Template)
		}
	} else if len(w.Templates) > 0 {
		tp = w.Templates[0]
	}
	login := ""
	if a != nil {
		login = a.Login
	}
	cmds, err := m.cfg.initCmds(tp, w, login, o.Name, dir)
	if err != nil {
		return nil, err
	}
	spec, err = m.spec(w, o.Name, dir) // layout errors surface before the remote exists
	if err != nil {
		return nil, err
	}
	url := ""
	if o.GitHub {
		if url, err = m.createRemote(a, ac, o); err != nil {
			return nil, err
		}
	}
	// From here a failure removes the half-made folder (the Stat above proved
	// it's ours), so a retry isn't "already exists". A remote is never deleted
	// (needs delete_repo): the error says how to pick it up instead.
	defer func() {
		if err == nil && url != "" {
			err = git(dir, "remote", "add", "origin", url)
		}
		if err != nil {
			spec = nil
			os.RemoveAll(dir)
			if url != "" {
				err = fmt.Errorf("%w (remote %s was created; open it with: gtmux ws %s %s)", err, url, w.Name, o.Name)
			}
		}
	}()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := git(dir, "init", "-q"); err != nil {
		return nil, err
	}
	if len(cmds) == 0 { // so the first commit isn't empty; templates own their files
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# "+o.Name+"\n"), 0o644); err != nil {
			return nil, err
		}
	}
	// The push runs in the pane: give it the requester's agent, not the
	// daemon's frozen one. Copy — spec.Env aliases the workspace config.
	if s := o.Env["SSH_AUTH_SOCK"]; s != "" {
		env := map[string]string{"SSH_AUTH_SOCK": s}
		for k, v := range spec.Env {
			env[k] = v
		}
		spec.Env = env
	}
	chain := append(cmds, "git add -A", `git commit -q -m "Initial commit"`)
	if o.GitHub {
		chain = append(chain, "git push -u origin HEAD")
	}
	first := &spec.Windows[0]
	if len(first.Panes) == 0 {
		first.Panes = []Pane{{}}
	}
	// The pane's own layout cmd (nvim…) still starts after, pass or fail.
	cmd := strings.Join(chain, " && ")
	if c := first.Panes[0].Cmd; c != "" {
		cmd += "; " + c
	}
	first.Panes[0].Cmd = cmd
	return spec, nil
}

// createRepo's caller side: the token, the owner check for user accounts
// (/user/repos creates under the token's owner), the POST, the cache inject.
// Returns the origin URL.
func (m *Manager) createRemote(a *Account, ac *actor, o NewOpts) (string, error) {
	token, err := resolveToken(a.Auth)
	if err != nil {
		return "", err
	}
	if a.Kind == "user" {
		owner, err := tokenLogin(a, token)
		if err != nil {
			return "", err
		}
		if !strings.EqualFold(owner, a.Login) {
			return "", fmt.Errorf("token for account %s belongs to %s, not %s", a.Name, owner, a.Login)
		}
	}
	r, err := createRepo(a, token, o.Name, o.Private)
	if err != nil {
		return "", err
	}
	if ac != nil {
		ac.add <- r
	}
	if r.Name != o.Name { // validName is GitHub's charset, so this shouldn't happen
		return "", fmt.Errorf("GitHub created %s as %s; open it with: gtmux ws <workspace> %s", o.Name, r.Name, r.Name)
	}
	return fmt.Sprintf("git@%s:%s/%s.git", a.SSHHost, a.Login, r.Name), nil
}

func git(dir string, args ...string) error {
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ensureTempRoot creates the per-user temp root, refusing one that another
// user pre-created in the shared temp dir (or a symlink to elsewhere).
func ensureTempRoot(root string) error {
	if err := os.Mkdir(root, 0o700); err != nil && !os.IsExist(err) {
		return err
	}
	fi, err := os.Lstat(root)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 || fi.Mode().Perm()&0o077 != 0 ||
		(ok && int(st.Uid) != os.Getuid()) {
		return fmt.Errorf("%s: not a private directory owned by you; refusing to use it", root)
	}
	return nil
}

// NextTemp is the lowest free temp number ("1", "2", …): folders own their
// number even with their session closed; open(session) skips numbers whose
// session is still open (folder deleted by hand).
func (m *Manager) NextTemp(open func(string) bool) string {
	used := map[string]bool{}
	if ents, err := os.ReadDir(m.cfg.temp().Path); err == nil {
		for _, e := range ents {
			used[e.Name()] = true
		}
	}
	n := 1
	w := m.cfg.temp()
	for used[fmt.Sprint(n)] || (open != nil && open(SessionName(w, fmt.Sprint(n)))) {
		n++
	}
	return fmt.Sprint(n)
}

// RemoveTemp deletes a temp folder. The caller checks its session is closed.
func (m *Manager) RemoveTemp(name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("bad temp name %q", name)
	}
	root := m.cfg.temp().Path
	if err := ensureTempRoot(root); err != nil {
		return err
	}
	dir := filepath.Join(root, name)
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("no temp %s", name)
	}
	return os.RemoveAll(dir)
}

// rooted copies a layout's windows with dirs resolved against dir; an empty
// layout is one plain window. The first window takes name if it has none.
func rooted(wins []Window, dir, name string) []Window {
	if len(wins) == 0 {
		wins = []Window{{}}
	}
	out := make([]Window, len(wins))
	for i, w := range wins {
		w.Panes = append([]Pane(nil), w.Panes...)
		switch {
		case w.Dir == "":
			w.Dir = dir
		case !filepath.IsAbs(w.Dir):
			w.Dir = filepath.Join(dir, w.Dir)
		}
		if i == 0 && w.Name == "" {
			w.Name = name
		}
		out[i] = w
	}
	return out
}

// clone runs git clone over SSH. The daemon has no TTY, so prompts are
// disabled (BatchMode) and a timeout bounds a hung connection; the
// requester's SSH_AUTH_SOCK reaches the agent.
func clone(a *Account, repo, dir string, env map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	url := fmt.Sprintf("git@%s:%s/%s.git", a.SSHHost, a.Login, repo)
	cmd := exec.CommandContext(ctx, "git", "clone", "--", url, dir)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if os.Getenv("GIT_SSH_COMMAND") == "" {
		cmd.Env = append(cmd.Env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes")
	}
	if s := env["SSH_AUTH_SOCK"]; s != "" {
		cmd.Env = append(cmd.Env, "SSH_AUTH_SOCK="+s)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		// A killed git (timeout) can't clean up; a partial checkout would
		// later look like a real project. Prepare only clones absent dirs.
		os.RemoveAll(dir)
		msg := strings.TrimSpace(string(out))
		if i := strings.LastIndexByte(msg, '\n'); i >= 0 {
			msg = msg[i+1:]
		}
		return fmt.Errorf("clone %s: %v: %s", url, err, msg)
	}
	return nil
}

// --- actor -------------------------------------------------------------------

// snapshot is a copy of an actor's state handed to a reader.
type snapshot struct {
	repos []Repo
	err   string
}

// actor is the sole owner of one workspace's remote repo list (the
// windowActor pattern: state lives on one goroutine, others ask via channels).
type actor struct {
	acct *Account
	get  chan chan snapshot
	kick chan bool // true = full sweep
	add  chan Repo // a repo `new` just created: visible before the next refresh
}

func (a *actor) snapshot() snapshot {
	ch := make(chan snapshot, 1)
	a.get <- ch
	return <-ch
}

// cacheFile is the warm-start snapshot on disk. Never holds the token.
type cacheFile struct {
	Repos    []Repo    `json:"repos"`
	ETag     string    `json:"etag,omitempty"`
	HWM      time.Time `json:"hwm"`
	LastFull time.Time `json:"last_full"`
}

func cachePath(account string) string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "gtmux", "accounts", account+".json")
}

func (a *actor) run(path string) {
	repos := map[string]Repo{}
	var c cacheFile
	if path != "" {
		if data, err := os.ReadFile(path); err == nil && json.Unmarshal(data, &c) == nil {
			for _, r := range c.Repos {
				repos[r.Name] = r
			}
		}
	}
	save := func() {
		if path == "" {
			return
		}
		c.Repos = c.Repos[:0]
		for _, r := range repos {
			c.Repos = append(c.Repos, r)
		}
		sort.Slice(c.Repos, func(i, j int) bool { return c.Repos[i].Name < c.Repos[j].Name })
		data, _ := json.Marshal(c)
		os.MkdirAll(filepath.Dir(path), 0o700)
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, data, 0o600); err == nil {
			os.Rename(tmp, path)
		}
	}

	results := make(chan fetchMsg, 4)
	var (
		token     string
		inFlight  bool
		sweepFull bool
		retried   bool
		seen      map[string]bool
		pendETag  string
		changed   bool
		lastErr   string
		queued    bool // a full sweep asked for while one was running
	)
	start := func(full bool) {
		if inFlight {
			queued = queued || full
			return
		}
		inFlight, sweepFull, seen, pendETag, changed = true, full, map[string]bool{}, c.ETag, false
		go fetch(a.acct, token, c.ETag, c.HWM, full, results)
	}
	due := func() bool { return time.Since(c.LastFull) > fullSweepEvery }
	start(due()) // warm at boot: cache is already served, refresh behind it

	tick := time.NewTicker(a.acct.Refresh)
	defer tick.Stop()
	for {
		select {
		case ch := <-a.get:
			s := snapshot{err: lastErr, repos: make([]Repo, 0, len(repos))}
			for _, r := range repos {
				s.repos = append(s.repos, r)
			}
			ch <- s
		case r := <-a.add:
			repos[r.Name] = r
			if seen != nil { // a sweep in flight predates it: don't let it delete r
				seen[r.Name] = true
			}
			save()
		case full := <-a.kick:
			start(full || due())
		case <-tick.C:
			start(due())
		case m := <-results:
			if m.token != "" {
				token = m.token
			}
			if m.err != nil {
				inFlight = false
				if errors.Is(m.err, errUnauthorized) && !retried {
					token, retried = "", true // token rotated? re-resolve once
					start(sweepFull)
					continue
				}
				lastErr = m.err.Error()
				log.Printf("gtmux workspace %s: refresh: %v", a.acct.Name, m.err)
				if changed { // keep what landed, but not the ETag/HWM (see fetch)
					save()
				}
				if queued { // a Ctrl-R that arrived mid-refresh still gets its sweep
					queued = false
					start(true)
				}
				continue
			}
			retried = false // a page landed: the token works, so a later 401 may re-resolve again
			for _, r := range m.repos {
				if old, ok := repos[r.Name]; !ok || old != r {
					repos[r.Name] = r
					changed = true
				}
				seen[r.Name] = true
			}
			if m.etag != "" {
				pendETag = m.etag
			}
			if !m.done {
				continue
			}
			inFlight, retried, lastErr = false, false, ""
			if m.full {
				for n := range repos {
					if !seen[n] {
						delete(repos, n)
						changed = true
					}
				}
				c.LastFull = time.Now()
				changed = true
			}
			// Commit ETag + high-water mark only on a clean finish, so a refresh
			// that died mid-way can't make the next one skip what it missed.
			if pendETag != c.ETag {
				c.ETag, changed = pendETag, true
			}
			for _, r := range repos {
				if r.PushedAt.After(c.HWM) {
					c.HWM, changed = r.PushedAt, true
				}
			}
			if changed {
				save()
			}
			if queued {
				queued = false
				start(true)
			}
		}
	}
}
