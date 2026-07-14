//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/FyrmForge/gtmux/internal/harness"
)

// TestNewDetached covers `gtmux new -d`: create a session without attaching. The
// CLI must return (not block on an attach — else c.Run would hang), the session
// must exist, and it must be buildable via `run` for a later attach.
func TestNewDetached(t *testing.T) {
	c := harness.Start(t) // server + one client on "default"
	c.WaitForStatus("default")

	// Detached create — returns immediately (no attach).
	c.Run("new", "-d", "detachedsess")

	// It exists without any client attached to it.
	if err := c.RunErr("has-session", "detachedsess"); err != nil {
		t.Fatalf("detached session not created: %v", err)
	}

	// It runs and is buildable: add a window, expect two.
	c.Run("run", "detachedsess", "new-window")
	out := c.Run("run", "detachedsess", "list-windows")
	if n := strings.Count(strings.TrimSpace(out), "\n") + 1; n != 2 {
		t.Fatalf("expected 2 windows after new-window, got %d:\n%s", n, out)
	}
}
