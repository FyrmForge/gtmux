//go:build e2e

package e2e

import (
	"testing"

	"github.com/FyrmForge/gtmux/internal/harness"
)

// TestNewWindowCommandAndCwd proves new-window runs a given command in a given
// start directory: `new-window -c /tmp <cmd>` spawns the window's pane via
// `shell -c cmd` with cwd=/tmp, so `pwd` prints /tmp and the command's marker
// shows. sleep keeps the pane alive long enough to capture (no remain-on-exit).
func TestNewWindowCommandAndCwd(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.WaitFor(func(s *harness.Screen) bool { return s.Row(0).String() != "" })

	c.Run("run", "default", "new-window", "-c", "/tmp", "echo NEWWIN-MARK; pwd; sleep 30")
	c.WaitForStatus("2:") // the new window exists
	c.WaitForText("NEWWIN-MARK")
	c.WaitForText("/tmp") // pwd ran in the -c start dir
}

// TestSplitWindowCommandAndCwd proves the same threading for split-window:
// `split-window -h -c /tmp <cmd>` opens a second pane running the command in
// /tmp.
func TestSplitWindowCommandAndCwd(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.WaitFor(func(s *harness.Screen) bool { return s.Row(0).String() != "" })

	c.Run("run", "default", "split-window", "-h", "-c", "/tmp", "echo SPLIT-MARK; pwd; sleep 30")
	c.WaitForText("SPLIT-MARK")
	c.WaitForText("/tmp")
}
