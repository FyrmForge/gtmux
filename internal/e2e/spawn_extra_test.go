//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/FyrmForge/gtmux/internal/harness"
)

// TestNewWindowName covers `new-window -n <name>`: the window is created with
// that name (no separate rename-window needed).
func TestNewWindowName(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("default")

	c.Run("run", "default", "new-window", "-n", "buildwin")
	out := c.Run("run", "default", "list-windows", "-F", "#{window_name}")
	if !strings.Contains(out, "buildwin") {
		t.Fatalf("new-window -n name not applied; list-windows:\n%s", out)
	}
}

// TestResizePaneAbsolute covers `resize-pane -x <N|N%>`: a left/right split's
// divider moves so the active (right) pane gets the requested width. 80 cols
// wide: -x 60 → right pane 60, divider near col 19; -x 25% → right pane 20,
// divider near col 59.
func TestResizePaneAbsolute(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("default")
	c.Run("run", "default", "split-window", "-h") // two panes, new (right) is active
	c.WaitFor(func(s *harness.Screen) bool { return s.Col(2, '│') > 0 })

	c.Run("run", "default", "resize-pane", "-x", "60")
	c.WaitFor(func(s *harness.Screen) bool { col := s.Col(2, '│'); return col > 0 && col < 25 })

	c.Run("run", "default", "resize-pane", "-x", "25%")
	c.WaitFor(func(s *harness.Screen) bool { return s.Col(2, '│') > 55 })
}
