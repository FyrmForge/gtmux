//go:build e2e

package e2e

import (
	"testing"

	"github.com/FyrmForge/gtmux/internal/harness"
)

// select-pane -Z (the next_pane/prev_pane responsive idiom): cycling to the
// next pane while zoomed keeps the window zoomed — the zoom follows the new
// active pane instead of dropping back to tiles.
func TestSelectPaneKeepZoom(t *testing.T) {
	c := harness.Start(t)
	promptReady(c)
	c.TypeLine("echo first-pane")
	c.WaitForText("first-pane")

	c.Run("run", "default", "split-window", "-h") // side by side -> │ divider
	divider := func(s *harness.Screen) bool { return s.Col(1, '│') >= 0 }
	c.WaitFor(divider)
	c.TypeLine("echo second-pane") // new pane has focus
	c.WaitForText("second-pane")

	c.Run("run", "default", "resize-pane", "-Z") // zoom the second pane
	c.WaitFor(func(s *harness.Screen) bool { return !divider(s) })

	// Cycle with -Z: the FIRST pane's content must fill the window — still no
	// divider — instead of the layout un-tiling.
	c.Run("run", "default", "select-pane", "-t", ":.+", "-Z")
	c.WaitForText("first-pane")
	c.WaitFor(func(s *harness.Screen) bool { return !divider(s) })

	// Plain select-pane (no -Z) on a zoomed window stays tmux-like via the
	// unzoom-on-navigate paths elsewhere; -Z is the only addition under test.
	// Un-zoom and confirm the tiled layout comes back.
	c.Run("run", "default", "resize-pane", "-Z")
	c.WaitFor(divider)
}
