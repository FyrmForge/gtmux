//go:build e2e

package e2e

import (
	"testing"

	"github.com/FyrmForge/gtmux/internal/harness"
)

// TestKillSessionUnderFlood tears a session down while a pane is actively
// flooding output — the case session-end actor teardown has to survive. If
// stopActor deadlocks on renderCh backpressure the kill never completes and the
// switch below times out; a teardown panic kills the server process outright.
// (The server runs as a plain subprocess, so this observes crashes/hangs, not
// -race — server-side races are covered by go test -race ./internal/server/.)
func TestKillSessionUnderFlood(t *testing.T) {
	c := harness.Start(t) // session "default"
	c.WaitForStatus("default")
	peer := c.AttachSession("work")
	peer.WaitForStatus("work")

	c.WaitFor(func(s *harness.Screen) bool { return s.Row(0).String() != "" })
	c.TypeLine("for i in $(seq 1 200000); do echo FLOOD$i; done")
	c.WaitForText("FLOOD1")

	// detach-on-destroy off: a clean teardown hands c to the surviving session.
	c.Run("run", "default", "set-option", "-g", "detach-on-destroy", "off")
	c.Run("run", "default", "kill-session")
	c.WaitForStatus("work")
}

// TestKillWindowUnderFlood kills a flooding window mid-session (scope B). The
// window's actor is stopped before its panes close (the pipeW-race fix), and the
// reader's in-flight output/exit events land after the stop — the wa.stopped
// guard must drop them rather than send on the closed actor channel. A panic
// there kills the session; a clean run leaves the session alive on window 2.
func TestKillWindowUnderFlood(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.Prefix("c") // second window so the session survives killing the first
	c.WaitForStatus("2:")
	c.Prefix("p") // back to window 1
	c.WaitFor(func(s *harness.Screen) bool { return s.ActiveWindow() == 1 })

	c.TypeLine("for i in $(seq 1 200000); do echo FLOOD$i; done")
	c.WaitForText("FLOOD1")
	c.Run("run", "default", "kill-window") // kills window 1 mid-flood

	// The survivor (window 2, now shown at index 1 since gtmux indexes by slice
	// position) becomes current. A teardown panic would kill the session; instead
	// its shell stays responsive.
	c.WaitFor(func(s *harness.Screen) bool { return !s.Status().Has("2:") })
	c.TypeLine("echo ALIVE-AFTER-KILL")
	c.WaitForText("ALIVE-AFTER-KILL")
}
