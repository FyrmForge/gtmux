//go:build e2e

// Package e2e holds full-stack gtmux tests that spawn real server+client
// subprocesses. They're behind the `e2e` build tag so plain `go test ./...`
// stays unit-only and fast. Run them with:
//
//	go test -tags=e2e ./internal/e2e/
package e2e

import (
	"testing"

	"github.com/FyrmForge/gtmux/internal/harness"
)

// TestSmokeEcho proves the whole pipe: build -> spawn server+client -> pty ->
// emu-interpret -> WaitFor. A keystroke reaches the pane's shell and its output
// shows up on the rendered screen.
func TestSmokeEcho(t *testing.T) {
	c := harness.Start(t)
	// Wait for the shell prompt to draw on the top row (not just the status
	// bar, which renders immediately).
	c.WaitFor(func(s *harness.Screen) bool { return s.Row(0).String() != "" })
	c.TypeLine("echo SMOKE-OK")
	c.WaitForText("SMOKE-OK")
}

// TestWindowCreate covers a prefix binding end to end: prefix+c adds a second
// window, reflected in the status bar.
func TestWindowCreate(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.Prefix("c")
	c.WaitForStatus("2:")
}

// TestMultiClientDotFill covers window-size "latest" plus the dot-fill: a
// larger peer becomes the acting client, so the original smaller client can't
// show the whole window and dot-fills the slack below the window.
func TestMultiClientDotFill(t *testing.T) {
	c := harness.Start(t) // 80x24
	c.WaitForStatus("default")

	big := c.NewPeer(190, 9) // wider + shorter; attaching makes it acting
	big.WaitForStatus("default")

	// On the 80x24 client the window is now 190x9: rows at/below the window's
	// 8 content rows are outside it, so they dot-fill.
	c.WaitFor(func(s *harness.Screen) bool { return s.Cell(10, 0).Char == '·' })
}
