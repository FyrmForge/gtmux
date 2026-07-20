//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/FyrmForge/gtmux/internal/harness"
)

// epochWithSize parses list-clients output for the epoch of the client with the
// given "[CxR]" size tag. Returns "" if not found.
func epochWithSize(list, size string) string {
	for _, ln := range strings.Split(strings.TrimSpace(list), "\n") {
		if strings.Contains(ln, size) && strings.HasPrefix(ln, "client-") {
			if i := strings.IndexByte(ln, ':'); i > 0 {
				return ln[len("client-"):i]
			}
		}
	}
	return ""
}

// TestChooseClient covers detach-client -t (the command choose-client's picker
// runs): a specific connected client is detached by (session,epoch), the target
// leaves the server's client list, and the other client survives.
func TestChooseClient(t *testing.T) {
	c := harness.Start(t) // 80x24, session default
	c.WaitForStatus("default")
	big := c.NewPeer(190, 9)
	big.WaitForStatus("default")

	// Both clients present; find the peer's epoch by its distinctive size.
	// list-clients reports the window (content) height — the client subtracts its
	// own status row before reporting, so a 24/9-row terminal lists as 23/8.
	list := c.Run("run", "default", "list-clients")
	epoch := epochWithSize(list, "[190x8]")
	if epoch == "" {
		t.Fatalf("peer not in list-clients:\n%s", list)
	}
	if epochWithSize(list, "[80x23]") == "" {
		t.Fatalf("acting client not in list-clients:\n%s", list)
	}

	// Detach exactly that client. Both commands run on default's owner goroutine
	// serially, so the follow-up list-clients already reflects the detach.
	c.Run("run", "default", "detach-client", "-t", "client-"+epoch+"@default")

	list = c.Run("run", "default", "list-clients")
	if epochWithSize(list, "[190x8]") != "" {
		t.Fatalf("detached client still listed:\n%s", list)
	}
	if epochWithSize(list, "[80x23]") == "" {
		t.Fatalf("acting client wrongly detached:\n%s", list)
	}
}

// TestDetachClientAtInName targets a client in a session whose name contains '@'
// — the parse must split on the FIRST '@' (after the numeric epoch), not the
// last, or the session lookup fails.
func TestDetachClientAtInName(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("default")
	peer := c.AttachSession("we@rd")
	peer.WaitForStatus("we@rd")

	list := c.Run("run", "we@rd", "list-clients")
	// Find the epoch of the client in session we@rd.
	epoch := ""
	for _, ln := range strings.Split(strings.TrimSpace(list), "\n") {
		if strings.Contains(ln, "we@rd") && strings.HasPrefix(ln, "client-") {
			if i := strings.IndexByte(ln, ':'); i > 0 {
				epoch = ln[len("client-"):i]
			}
		}
	}
	if epoch == "" {
		t.Fatalf("we@rd client not listed:\n%s", list)
	}

	c.Run("run", "we@rd", "detach-client", "-t", "client-"+epoch+"@we@rd")

	list = c.Run("run", "we@rd", "list-clients")
	for _, ln := range strings.Split(strings.TrimSpace(list), "\n") {
		if strings.Contains(ln, "we@rd") {
			t.Fatalf("client in we@rd not detached (parse split on wrong '@'):\n%s", list)
		}
	}
}

// TestDetachClientUnknownTarget: a -t naming a nonexistent session or epoch is a
// "can't find client" error, not a silent no-op.
func TestDetachClientUnknownTarget(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("default")

	if out := c.Run("run", "default", "detach-client", "-t", "client-1@nosuch"); !strings.Contains(out, "can't find client") {
		t.Fatalf("unknown session: got %q, want a can't-find-client error", out)
	}
	if out := c.Run("run", "default", "detach-client", "-t", "client-999@default"); !strings.Contains(out, "can't find client") {
		t.Fatalf("unknown epoch: got %q, want a can't-find-client error", out)
	}
}
