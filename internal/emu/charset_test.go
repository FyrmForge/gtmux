package emu

import "testing"

func row(t *testing.T, term Terminal, y, n int) string {
	t.Helper()
	out := make([]rune, n)
	for x := 0; x < n; x++ {
		out[x] = rune(term.Cell(x, y).Char)
	}
	return string(out)
}

// tcell's enacs (ESC ( B ESC ) 0) designates G1 without activating it — text
// must stay ASCII. This is the lazygit glyph-soup bug.
func TestEnacsDoesNotActivateG1(t *testing.T) {
	term := New()
	term.Write([]byte("\x1b(B\x1b)0master"))
	if got := row(t, term, 0, 6); got != "master" {
		t.Fatalf("after enacs got %q, want master", got)
	}
}

// ncurses ACS via G0: ESC ( 0 activates line drawing immediately, ESC ( B
// returns to ASCII — 'q' draws as '─' in between.
func TestG0LineDrawing(t *testing.T) {
	term := New()
	term.Write([]byte("\x1b(0qq\x1b(Bqq"))
	if got := row(t, term, 0, 4); got != "──qq" {
		t.Fatalf("got %q, want ──qq", got)
	}
}

// G1 via SO/SI: designate G1 to line drawing, SO activates it, SI returns to
// G0 (ASCII) — the screen/tmux-terminfo smacs/rmacs path.
func TestShiftOutIn(t *testing.T) {
	term := New()
	term.Write([]byte("\x1b)0q\x0eq\x0fq"))
	if got := row(t, term, 0, 3); got != "q─q" {
		t.Fatalf("got %q, want q─q", got)
	}
}
