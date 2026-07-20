package emu

import (
	"testing"

	"github.com/FyrmForge/gtmux/internal/geom"
)

// OSC 133 ; D ; <code> ST records a CommandFinished event carrying the exit
// code — the signal the server drains for gtmux.on("command-exited"). A bare
// D with no code yields a nil ExitCode (server treats it as 0).
func TestOSC133CommandFinished(t *testing.T) {
	term := New(WithSize(geom.Vec2{R: 5, C: 20}))
	if _, err := term.Write([]byte("\033]133;D;1\033\\")); err != nil {
		t.Fatal(err)
	}
	got := term.Changes().GetSemanticPrompts()
	if len(got) != 1 || got[0].Type != CommandFinished {
		t.Fatalf("events = %+v, want one CommandFinished", got)
	}
	if got[0].ExitCode == nil || *got[0].ExitCode != 1 {
		t.Fatalf("exit code = %v, want 1", got[0].ExitCode)
	}

	// ClearSemanticPrompts drops it without touching the (separately reset) grid.
	term.Changes().ClearSemanticPrompts()
	if n := len(term.Changes().GetSemanticPrompts()); n != 0 {
		t.Fatalf("after clear: %d events, want 0", n)
	}
}
