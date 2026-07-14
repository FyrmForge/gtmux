//go:build e2e

package e2e

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/gtmux/internal/harness"
)

// TestBreakPaneUnderFlood breaks an actively-flooding pane into its own window.
// The pane changes window actors mid-stream, so this is the case the reader
// retargeting has to get right: run under -race and the output for the moved
// pane must keep rendering in its new home. Before the session-routed fix the
// reader kept posting to the old actor — a data race on the pane's grid and a
// pane that never redrew in the new window.
func TestBreakPaneUnderFlood(t *testing.T) {
	c := harness.Start(t)
	c.WaitFor(func(s *harness.Screen) bool { return s.Row(0).String() != "" })

	// Split so the window has two panes; the new (active) pane is what we break.
	c.Prefix("%")
	c.WaitFor(func(s *harness.Screen) bool { return s.Col(3, '│') >= 0 })

	// Start a long flood in the active pane, then break it while it streams.
	c.TypeLine("for i in $(seq 1 200000); do echo FLOOD$i; done")
	c.WaitForText("FLOOD1")
	c.Prefix("!")

	// The flooding pane is now window 2 and must render there live.
	c.WaitForStatus("2:")
	c.WaitForText("FLOOD")
}

// TestBreakThenKillOrigin breaks a flooding pane into its own window, then kills
// the ORIGIN window (which still relays the moved pane's output — reader→origin→
// new actor). The origin becomes a relay-only zombie rather than stopping, so the
// moved pane must keep flowing; killing its origin must not freeze it. This is the
// case the origin-relay model exists to handle (vs. the session-routing it
// replaced, which froze a shared window when its creator ended).
func TestBreakThenKillOrigin(t *testing.T) {
	c := harness.Start(t)
	c.WaitFor(func(s *harness.Screen) bool { return s.Row(0).String() != "" })

	// Window 1 with two panes; endlessly flood the active (bottom) one (an
	// incrementing counter so the screen visibly changes), then break it out.
	c.Prefix("%")
	c.WaitFor(func(s *harness.Screen) bool { return s.Col(3, '│') >= 0 })
	c.TypeLine("i=0; while true; do echo FLOOD$i; i=$((i+1)); done")
	c.WaitForText("FLOOD1")
	c.Prefix("!") // break the flooding pane into window 2 (origin = window 1)
	c.WaitForStatus("2:")

	// Back to window 1 (still has the other pane, and relays the moved one).
	c.Prefix("p")
	c.WaitFor(func(s *harness.Screen) bool { return s.ActiveWindow() == 1 })
	// Kill window 1 — its actor must stay alive as a relay zombie for the moved pane.
	c.Run("run", "default", "kill-window")

	// Session lands on the survivor (the broken-out flooding window); its relayed
	// flood must be rendering there.
	c.WaitForText("FLOOD")
	start := maxFlood(c.Screen())
	// Forward progress well past the kill point proves the relayed pane is still
	// flowing; a freeze would pin the counter at `start`.
	c.WaitForUntil(8*time.Second, func(s *harness.Screen) bool { return maxFlood(s) > start+100 })
}

// maxFlood returns the highest N among "FLOOD<N>" lines on screen, or -1.
func maxFlood(s *harness.Screen) int { return maxCounter(s, "FLOOD") }

// maxCounter returns the highest N among "<prefix><N>" lines on screen, or -1 —
// a robust "is this counter still advancing" probe under bursty rendering.
func maxCounter(s *harness.Screen, prefix string) int {
	max := -1
	for _, line := range strings.Split(s.String(), "\n") {
		if i := strings.LastIndex(line, prefix); i >= 0 {
			if n, err := strconv.Atoi(strings.TrimSpace(line[i+len(prefix):])); err == nil && n > max {
				max = n
			}
		}
	}
	return max
}
