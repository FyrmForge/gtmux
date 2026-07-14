//go:build e2e

package e2e

import (
	"testing"
	"time"

	"github.com/FyrmForge/gtmux/internal/harness"
)

// TestLinkWindow links a window from one session into another and verifies both
// sessions display the SAME live window — the core of winlinks/linked windows.
// Then it unlinks from the borrower (the window survives in the owner) and kills
// it in the owner (the borrower is told to drop its winlink).
func TestLinkWindow(t *testing.T) {
	c := harness.Start(t) // session "default"
	c.WaitForStatus("default")
	c.WaitFor(func(s *harness.Screen) bool { return s.Row(0).String() != "" })

	// A slow live counter in default's only window.
	c.TypeLine("i=0; while true; do echo SHARED$i; i=$((i+1)); sleep 0.02; done")
	c.WaitForText("SHARED1")

	// A second session; link default's window 1 into it.
	peer := c.AttachSession("work")
	peer.WaitForStatus("work")
	peer.Run("run", "work", "link-window", "-s", "default:1")

	// work switched to the linked window and sees the shared counter advancing —
	// it's the same live window, not a copy.
	peer.WaitForText("SHARED")
	pstart := maxCounter(peer.Screen(), "SHARED")
	peer.WaitForUntil(6*time.Second, func(s *harness.Screen) bool {
		return maxCounter(s, "SHARED") > pstart+3
	})
	// default still shows it too, also advancing.
	cstart := maxCounter(c.Screen(), "SHARED")
	c.WaitForUntil(6*time.Second, func(s *harness.Screen) bool {
		return maxCounter(s, "SHARED") > cstart+3
	})

	// Unlink in work: the window goes there but survives in default.
	peer.Run("run", "work", "unlink-window")
	dstart := maxCounter(c.Screen(), "SHARED")
	c.WaitForUntil(6*time.Second, func(s *harness.Screen) bool {
		return maxCounter(s, "SHARED") > dstart+3 // default's copy still live
	})
}

// TestLinkWindowKillPropagates links a window into a second session, then kills
// it in the owner — the borrower must be told to drop its winlink (winlinkGone),
// not left rendering a stopped actor.
func TestLinkWindowKillPropagates(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("default")
	c.WaitFor(func(s *harness.Screen) bool { return s.Row(0).String() != "" })
	c.Prefix("c") // a second window in default so killing the shared one leaves it alive
	c.WaitForStatus("2:")
	c.Prefix("p")
	c.WaitFor(func(s *harness.Screen) bool { return s.ActiveWindow() == 1 })
	c.TypeLine("echo LINKME; while true; do sleep 1; done")
	c.WaitForText("LINKME")

	peer := c.AttachSession("work")
	peer.WaitForStatus("work")
	peer.Run("run", "work", "link-window", "-s", "default:1")
	peer.WaitForText("LINKME")

	// Kill the shared window in default. work must drop it and stay responsive.
	c.Run("run", "default", "kill-window")
	peer.TypeLine("echo WORK-ALIVE")
	peer.WaitForText("WORK-ALIVE")
}
