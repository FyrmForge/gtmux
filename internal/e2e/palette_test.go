//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/gtmux/internal/harness"
)

// TestCommandPalette drives prefix+: end to end: the palette opens with the
// command list, a fuzzy query + Enter on switch-client -t opens stage two with
// the sessions, and picking one switches the client there.
func TestCommandPalette(t *testing.T) {
	c := harness.Start(t)
	c.Resize(120, 30)
	c.WaitForStatus("default")
	c.Run("new", "notes", "-d")
	// Other sessions self-report into the snapshot on a 1s tick, and the client
	// gets it with the next status push: wait a tick, then cause one.
	time.Sleep(1500 * time.Millisecond)
	c.Prefix("c")
	c.WaitForStatus(" 2 ")

	c.Prefix(":")
	c.WaitForText("─ commands ─")
	c.Type("swcl t")
	c.WaitFor(func(s *harness.Screen) bool { return strings.Contains(s.String(), "switch to a session") })
	c.Type("\r")
	c.WaitForText("─ session ─")
	c.Type("nts")
	c.WaitForText("❯ switch-client -t nts")
	c.Type("\r")
	c.WaitForStatus("notes")
}

// TestPaletteDisplayMessage: display-message from the palette (no -p) shows its
// expanded text on the status line, like tmux, instead of vanishing into the
// action's unused return value.
func TestPaletteDisplayMessage(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("default")
	c.Prefix(":")
	c.WaitForText("─ commands ─")
	c.TypeLine("display-message hi from #{session}")
	c.WaitForStatus("hi from default")
}

// TestKillPaneConfirm: prefix+x opens the centered confirm dialog instead of
// killing outright; n leaves the pane, y kills it.
func TestKillPaneConfirm(t *testing.T) {
	c := harness.Start(t)
	c.Resize(120, 30)
	c.WaitForStatus("default")
	c.Prefix("%")
	panes := func() int { return strings.Count(strings.TrimSpace(c.Run("run", "default", "list-panes")), "\n") + 1 }
	c.WaitFor(func(*harness.Screen) bool { return panes() == 2 })

	c.Prefix("x")
	c.WaitForText("kill this pane?")
	c.Type("n")
	c.WaitFor(func(s *harness.Screen) bool { return !strings.Contains(s.String(), "kill this pane?") })
	if n := panes(); n != 2 {
		t.Fatalf("n killed the pane: %d panes", n)
	}

	c.Prefix("x")
	c.WaitForText("kill this pane?")
	c.Type("y")
	c.WaitFor(func(*harness.Screen) bool { return panes() == 1 })
}

// TestKillSessionStaysAttached: prefix+q (after y) kills the session and moves
// the client to another one instead of exiting; the palette's kill-window
// asks first too.
func TestKillSessionStaysAttached(t *testing.T) {
	c := harness.Start(t)
	c.Resize(120, 30)
	c.WaitForStatus("default")
	c.Run("new", "other", "-d")

	c.Prefix(":")
	c.WaitForText("─ commands ─")
	c.TypeLine("kill-window")
	c.WaitForText("kill this window?")
	c.Type("n")
	// The confirm consumes the rest of its input chunk: let it close before the prefix.
	c.WaitFor(func(s *harness.Screen) bool { return !strings.Contains(s.String(), "kill this window?") })

	c.Prefix("q")
	c.WaitForText("kill this session?")
	c.Type("y")
	c.WaitForStatus("other")
	if c.RunErr("has", "default") == nil {
		t.Fatal("session default still exists")
	}

	// detach-on-destroy off was set on the killed session only: a CLI kill of
	// "other" still detaches (the client exits).
	c.Run("new", "third", "-d")
	c.Run("kill", "other")
	time.Sleep(500 * time.Millisecond) // a switch would have re-attached by now
	if list := strings.TrimSpace(c.Run("run", "third", "list-clients")); list != "" {
		t.Fatalf("CLI kill switched the client instead of detaching:\n%s", list)
	}
}

// TestKillSessionTarget: kill-session -t kills the named session, not the one
// the command runs in.
func TestKillSessionTarget(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("default")
	c.Run("new", "victim", "-d")
	c.Run("run", "default", "kill-session", "-t", "victim")
	if c.RunErr("has", "victim") == nil {
		t.Fatal("victim still exists")
	}
	if err := c.RunErr("has", "default"); err != nil {
		t.Fatalf("kill-session -t victim killed default: %v", err)
	}
	if err := c.RunErr("run", "default", "kill-session", "-t", "nope"); err == nil {
		t.Fatal("kill-session -t nope: want an error")
	}
}
