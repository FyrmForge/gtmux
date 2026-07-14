//go:build e2e

package e2e

import (
	"testing"
	"time"

	"github.com/FyrmForge/gtmux/internal/harness"
)

// TestSessionGroup creates a session with `new-session -t default`: it joins
// default's group and displays default's current windows as a snapshot. The
// windows are the SAME live actors, so output seen in default shows up in the
// group member too.
func TestSessionGroup(t *testing.T) {
	c := harness.Start(t) // session "default"
	c.WaitForStatus("default")
	c.WaitFor(func(s *harness.Screen) bool { return s.Row(0).String() != "" })

	// A second window in default so the snapshot carries more than one.
	c.Prefix("c")
	c.WaitForStatus("2:")
	c.Prefix("p")
	c.WaitFor(func(s *harness.Screen) bool { return s.ActiveWindow() == 1 })

	// A slow live counter in default's window 1.
	c.TypeLine("i=0; while true; do echo GRP$i; i=$((i+1)); sleep 0.02; done")
	c.WaitForText("GRP1")

	// New session joined to default's group: it borrows default's windows.
	grp := c.AttachGroup("grp", "default")
	grp.WaitForStatus("grp")
	grp.WaitForStatus("2:") // the snapshot carried default's second window too

	// grp shows the shared counter advancing — same live window, not a copy.
	grp.WaitForText("GRP")
	start := maxCounter(grp.Screen(), "GRP")
	grp.WaitForUntil(6*time.Second, func(s *harness.Screen) bool {
		return maxCounter(s, "GRP") > start+3
	})
}
