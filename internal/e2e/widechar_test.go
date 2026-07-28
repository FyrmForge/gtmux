//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/FyrmForge/gtmux/internal/harness"
)

// A pane printing a double-width rune must not push the pane border sideways.
// The rune occupies two grid cells (the rune plus a ' ' placeholder), but the
// terminal advances two columns on the rune alone — so emitting the placeholder
// too shifted the rest of the physical row one column right, drawing the pane's
// content over the border. This is what hamr's "🔨" dev bar did to the divider:
// on the emoji's row the border vanished.
//
// The harness re-parses the client's actual output bytes through an emulator, so
// this asserts on where the border really lands, not on the compositor's intent.
func TestWideRuneDoesNotShiftBorder(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.WaitFor(func(s *harness.Screen) bool { return s.Row(0).String() != "" })

	// Two side-by-side panes; the divider's column is the thing under test.
	c.Run("run", "default", "split-window", "-h", "sleep 30")
	c.WaitFor(func(s *harness.Screen) bool { return s.Col(0, '│') > 0 })
	divider := c.Screen().Col(0, '│')

	// Focus the original (left) shell — the split left focus on the new pane —
	// so the wide runes land to the LEFT of the divider, where their overflow
	// would run into it.
	c.PrefixArrow("left")
	c.TypeLine("printf 'WIDE:\U0001F528\U0001F528 done\\n'")
	c.WaitForText("WIDE:")

	// Assert on the rows that actually carry the wide runes; the shell prompt's
	// height varies by environment, so find them rather than hardcoding.
	s := c.Screen()
	var checked int
	for row := 0; row < 10; row++ {
		if !strings.Contains(s.Row(row).String(), "WIDE:") {
			continue
		}
		checked++
		if got := s.Cell(row, divider).Char; got != '│' {
			t.Errorf("row %d carries a wide rune: cell at divider column %d = %q, want '│' — the wide rune shifted the row and swallowed the border\n%s",
				row, divider, got, s.String())
		}
	}
	if checked == 0 {
		t.Fatalf("no row carried the wide runes; the test proved nothing\n%s", s.String())
	}
}
