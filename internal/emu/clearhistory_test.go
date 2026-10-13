package emu

import (
	"strings"
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

// TestED3ClearsScrollback: ED 3 (sent by `clear`) drops the active buffer's
// scrollback but leaves the screen; in the alt screen it leaves the primary's.
func TestED3ClearsScrollback(t *testing.T) {
	term := New(WithSize(geom.Vec2{R: 2, C: 10}), WithHistoryLimit(100))
	term.Write([]byte("line1\r\nline2\r\nline3\r\nline4"))
	screenBefore := term.String()
	term.Write([]byte("\x1b[3J"))
	if got := len(term.History()); got != 0 {
		t.Fatalf("history after ED 3 = %d lines, want 0", got)
	}
	if term.String() != screenBefore {
		t.Errorf("ED 3 changed the screen:\nbefore %q\nafter  %q", screenBefore, term.String())
	}

	term.Write([]byte("\r\nline5\r\nline6\x1b[?1049h\x1b[3J\x1b[?1049l"))
	if len(term.History()) == 0 {
		t.Error("ED 3 in the alt screen dropped the primary's scrollback")
	}
}

// TestED2SavesScreen: ED 2 pushes the screen's used rows into scrollback
// before clearing (tmux scroll-on-clear); trailing blank rows aren't saved, and
// the alt screen saves nothing.
func TestED2SavesScreen(t *testing.T) {
	term := New(WithSize(geom.Vec2{R: 4, C: 10}), WithHistoryLimit(100))
	term.Write([]byte("a\r\nb\x1b[2J"))
	h := term.History()
	if len(h) != 2 || h[0].String() != "a" || h[1].String() != "b" {
		t.Fatalf("history after ED 2 = %q, want [a b]", h)
	}
	if strings.TrimSpace(term.String()) != "" {
		t.Errorf("screen not cleared: %q", term.String())
	}

	// Last used row wrapped onto a since-erased row: the saved copy must not
	// glue on later output.
	term.Write([]byte("\x1b[H0123456789X\r\x1b[K\x1b[2J\x1b[Hnext\r\n\x1b[2J"))
	h = term.History()
	if got := h[len(h)-1].String(); got != "next" {
		t.Errorf("history tail = %q, want next", got)
	}

	before := len(term.History())
	term.Write([]byte("x\x1b[?1049hy\x1b[2J\x1b[?1049l"))
	if len(term.History()) != before {
		t.Error("ED 2 in the alt screen touched scrollback")
	}
}
