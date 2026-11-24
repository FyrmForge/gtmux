package workspace

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	lua "github.com/yuin/gopher-lua"
)

func load(t *testing.T, src string) *Config {
	t.Helper()
	L := lua.NewState()
	g := L.NewTable()
	cfg := Register(L, g)
	L.SetGlobal("gtmux", g)
	if err := L.DoString(src); err != nil {
		t.Fatal(err)
	}
	out, err := cfg.Finish(g)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

const sample = `
local ws = gtmux.workspace
ws.root = "/projects"

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
      { split = "h", size = 30 },
    },
  },
}

ws.account{
  name = "me",
  kind = "user",
  login = "darthvader",
  auth = ws.ghcli_auth{ user = "darthvader" },
}
ws.account{
  name = "work",
  kind = "org",
  login = "acme",
  auth = ws.cmd_auth("pass show work/gh"),
  ssh_host = "github-work",
  refresh = 60,
}
ws.account{ name = "fast", login = "x", refresh = 0.5 }

ws.add{ name = "me", account = "me", layout = dev }
ws.add{
  name = "work",
  account = "work",
  prefix = "w",
  default = true,
  env = { GIT_AUTHOR_EMAIL = "me@acme.com" },
  projects = {
    api = {
      layout = function(p)
        return { { name = p.name .. "-fn", panes = { { cmd = "make" } } } }
      end,
      sisters = { "api-protos", { name = "docs", label = "D" } },
    },
  },
}
`

func TestLuaConfig(t *testing.T) {
	c := load(t, sample)
	if c.Root != "/projects" {
		t.Fatalf("root %q", c.Root)
	}
	me, work := c.Accounts["me"], c.Accounts["work"]
	if me.Auth != (Auth{Kind: "ghcli", Arg: "darthvader"}) || me.SSHHost != "github.com" || me.Refresh != 15*time.Minute {
		t.Fatalf("me account: %+v", me)
	}
	if work.Auth != (Auth{Kind: "cmd", Arg: "pass show work/gh"}) || work.SSHHost != "github-work" || work.Refresh != time.Minute {
		t.Fatalf("work account: %+v", work)
	}
	if c.Accounts["fast"].Refresh != time.Second {
		t.Fatalf("fractional refresh = %v, want 1s floor (0 would panic the ticker)", c.Accounts["fast"].Refresh)
	}
	w, err := c.Get("")
	if err != nil || w.Name != "work" {
		t.Fatalf("default = %v %v, want work (default=true)", w, err)
	}
	if w.Path != "/projects/work" || w.Prefix != "w" || w.Env["GIT_AUTHOR_EMAIL"] != "me@acme.com" {
		t.Fatalf("work ws: %+v", w)
	}
	m, _ := c.Get("me")
	if len(m.Layout.Windows) != 2 || m.Layout.Windows[1].Panes[1] != (Pane{Split: "h", Size: 30}) {
		t.Fatalf("me layout: %+v", m.Layout.Windows)
	}
	if p := w.Projects["api"]; p.Layout.fn == nil || len(p.Sisters) != 2 || p.Sisters[1].Name != "docs" || p.Sisters[1].Label != "D" {
		t.Fatalf("api project: %+v", p)
	}
}

func TestLuaConfigErrors(t *testing.T) {
	for _, src := range []string{
		`gtmux.workspace.add{ name = "refresh" }`,
		`gtmux.workspace.add{ name = "new" }`,
		`gtmux.workspace.add{ name = "temp" }`,
		`gtmux.workspace.add{ name = "x", templates = { { init = function() end } } }`,
		`gtmux.workspace.add{ name = "x", templates = { { name = "t", init = "cargo init" } } }`,
		`gtmux.workspace.add{ name = "x", templates = { { name = "t" }, { name = "t" } } }`,
		`gtmux.workspace.add{ name = "../x" }`,
		`gtmux.workspace.add{ name = "a/b" }`,
		`gtmux.workspace.account{ name = "a", login = "x", auth = "cmd:pass" }`,
		`gtmux.workspace.account{ name = "a", login = "x", kind = "team" }`,
		`gtmux.workspace.add{ name = "x", layout = 3 }`,
		`gtmux.workspace.add{ name = "tmp" }`,
		`gtmux.workspace.add{ name = "x", prefix = "tmp/x" }`,
		`gtmux.workspace.add{ name = "x", home = ".." }`,
		`gtmux.workspace.add{ name = "x", home = "." }`,
		`gtmux.workspace.add{ name = "x", home = "../y" }`,
		`gtmux.workspace.add{ name = "x", prefix = "work/" }`,
	} {
		L := lua.NewState()
		g := L.NewTable()
		Register(L, g)
		L.SetGlobal("gtmux", g)
		if err := L.DoString(src); err == nil {
			t.Errorf("%s: want error", src)
		}
	}
	L := lua.NewState()
	g := L.NewTable()
	cfg := Register(L, g)
	L.SetGlobal("gtmux", g)
	L.DoString(`gtmux.workspace.add{ name = "x", account = "nope" }`)
	if _, err := cfg.Finish(g); err == nil {
		t.Error("unknown account: want error")
	}
	if c, _ := Register(lua.NewState(), lua.NewState().NewTable()).Finish(nil); c != nil {
		t.Error("no workspaces should yield nil config")
	}
}

func TestPrepare(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"work/api", "work/api-protos", "me/site"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	c := load(t, strings.Replace(sample, `ws.root = "/projects"`, fmt.Sprintf("ws.root = %q", root), 1))
	m := &Manager{cfg: c, actors: map[string]*actor{}}

	spec, err := m.Prepare("work", "api", nil)
	if err != nil {
		t.Fatal(err)
	}
	api := filepath.Join(root, "work", "api")
	if spec.Name != "w/api" || spec.Dir != api || spec.Workspace != "work" {
		t.Fatalf("spec: %+v", spec)
	}
	// function layout window, then sister api-protos (exists); docs missing → skipped.
	if len(spec.Windows) != 2 || spec.Windows[0].Name != "api-fn" || spec.Windows[0].Panes[0].Cmd != "make" ||
		spec.Windows[1].Name != "api-protos" || spec.Windows[1].Dir != filepath.Join(root, "work", "api-protos") {
		t.Fatalf("windows: %+v", spec.Windows)
	}

	spec, err = m.Prepare("me", "site", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Windows) != 2 || spec.Windows[0].Name != "edit" || spec.Windows[1].Dir != filepath.Join(root, "me", "site") {
		t.Fatalf("me/site windows: %+v", spec.Windows)
	}
	for _, bad := range []string{"", "..", "a/b"} {
		if _, err := m.Prepare("me", bad, nil); err == nil {
			t.Errorf("Prepare(%q): want error", bad)
		}
	}
}

// fakeGitHub serves repos newest-pushed first, 2 per page, with an ETag on
// page 1 and Link next headers; it counts requests per page.
type fakeGitHub struct {
	repos []Repo
	hits  atomic.Int32
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.hits.Add(1)
	if r.Header.Get("Authorization") != "Bearer tok" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	pg := 1
	fmt.Sscanf(r.URL.Query().Get("page"), "%d", &pg)
	etag := fmt.Sprintf(`"%d-%s"`, len(f.repos), f.repos[0].Name)
	if pg == 1 && r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	lo, hi := (pg-1)*2, min(pg*2, len(f.repos))
	if hi < len(f.repos) {
		q := r.URL.Query()
		q.Set("page", fmt.Sprint(pg+1))
		w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?%s>; rel="next"`, r.Host, r.URL.Path, q.Encode()))
	}
	if pg == 1 {
		w.Header().Set("ETag", etag)
	}
	var out []map[string]any
	for _, x := range f.repos[lo:hi] {
		out = append(out, map[string]any{"name": x.Name, "pushed_at": x.PushedAt, "description": x.Description})
	}
	json.NewEncoder(w).Encode(out)
}

func collect(a *Account, token, etag string, hwm time.Time, full bool) (repos []Repo, last fetchMsg) {
	ch := make(chan fetchMsg, 16)
	go fetch(a, token, etag, hwm, full, ch)
	for m := range ch {
		repos = append(repos, m.repos...)
		if m.done {
			return repos, m
		}
	}
	return
}

func TestFetchIncremental(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	gh := &fakeGitHub{}
	for i := 0; i < 7; i++ { // r6 newest … r0 oldest
		gh.repos = append([]Repo{{Name: fmt.Sprintf("r%d", i), PushedAt: t0.Add(time.Duration(i) * time.Hour)}}, gh.repos...)
	}
	srv := httptest.NewServer(gh)
	defer srv.Close()
	a := &Account{Kind: "org", Login: "acme", apiURL: srv.URL}

	repos, m := collect(a, "tok", "", time.Time{}, false)
	if len(repos) != 7 || !m.full || m.err != nil {
		t.Fatalf("first fetch: %d repos, full=%v err=%v", len(repos), m.full, m.err)
	}
	if gh.hits.Load() != 4 {
		t.Fatalf("first fetch hits = %d, want 4 pages", gh.hits.Load())
	}
	etag := `"7-r6"`

	// Unchanged: page 1 → 304, one request, nothing returned.
	gh.hits.Store(0)
	repos, m = collect(a, "tok", etag, t0.Add(6*time.Hour), false)
	if len(repos) != 0 || gh.hits.Load() != 1 || m.full {
		t.Fatalf("304 path: %d repos, %d hits, full=%v", len(repos), gh.hits.Load(), m.full)
	}

	// r1 pushed (jumps to the top): one page is enough — it holds r6, older than hwm.
	gh.repos = append([]Repo{{Name: "r1", PushedAt: t0.Add(10 * time.Hour)}},
		append(gh.repos[:5:5], gh.repos[6:]...)...)
	gh.hits.Store(0)
	repos, m = collect(a, "tok", etag, t0.Add(6*time.Hour), false)
	if gh.hits.Load() != 1 || len(repos) != 2 || repos[0].Name != "r1" || m.full {
		t.Fatalf("incremental: hits=%d repos=%v full=%v", gh.hits.Load(), repos, m.full)
	}

	// Bad token surfaces as errUnauthorized for the actor's re-resolve.
	if _, m = collect(a, "bad", "", time.Time{}, false); m.err != errUnauthorized {
		t.Fatalf("bad token err = %v", m.err)
	}
}

func TestActorAndProjects(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", cacheDir(t))
	t.Setenv("TEST_GH_TOKEN", "tok")
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	gh := &fakeGitHub{repos: []Repo{
		{Name: "new", PushedAt: t0.Add(3 * time.Hour), Description: "fresh"},
		{Name: "mid", PushedAt: t0.Add(2 * time.Hour)},
		{Name: "old", PushedAt: t0.Add(1 * time.Hour)},
	}}
	srv := httptest.NewServer(gh)
	defer srv.Close()

	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "acme", "old"), 0o755)
	os.MkdirAll(filepath.Join(root, "acme", "zlocal"), 0o755)
	os.MkdirAll(filepath.Join(root, "acme", ".hidden"), 0o755)
	acct := &Account{Name: "acme", Kind: "org", Login: "acme", Auth: Auth{Kind: "env", Arg: "TEST_GH_TOKEN"},
		Refresh: time.Hour, apiURL: srv.URL}
	w := &Workspace{Name: "acme", Prefix: "acme", Account: "acme", Path: filepath.Join(root, "acme")}
	cfg := &Config{Accounts: map[string]*Account{"acme": acct}, Workspaces: []*Workspace{w}}
	m := Start(cfg)

	var rows []Row
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, rows, _, _ = m.Projects("", func(s string) bool { return s == "acme/mid" })
		if len(rows) == 5 && rows[3].Remote { // pages land 2 at a time: wait for "old"'s
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	var got []string
	for _, r := range rows {
		got = append(got, r.Name)
	}
	// open first, then A-Z (dot-folders included).
	if strings.Join(got, ",") != "mid,.hidden,new,old,zlocal" {
		t.Fatalf("order = %v", got)
	}
	if !rows[3].Local || !rows[3].Remote || rows[2].Description != "fresh" || rows[4].Remote {
		t.Fatalf("row flags: %+v", rows)
	}

	// The cache landed on disk (warm start next boot) and never holds the token.
	path := cachePath("acme")
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "tok") || !strings.Contains(string(data), `"new"`) {
		t.Fatalf("cache: %s", data)
	}
}

// TestPrepareClones: a missing project folder is cloned from
// git@<ssh_host>:<login>/<repo>.git — redirected here to a local bare repo via
// git's url.insteadOf (env config), so the real SSH URL shape is exercised.
func TestPrepareClones(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	src := t.TempDir()
	bare := filepath.Join(src, "acme", "api.git")
	for _, args := range [][]string{
		{"init", "-q", "--bare", bare},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url."+src+"/.insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", "git@github-work:")

	root := t.TempDir()
	acct := &Account{Name: "w", Kind: "org", Login: "acme", SSHHost: "github-work"}
	w := &Workspace{Name: "w", Prefix: "w", Account: "w", Path: filepath.Join(root, "w")}
	m := &Manager{cfg: &Config{Accounts: map[string]*Account{"w": acct}, Workspaces: []*Workspace{w}}, actors: map[string]*actor{}}
	spec, err := m.Prepare("w", "api", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(spec.Dir, ".git")); err != nil {
		t.Fatalf("not cloned: %v", err)
	}
	if _, err := m.Prepare("w", "missing", nil); err == nil || !strings.Contains(err.Error(), "clone git@github-work:acme/missing.git") {
		t.Fatalf("missing repo err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(w.Path, "missing")); !os.IsNotExist(err) {
		t.Fatalf("failed clone left a folder behind: %v", err)
	}
}

// TestCreateLocal: `new` without GitHub — folder + git init, README only when
// the template has no commands, and the first pane gets the scaffold chain
// with its own layout cmd appended.
func TestCreateLocal(t *testing.T) {
	root := t.TempDir()
	m := Start(load(t, fmt.Sprintf(`
local ws = gtmux.workspace
ws.root = %q
ws.add{
  name = "me",
  layout = {
    {
      name = "edit",
      panes = {
        { cmd = "nvim" },
      },
    },
  },
  templates = {
    { name = "empty" },
    {
      name = "rust",
      init = function(p)
        return {
          "",
          "cargo init --name " .. p.name,
        }
      end,
    },
    {
      name = "multi",
      init = function(p)
        return {
          "a\nb",
        }
      end,
    },
  },
}
`, root)))
	commit := `git add -A && git commit -q -m "Initial commit"`

	spec, err := m.Create("me", NewOpts{Name: "a"}) // "" = first template (empty)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "me", "a")
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatal("no git init:", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "README.md")); err != nil {
		t.Fatal("no README:", err)
	}
	if got := spec.Windows[0].Panes[0].Cmd; got != commit+"; nvim" || spec.Name != "me/a" {
		t.Fatalf("spec %s cmd = %q", spec.Name, got)
	}

	spec, err = m.Create("me", NewOpts{Name: "b", Template: "rust"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "me", "b", "README.md")); err == nil {
		t.Fatal("README written despite template commands")
	}
	if got := spec.Windows[0].Panes[0].Cmd; got != "cargo init --name b && "+commit+"; nvim" {
		t.Fatalf("cmd = %q", got)
	}

	for _, o := range []NewOpts{
		{Name: "a"},                   // exists
		{Name: "x y"},                 // bad name
		{Name: ".."},                  // bad name
		{Name: "c", Template: "nope"}, // unknown template
		{Name: "c", GitHub: true},     // no account
		{Name: ".hidden"},             // no new dot-projects
		{Name: strings.Repeat("x", 101)},
		{Name: "c", Private: true},     // private without github
		{Name: "c", Template: "multi"}, // newline in a command
	} {
		if _, err := m.Create("me", o); err == nil {
			t.Errorf("%+v: want error", o)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "me", "c")); err == nil {
		t.Error("a refused create left a folder")
	}
}

// TestCreateGitHub: the remote is created first (POST, empty, private flag),
// origin points at the SSH URL, the chain ends with the push, and the picker
// sees the repo at once. A token that can't create repos stops before any
// local folder exists.
func TestCreateGitHub(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", cacheDir(t))
	t.Setenv("TEST_GH_TOKEN", "tok")
	var posted map[string]any
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/user" {
			w.Write([]byte(`{"login":"bob"}`))
			return
		}
		if r.Method == "GET" {
			w.Write([]byte("[]"))
			return
		}
		posts.Add(1)
		json.NewDecoder(r.Body).Decode(&posted)
		if r.URL.Path != "/orgs/acme/repos" || r.Header.Get("Authorization") != "Bearer tok" || posted["name"] == "deny" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"name": posted["name"]})
	}))
	defer srv.Close()
	root := t.TempDir()
	acct := &Account{Name: "acme", Kind: "org", Login: "acme", SSHHost: "github-work",
		Auth: Auth{Kind: "env", Arg: "TEST_GH_TOKEN"}, Refresh: time.Hour, apiURL: srv.URL}
	w := &Workspace{Name: "acme", Prefix: "acme", Account: "acme", Path: filepath.Join(root, "acme")}
	m := Start(&Config{Accounts: map[string]*Account{"acme": acct}, Workspaces: []*Workspace{w}})

	w.Env = map[string]string{"A": "1"}
	spec, err := m.Create("acme", NewOpts{Name: "svc", GitHub: true, Private: true,
		Env: map[string]string{"SSH_AUTH_SOCK": "/agent"}})
	if err != nil {
		t.Fatal(err)
	}
	// The pane's push gets the requester's agent; the workspace config is untouched.
	if spec.Env["SSH_AUTH_SOCK"] != "/agent" || spec.Env["A"] != "1" || len(w.Env) != 1 {
		t.Fatalf("env: spec %v, workspace %v", spec.Env, w.Env)
	}
	if posted["private"] != true || posted["auto_init"] != false {
		t.Fatalf("posted %v", posted)
	}
	out, _ := exec.Command("git", "-C", filepath.Join(w.Path, "svc"), "remote", "get-url", "origin").Output()
	if strings.TrimSpace(string(out)) != "git@github-work:acme/svc.git" {
		t.Fatalf("origin = %q", out)
	}
	if cmd := spec.Windows[0].Panes[0].Cmd; !strings.HasSuffix(cmd, "&& git push -u origin HEAD") {
		t.Fatalf("cmd = %q", cmd)
	}
	_, rows, _, _ := m.Projects("acme", nil)
	if len(rows) != 1 || rows[0].Name != "svc" || !rows[0].Remote || !rows[0].Local {
		t.Fatalf("rows = %+v", rows)
	}
	if _, err := m.Create("acme", NewOpts{Name: "svc"}); err == nil {
		t.Fatal("existing name: want error")
	}

	_, err = m.Create("acme", NewOpts{Name: "deny", GitHub: true})
	if err == nil || !strings.Contains(err.Error(), "can't create repos") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(w.Path, "deny")); err == nil {
		t.Fatal("folder created despite remote failure")
	}

	// A user token that belongs to someone else is refused before any POST.
	n := posts.Load()
	acct.Kind = "user"
	_, err = m.Create("acme", NewOpts{Name: "mine", GitHub: true})
	if err == nil || !strings.Contains(err.Error(), "belongs to bob") || posts.Load() != n {
		t.Fatalf("owner check: err=%v posts %d→%d", err, n, posts.Load())
	}
	acct.Kind = "org"

	// A layout error surfaces before the remote exists.
	m.cfg.vm = lua.NewState()
	w.Layout = Layout{fn: m.cfg.vm.NewFunction(func(l *lua.LState) int { l.RaiseError("boom"); return 0 })}
	if _, err := m.Create("acme", NewOpts{Name: "lay", GitHub: true}); err == nil || posts.Load() != n {
		t.Fatalf("layout error: err=%v posts %d→%d", err, n, posts.Load())
	}
	w.Layout = Layout{}

	// A local failure after the remote exists: folder removed, error names
	// the remote. (The workspace path is a file, so mkdir fails.)
	w.Path = filepath.Join(root, "file")
	os.WriteFile(w.Path, nil, 0o644)
	_, err = m.Create("acme", NewOpts{Name: "late", GitHub: true})
	if err == nil || !strings.Contains(err.Error(), "remote git@github-work:acme/late.git was created") {
		t.Fatalf("late failure: %v", err)
	}
}

// cacheDir is a throwaway XDG_CACHE_HOME. Not t.TempDir: actors never stop,
// so one may still be saving while the test's cleanup deletes the dir.
func cacheDir(t *testing.T) string {
	d, err := os.MkdirTemp("", "gtmux-ws-cache")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) }) // best effort
	return d
}

// TestTemp: the built-in temp workspace works with no config, numbers fill the
// lowest gap, rows are folders newest first, rm deletes, and a temp root that
// isn't private to us is refused.
func TestTemp(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	m := Start(nil)
	if w, err := m.Config().Get(""); err != nil || w.Name != "temp" {
		t.Fatalf("no config: Get(\"\") = %v, %v", w, err)
	}
	if got := m.Config().Names(); len(got) != 1 || got[0] != "temp" {
		t.Fatalf("names = %v", got)
	}
	if n := m.NextTemp(nil); n != "1" {
		t.Fatalf("first = %s", n)
	}
	spec, err := m.Prepare("temp", "1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Name != "tmp/1" || spec.Workspace != "temp" || spec.Dir != filepath.Join(TempRoot(), "1") {
		t.Fatalf("spec = %+v", spec)
	}
	if fi, _ := os.Stat(TempRoot()); fi.Mode().Perm() != 0o700 {
		t.Fatalf("root perm = %v", fi.Mode().Perm())
	}
	time.Sleep(10 * time.Millisecond)
	if _, err := m.Prepare("temp", "scratch", nil); err != nil {
		t.Fatal(err)
	}
	if n := m.NextTemp(nil); n != "2" {
		t.Fatalf("next = %s", n)
	}
	_, rows, _, _ := m.Projects("temp", nil)
	if len(rows) != 2 || rows[0].Name != "scratch" {
		t.Fatalf("rows (newest first) = %+v", rows)
	}
	if n := m.NextTemp(func(s string) bool { return s == "tmp/2" }); n != "3" {
		t.Fatalf("open tmp/2: next = %s", n)
	}
	if _, err := m.Create("", NewOpts{Name: "z"}); err == nil {
		t.Fatal("new on temp: want error")
	}
	os.WriteFile(filepath.Join(TempRoot(), "file"), nil, 0o600)
	for _, bad := range []string{"", ".x", "a/b", "-h", "file"} {
		if _, err := m.Prepare("temp", bad, nil); err == nil {
			t.Errorf("temp %q: want error", bad)
		}
	}
	if err := m.RemoveTemp("1"); err != nil {
		t.Fatal(err)
	}
	if n := m.NextTemp(nil); n != "1" {
		t.Fatalf("after rm: next = %s", n)
	}
	if err := m.RemoveTemp("nope"); err == nil {
		t.Fatal("rm missing: want error")
	}
	os.Chmod(TempRoot(), 0o755)
	if _, err := m.Prepare("temp", "x", nil); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("shared root: %v", err)
	}

	// A temp-only config keeps the VM alive and carries layout/env.
	cfg := load(t, `gtmux.workspace.temp{ env = { B = "1" } }
gtmux.workspace.temp{ env = { A = "1" }, layout = { { name = "scratch" } } }`)
	if cfg == nil || cfg.temp().Env["A"] != "1" || cfg.temp().Env["B"] != "" || len(cfg.temp().Layout.Windows) != 1 {
		t.Fatalf("temp config: %+v", cfg)
	}
}

// TestFolders: ws.folder{} declares single folders, listed under the
// "folders" pseudo-workspace and opened as one session named after the folder,
// with the folder's own env/layout; no create, missing paths refused.
func TestFolders(t *testing.T) {
	dir := t.TempDir()
	cfg := load(t, `gtmux.workspace.folder{ name = "dots", path = "`+dir+`",
  env = { A = "1" }, layout = { { name = "profile", panes = { { cmd = "nvim" }, {} } } } }
gtmux.workspace.folder{ name = "gone", path = "`+filepath.Join(dir, "nope")+`" }`)
	if cfg == nil {
		t.Fatal("folder-only config dropped")
	}
	m := Start(cfg)
	if got := strings.Join(cfg.Names(), ","); got != "folders,temp" {
		t.Fatalf("names = %s", got)
	}
	_, rows, _, err := m.Projects("folders", func(s string) bool { return s == "dots" })
	if err != nil || len(rows) != 2 || rows[0].Name != "dots" || !rows[0].Open || rows[0].Description != dir {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	spec, err := m.Prepare("folders", "dots", nil)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Name != "dots" || spec.Workspace != "folders" || spec.Dir != dir || spec.Env["A"] != "1" ||
		len(spec.Windows) != 1 || spec.Windows[0].Name != "profile" || spec.Windows[0].Dir != dir {
		t.Fatalf("spec = %+v", spec)
	}
	for _, bad := range []string{"gone", "missing"} {
		if _, err := m.Prepare("folders", bad, nil); err == nil {
			t.Errorf("folder %q: want error", bad)
		}
	}
	if _, err := m.Create("folders", NewOpts{Name: "x"}); err == nil {
		t.Fatal("new on folders: want error")
	}
	if _, err := Start(nil).Config().Get("folders"); err == nil {
		t.Fatal("no folders declared: Get(folders) want error")
	}
}

// TestDeclaratorsFrozen: after Finish, ws.* declarations refuse, through an
// alias captured at load time too (a layout function must not mutate Config).
func TestDeclaratorsFrozen(t *testing.T) {
	cfg := load(t, `local add = gtmux.workspace.add
gtmux.workspace.add{ name = "a", path = "/tmp/a", layout = function(p) add{ name = "b" } return {} end }`)
	_, err := cfg.windows(cfg.Workspaces[0].Layout, cfg.Workspaces[0], "x", "/tmp/a/x")
	if err == nil || !strings.Contains(err.Error(), "load-time only") {
		t.Fatalf("aliased add after load: %v", err)
	}
	if len(cfg.Workspaces) != 1 {
		t.Fatalf("workspaces mutated: %d", len(cfg.Workspaces))
	}
}

// TestHome: ws.add{ home = ... } adds a pinned home row (session <prefix>/home)
// in the workspace folder itself (root) or a folder under it, which is then
// hidden from the normal rows and created when missing (no remote).
func TestHome(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"a/x", "b/hq", "b/y"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	cfg := load(t, `local ws = gtmux.workspace
ws.root = "`+root+`"
ws.add{ name = "a", home = "root", layout = { {}, { name = "two" } } }
ws.add{ name = "b", home = "hq" }
ws.add{ name = "c", home = "made" }
ws.add{ name = "d2", home = ".llm" }
ws.add{ name = "d" }`)
	m := Start(cfg)
	_, rows, _, _ := m.Projects("b", nil)
	if len(rows) != 2 || rows[0].Name != HomeProject || rows[1].Name != "y" || rows[0].Description != filepath.Join(root, "b", "hq") {
		t.Fatalf("b rows = %+v", rows)
	}
	spec, err := m.Prepare("a", HomeProject, nil)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Name != "a/home" || spec.Dir != filepath.Join(root, "a") || spec.Windows[0].Name != "home" || len(spec.Windows) != 2 {
		t.Fatalf("a home spec = %+v", spec)
	}
	if _, err := m.Prepare("c", HomeProject, nil); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(root, "c", "made")); err != nil || !fi.IsDir() {
		t.Fatalf("home folder not created: %v", err)
	}
	// A dot-folder home: created on open, and not also listed as a normal row.
	spec, err = m.Prepare("d2", HomeProject, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(root, "d2", ".llm")); err != nil || !fi.IsDir() || spec.Name != "d2/home" {
		t.Fatalf("d2 home: spec %+v, stat err %v", spec, err)
	}
	if _, rows, _, _ := m.Projects("d2", nil); len(rows) != 1 || rows[0].Name != HomeProject {
		t.Fatalf("d2 rows = %+v", rows)
	}
	if _, err := m.Prepare("d", HomeProject, nil); err == nil {
		t.Fatal("no home declared: want error")
	}
	if _, rows, _, _ := m.Projects("d", nil); len(rows) != 0 {
		t.Fatalf("d has a home row: %+v", rows)
	}
}

// TestPathAndHomeClash: a relative ws.add path is under ws.root (not the
// daemon's cwd); a project named "home" is refused while a home is declared.
func TestPathAndHomeClash(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "sub", "home"), 0o755)
	cfg := load(t, `gtmux.workspace.root = "`+root+`"
gtmux.workspace.add{ name = "w", path = "sub", home = "root" }`)
	m := Start(cfg)
	if w, _ := cfg.Get("w"); w.Path != filepath.Join(root, "sub") {
		t.Fatalf("relative path = %s", w.Path)
	}
	if _, err := m.Prepare("w", "home", nil); err == nil || !strings.Contains(err.Error(), "clashes") {
		t.Fatalf("open home project: %v", err)
	}
	if _, err := m.Create("w", NewOpts{Name: "home"}); err == nil || !strings.Contains(err.Error(), "clashes") {
		t.Fatalf("new home project: %v", err)
	}
}
