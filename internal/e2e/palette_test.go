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
