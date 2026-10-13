//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/FyrmForge/gtmux/internal/harness"
)

// TestFindSession drives gtmux.find_session end to end: the overlay opens with
// a filter line, a fuzzy query narrows the list (substring hit first), C-n
// moves within it, and Enter switches the client to the chosen session.
func TestFindSession(t *testing.T) {
	c := harness.StartWithConfig(t, `gtmux.bind("f", gtmux.find_session)`, "")
	c.WaitForStatus("default")
	for _, s := range []string{"gtmux-dev", "notes", "gt-x"} {
		c.Run("new", s, "-d")
	}

	c.Prefix("f")
	c.WaitForText("❯")
	c.Type("gx")
	c.WaitForText("❯ gx")
	c.WaitFor(func(s *harness.Screen) bool {
		txt := s.String()
		x, dev := strings.Index(txt, "gt-x"), strings.Index(txt, "gtmux-dev")
		return !strings.Contains(txt, "notes") && x >= 0 && dev > x
	})

	c.Ctrl('n') // gt-x (substring) -> gtmux-dev (subsequence)
	c.Type("\r")
	c.WaitForStatus("gtmux-dev")
}

// TestFindSessionDetachRestoresCursor: the finder switches the terminal to a
// blinking bar cursor; if the client is detached while it's open (here the
// session is killed underneath it), the exit path still restores the default.
func TestFindSessionDetachRestoresCursor(t *testing.T) {
	c := harness.StartWithConfig(t, `gtmux.bind("f", gtmux.find_session)`, "")
	c.WaitForStatus("default")
	c.Prefix("f")
	c.WaitForText("❯")
	c.WaitForRaw([]byte("\x1b[5 q"))
	if c.RawContains([]byte("\x1b[0 q")) {
		t.Fatal("cursor reset before the finder closed")
	}
	c.Run("kill", "default")
	c.WaitForRaw([]byte("\x1b[0 q"))
}

// TestFindSessionOverDock: the finder spans the whole terminal, painting over a
// left dock instead of being pushed right into the content area beside it.
func TestFindSessionOverDock(t *testing.T) {
	c := harness.StartWithConfig(t, `require("gtmux.sidebar"){ size = 25, min_cols = 0 }
gtmux.bind("f", gtmux.find_session)`, "")
	c.Resize(140, 30)
	c.WaitForText("SESSIONS")
	c.Prefix("f")
	c.WaitForText("❯")
	c.WaitFor(func(s *harness.Screen) bool {
		for r := 0; r < 30; r++ {
			if col := s.Col(r, '╭'); col >= 0 && col < 25 && strings.Contains(s.Row(r).String(), "session") {
				return true // list box's top-left corner sits inside the dock strip
			}
		}
		return false
	})
}

// TestFindSessionTransparentBeside: the cells beside the narrower input box
// show what's behind (here the sidebar dock's header), not blanked cells.
func TestFindSessionTransparentBeside(t *testing.T) {
	c := harness.StartWithConfig(t, `require("gtmux.sidebar"){ size = 25, min_cols = 0 }
gtmux.bind("f", gtmux.find_session)`, "")
	c.Resize(140, 30)
	c.WaitForText("SESSIONS")
	c.Prefix("f")
	c.WaitForText("❯")
	c.WaitFor(func(s *harness.Screen) bool {
		// The input box's top border shares its row with the dock's SESSIONS
		// header, which sits inside the panel's margin left of the box.
		for r := 0; r < 30; r++ {
			if row := s.Row(r).String(); strings.Contains(row, "find session") {
				return strings.Contains(row, "SESSIONS")
			}
		}
		return false
	})
}

// TestFindSessionResponsive: on a wide terminal the panel stops at its width
// cap (centered, not edge to edge); on a narrow one the preview box drops out
// and the list takes the panel.
func TestFindSessionResponsive(t *testing.T) {
	c := harness.StartWithConfig(t, `gtmux.bind("f", gtmux.find_session)`, "")
	c.Resize(220, 40)
	c.WaitForStatus("default")
	c.Prefix("f")
	c.WaitForText("❯")
	c.WaitFor(func(s *harness.Screen) bool {
		for r := 0; r < 40; r++ {
			if row := s.Row(r).String(); strings.Contains(row, "─ session ─") {
				l, rr := strings.IndexRune(row, '╭'), strings.LastIndex(row, "╮")
				w := len([]rune(row[l:rr])) + 1
				return w <= 120 && l > 40 // capped and centered
			}
		}
		return false
	})
	c.Resize(50, 20)
	c.WaitFor(func(s *harness.Screen) bool {
		txt := s.String()
		return strings.Contains(txt, "─ session ─") && strings.Count(txt, "╭") == 2 // input + list, no preview
	})
}
