//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/FyrmForge/gtmux/internal/harness"
)

// TestUpgradeInPlace: `gtmux upgrade` re-execs the daemon with its PTYs
// inherited. An attached client reconnects on its own, the shell in the pane is
// the same process (its state survives: a variable set before the upgrade is
// readable after), and the split layout + scrollback come back.
func TestUpgradeInPlace(t *testing.T) {
	c := harness.Start(t)
	c.WaitFor(func(s *harness.Screen) bool { return s.Row(0).String() != "" })
	c.TypeLine("echo MARK-1")
	c.WaitForText("MARK-1")
	c.Prefix("%") // split: the new (right) pane becomes active
	c.WaitForStatus("default")
	c.TypeLine("BEFORE=survived") // set in the pane that stays active across upgrade

	if out := c.Run("upgrade"); strings.Contains(out, "error") {
		t.Fatalf("upgrade: %s", out)
	}
	// The client reconnected onto the new image: pre-upgrade scrollback is
	// replayed and both panes came back.
	c.WaitForText("MARK-1")
	if panes := c.Run("run", "default", "list-panes"); strings.Count(panes, "%") != 2 {
		t.Fatalf("expected 2 panes after upgrade, got:\n%s", panes)
	}
	// The active pane's shell is the same live process — its env survived, so a
	// variable set before the upgrade is still readable (same PID, not a respawn).
	c.TypeLine("echo VAR-$BEFORE")
	c.WaitForText("VAR-survived")
}
