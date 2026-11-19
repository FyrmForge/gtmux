//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/gtmux/internal/harness"
)

// TestWorkspaceOpen drives the whole local path (no GitHub account): the
// prefix+P picker lists <root>/<ws>/<project> folders, Enter builds the
// project's session from its layout (windows, typed pane commands, workspace
// env) and switches this client to it; the session is tagged @workspace.
func TestWorkspaceOpen(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"work/alpha", "work/beta", "work/x-y", "other/y"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	serverLua := fmt.Sprintf(`
local ws = gtmux.workspace
ws.root = %q
ws.add{
  name = "work",
  env = { WS_MARK = "from-env" },
  layout = {
    {
      name = "edit",
      panes = {
        { cmd = "echo PANE-$WS_MARK" },
        { split = "h" },
      },
    },
    { name = "logs" },
  },
}
-- prefix "work-x" + project "y" collides with work's "work" + "x-y".
ws.add{ name = "other", prefix = "work-x" }
`, root)
	c := harness.StartWithConfig(t, "", serverLua)
	c.WaitForStatus("default")

	c.Prefix("P")
	c.WaitForText("workspace: work")
	c.WaitForText("beta")
	c.Type("bet")
	time.Sleep(200 * time.Millisecond)
	c.TypeLine("")

	waitStatus(c, "work-beta") // a build: give it more than the 2s default
	// The first pane spawned with the workspace env and then had a command
	// typed into it: give a cold login shell more than the default 2s.
	c.WaitForUntil(10*time.Second, func(s *harness.Screen) bool { return strings.Contains(s.String(), "PANE-from-env") })

	out := c.Run("run", "work-beta", "list-windows")
	if !strings.Contains(out, "edit") || !strings.Contains(out, "logs") || strings.Count(strings.TrimSpace(out), "\n") != 1 {
		t.Fatalf("windows:\n%s", out)
	}
	if out := c.Run("run", "work-beta", "display-message", "-p", "#{@workspace}"); strings.TrimSpace(out) != "work" {
		t.Fatalf("@workspace = %q", out)
	}
	if out := c.Run("run", "work-beta", "display-message", "-p", "#{pane_path}"); !strings.Contains(out, "work/beta") {
		t.Fatalf("pane path = %q", out)
	}

	// The headline fix over workspacer: `gtmux ws` typed in a pane of this
	// daemon switches THIS client (no nested attach).
	c.TypeLine(c.Bin() + " ws work alpha")
	waitStatus(c, "work-alpha")

	// "" = the acting session's own workspace; an existing session is just
	// switched to, not rebuilt.
	c.Run("run", "work-alpha", "workspace-open", "", "beta")
	waitStatus(c, "work-beta")
	if out := c.Run("run", "work-beta", "list-windows"); strings.Count(strings.TrimSpace(out), "\n") != 1 {
		t.Fatalf("beta rebuilt? windows:\n%s", out)
	}
	// Opening the project you're in is a no-op: no detach/reattach, and the
	// last session stays alpha (it used to become beta itself).
	c.Run("run", "work-beta", "workspace-open", "work", "beta")
	time.Sleep(300 * time.Millisecond)
	c.Run("run", "work-beta", "switch-client", "-l")
	waitStatus(c, "work-alpha")
	c.Run("run", "work-alpha", "workspace-open", "work", "beta")
	waitStatus(c, "work-beta")

	// A colliding session name owned by another workspace is refused, not reused.
	c.Run("run", "work-beta", "workspace-open", "work", "x-y")
	waitStatus(c, "work-x-y")
	c.Run("run", "work-x-y", "workspace-open", "other", "y")
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(c.Run("run", "work-x-y", "show-messages"), "belongs to workspace work") {
		if time.Now().After(deadline) {
			t.Fatalf("collision not refused; messages:\n%s", c.Run("run", "work-x-y", "show-messages"))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func waitStatus(c *harness.Client, sub string) {
	c.WaitForUntil(10*time.Second, func(s *harness.Screen) bool { return s.Status().Has(sub) })
}

// TestWorkspaceStandalonePicker: `gtmux workspace <ws>` outside gtmux draws the
// same Lua picker on a blank screen (no attached session), C-c exits clean,
// and Enter opens the project and attaches to it.
func TestWorkspaceStandalonePicker(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"work/alpha", "work/beta"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	c := harness.StartWithConfig(t, "", fmt.Sprintf(`
gtmux.workspace.root = %q
gtmux.workspace.add{ name = "work" }
`, root))
	c.WaitForStatus("default")

	quit := c.SpawnTTY(80, 24, "ws", "work")
	quit.WaitForText("workspace: work")
	quit.WaitForText("alpha")
	quit.Ctrl('c')
	time.Sleep(300 * time.Millisecond)
	if err := c.RunErr("has-session", "work-alpha"); err == nil {
		t.Fatal("C-c must not create a session")
	}

	p := c.SpawnTTY(80, 24, "ws", "work")
	p.WaitForText("beta")
	p.Type("alp")
	time.Sleep(200 * time.Millisecond)
	p.TypeLine("")
	waitStatus(p, "work-alpha")
}

// TestWorkspaceNew: the new-project wizard, local only (no account). From the
// picker, Enter on a filter that matches nothing starts the wizard with that
// name; the chosen template's command, then the first commit, run in the new
// session's first pane. `ws new` with flags skips the wizard; `ws new` outside
// gtmux shows the wizard on a blank screen and attaches.
func TestWorkspaceNew(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "work", "alpha"), 0o755)
	c := harness.StartWithConfig(t, "", fmt.Sprintf(`
local ws = gtmux.workspace
ws.root = %q
ws.add{
  name = "work",
  env = {
    GIT_CONFIG_GLOBAL = "/dev/null",
    GIT_AUTHOR_NAME = "t",
    GIT_AUTHOR_EMAIL = "t@t",
    GIT_COMMITTER_NAME = "t",
    GIT_COMMITTER_EMAIL = "t@t",
  },
  templates = {
    { name = "empty" },
    {
      name = "mark",
      init = function(p)
        return {
          "echo SCAFFOLD-" .. p.name .. " > mark.txt",
        }
      end,
    },
  },
}
ws.add{ name = "bare" }
`, root))
	c.WaitForStatus("default")
	committed := func(name string) {
		t.Helper()
		dir := filepath.Join(root, "work", name)
		deadline := time.Now().Add(15 * time.Second)
		for {
			out, _ := exec.Command("git", "-C", dir, "log", "--oneline").CombinedOutput()
			if strings.Contains(string(out), "Initial commit") {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: no first commit: %s", name, out)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	c.Prefix("P")
	c.WaitForText("alpha")
	c.Type("fresh")
	c.WaitForText("Enter: create fresh")
	c.TypeLine("")
	c.WaitForText("new project: work")
	c.TypeLine("") // name step: "fresh" prefilled
	c.WaitForText("Up/Down choose")
	c.Type("\x1b[B") // template: empty → mark
	c.WaitForText("template: mark")
	c.TypeLine("") // template chosen
	c.WaitForText("Enter create")
	c.TypeLine("") // summary: create
	waitStatus(c, "work-fresh")
	committed("fresh")
	if b, err := os.ReadFile(filepath.Join(root, "work", "fresh", "mark.txt")); err != nil || !strings.Contains(string(b), "SCAFFOLD-fresh") {
		t.Fatalf("template didn't run: %q %v", b, err)
	}

	c.TypeLine(c.Bin() + " ws new work quick --template empty")
	waitStatus(c, "work-quick")
	committed("quick")
	if _, err := os.Stat(filepath.Join(root, "work", "quick", "README.md")); err != nil {
		t.Fatal("empty template: no README")
	}

	// No templates: the wizard is name → summary, and the empty template arg
	// must survive the overlay → session-command path.
	c.Prefix("P")
	c.WaitForText("workspace: ")
	c.Type("\t") // cycle to "bare"
	c.WaitForText("workspace: bare")
	c.Type("plain")
	c.WaitForText("Enter: create plain")
	c.TypeLine("")
	c.WaitForText("new project: bare")
	c.TypeLine("") // name
	c.WaitForText("Enter create")
	c.TypeLine("")
	waitStatus(c, "bare-plain")
	if _, err := os.Stat(filepath.Join(root, "bare", "plain", "README.md")); err != nil {
		t.Fatal("bare: no README")
	}

	// A leftover session (folder deleted) must refuse, not swallow the scaffold.
	c.Run("run", "bare-plain", "workspace-open", "work", "alpha")
	waitStatus(c, "work-alpha")
	os.RemoveAll(filepath.Join(root, "work", "alpha"))
	c.Run("run", "work-alpha", "workspace-new", "work", "alpha", "", "0", "0")
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(c.Run("run", "work-alpha", "show-messages"), "session work-alpha already exists") {
		if time.Now().After(deadline) {
			t.Fatalf("leftover session not refused; messages:\n%s", c.Run("run", "work-alpha", "show-messages"))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(root, "work", "alpha")); err == nil {
		t.Fatal("refused create left a folder")
	}

	p := c.SpawnTTY(80, 24, "ws", "new", "work", "solo")
	p.WaitForText("new project: work")
	p.TypeLine("") // name
	time.Sleep(200 * time.Millisecond)
	p.TypeLine("") // template (empty)
	p.WaitForText("Enter create")
	p.TypeLine("")
	waitStatus(p, "work-solo")
	committed("solo")
}

// TestWorkspaceTemp: temp workspaces with no workspaces configured. `gtmux
// temp` takes the next number, `gtmux t <name>` a name; the picker opens on
// temp (the only entry), C-o makes the next number, C-d + y deletes a closed
// temp; `temp rm` is refused while the session is open.
func TestWorkspaceTemp(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir()) // the daemon inherits it
	c := harness.StartWithConfig(t, "", "")
	c.WaitForStatus("default")
	root := filepath.Join(os.Getenv("TMPDIR"), fmt.Sprintf("gtmux-%d", os.Getuid()))

	// The pane's $GTMUX still names "default" after a rename: the CLI must find
	// the session holding its pane instead of failing "no such session".
	c.Run("rename", "default", "renamed")
	waitStatus(c, "renamed")
	c.TypeLine(c.Bin() + " temp")
	waitStatus(c, "tmp-1")
	c.TypeLine(c.Bin() + " t notes")
	waitStatus(c, "tmp-notes")
	if out := c.Run("run", "tmp-notes", "display-message", "-p", "#{@workspace} #{pane_path}"); !strings.Contains(out, "temp "+filepath.Join(root, "notes")) {
		t.Fatalf("tag/path = %q", out)
	}

	c.Prefix("P")
	c.WaitForText("workspace: temp")
	c.WaitForText("notes")
	c.Ctrl('o')
	waitStatus(c, "tmp-2")

	if err := c.RunErr("temp", "rm", "1"); err == nil {
		t.Fatal("rm of an open temp must be refused")
	}
	// A renamed temp session still holds its folder.
	c.Run("rename", "tmp-notes", "kept")
	if err := c.RunErr("temp", "rm", "notes"); err == nil {
		t.Fatalf("rm of a renamed open temp: %v", err)
	}
	c.Run("run", "tmp-2", "kill-session", "-t", "tmp-1")
	if err := c.RunErr("temp", "rm", "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "1")); err == nil {
		t.Fatal("rm left the folder")
	}

	os.Mkdir(filepath.Join(root, "old"), 0o700)
	c.Prefix("P")
	c.WaitForText("old")
	c.Type("old")
	time.Sleep(200 * time.Millisecond)
	c.Ctrl('d')
	c.WaitForText("delete temp old? y/n")
	c.Type("y")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, "old")); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("C-d y didn't delete")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestWorkspaceHomeAndAdopt: `gtmux ws home <ws>` opens <prefix>-home in the
// workspace folder (home = "root"); opening a project whose session was made
// by hand (untagged) reuses it and tags it.
func TestWorkspaceHomeAndAdopt(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "work", "beta"), 0o755)
	c := harness.StartWithConfig(t, "", fmt.Sprintf(`gtmux.workspace.root = %q
gtmux.workspace.add{ name = "work", home = "root" }`, root))
	c.WaitForStatus("default")

	c.TypeLine(c.Bin() + " ws home work")
	waitStatus(c, "work-home")
	if out := c.Run("run", "work-home", "display-message", "-p", "#{@workspace} #{pane_path}"); !strings.Contains(out, "work "+filepath.Join(root, "work")) {
		t.Fatalf("home tag/path = %q", out)
	}

	c.Run("run", "work-home", "new-session", "-d", "-s", "work-beta")
	if out := c.Run("run", "work-beta", "display-message", "-p", "[#{@workspace}]"); strings.TrimSpace(out) != "[]" {
		t.Fatalf("hand-made session already tagged: %q", out)
	}
	c.TypeLine(c.Bin() + " ws work beta")
	waitStatus(c, "work-beta")
	if out := c.Run("run", "work-beta", "display-message", "-p", "#{@workspace}/#{@project}"); strings.TrimSpace(out) != "work/beta" {
		t.Fatalf("adopted session tags = %q", out)
	}
}

// TestWorkspaceOpenDeadSession: a layout whose first window dir doesn't exist
// kills the session as it starts; the open must report that, not a silent
// "success" that switches nowhere.
func TestWorkspaceOpenDeadSession(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "bad", "x"), 0o755)
	c := harness.StartWithConfig(t, "", fmt.Sprintf(`gtmux.workspace.root = %q
gtmux.workspace.add{ name = "bad", layout = { { dir = "missing" } } }`, root))
	c.WaitForStatus("default")
	c.Run("run", "default", "workspace-open", "bad", "x")
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(c.Run("run", "default", "show-messages"), "exited while starting") {
		if time.Now().After(deadline) {
			t.Fatalf("no failure message; messages:\n%s", c.Run("run", "default", "show-messages"))
		}
		time.Sleep(50 * time.Millisecond)
	}
}
