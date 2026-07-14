//go:build e2e

package e2e

import (
	"testing"
	"time"

	"github.com/FyrmForge/gtmux/internal/harness"
)

// TestSharedWindowSmallest links one window into two sessions of different
// widths under window-size smallest: the window sizes to the smaller viewer, so
// the wider session dot-fills its slack columns. Without actor-owned sizing the
// two sessions fight (last resize wins); with it, smallest is stable.
func TestSharedWindowSmallest(t *testing.T) {
	c := harness.Start(t) // default, 80 wide
	c.WaitForStatus("default")
	c.WaitFor(func(s *harness.Screen) bool { return s.Row(0).String() != "" })
	c.Run("run", "default", "set-option", "window-size", "smallest")

	peer := c.AttachSession("work") // 80x24 -> shrink to 60 wide
	peer.WaitForStatus("work")
	peer.Resize(60, 20)
	peer.WaitForStatus("work")
	peer.Run("run", "work", "set-option", "window-size", "smallest")

	// Share default's window into work: smallest -> 60 wide, so default (80 wide)
	// dot-fills cols 60-79.
	peer.Run("run", "work", "link-window", "-s", "default:1")
	c.WaitForUntil(6*time.Second, func(s *harness.Screen) bool {
		return s.Cell(0, 70).Char == '·'
	})
}

// TestAggressiveResize proves aggressive-resize: a shared window that is not the
// current window in the smaller session stops counting that session once
// aggressive-resize is on there, so the window grows back to the larger viewer.
func TestAggressiveResize(t *testing.T) {
	c := harness.Start(t) // default, 80 wide; its window 1 is the one we share
	c.WaitForStatus("default")
	c.WaitFor(func(s *harness.Screen) bool { return s.Row(0).String() != "" })
	c.Run("run", "default", "set-option", "window-size", "smallest")

	peer := c.AttachSession("work") // 80x24 -> 60 wide
	peer.WaitForStatus("work")
	peer.Resize(60, 20)
	peer.WaitForStatus("work")
	peer.Run("run", "work", "set-option", "window-size", "smallest")

	// Link default:1 into work (its window 2, current there), then switch work
	// back to its own window 1 so the shared window is background in work.
	peer.Run("run", "work", "link-window", "-s", "default:1")
	peer.Run("run", "work", "select-window", "-t", "1")

	// aggressive-resize off (default): work still counts even though the shared
	// window isn't current there -> 60 wide, default dot-fills col 70.
	c.WaitForUntil(6*time.Second, func(s *harness.Screen) bool {
		return s.Cell(0, 70).Char == '·'
	})

	// aggressive-resize on in work: the background shared window no longer counts
	// work -> grows to default's 80, default's dot-fill clears.
	peer.Run("run", "work", "set-option", "aggressive-resize", "on")
	c.WaitForUntil(6*time.Second, func(s *harness.Screen) bool {
		return s.Cell(0, 70).Char != '·'
	})
}
