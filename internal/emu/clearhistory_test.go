package emu

import (
	"testing"

	"github.com/FyrmForge/gtmux/internal/geom"
)

// TestClearHistory verifies clear-history drops the scrollback while leaving the
// visible screen untouched.
func TestClearHistory(t *testing.T) {
	term := New(WithSize(geom.Vec2{R: 2, C: 10}), WithHistoryLimit(100))
	// More lines than the 2-row screen, so the earliest scroll into history.
	term.Write([]byte("line1\r\nline2\r\nline3\r\nline4\r\n"))
	if len(term.History()) == 0 {
		t.Fatal("expected scrollback before clear")
	}
	screenBefore := term.String()

	term.ClearHistory()

	if got := len(term.History()); got != 0 {
		t.Fatalf("history after clear = %d lines, want 0", got)
	}
	if term.String() != screenBefore {
		t.Errorf("visible screen changed after clear-history:\nbefore %q\nafter  %q", screenBefore, term.String())
	}
}
