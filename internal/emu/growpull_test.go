package emu

import (
	"fmt"
	"strings"
	"testing"

	"github.com/FyrmForge/gtmux/internal/geom"
)

// Growing a pane pulls scrolled-off lines back from history, matching tmux
// 3.7c (each case checked against the real binary: 8-row pane zoomed to 24).

func growTerm(rows, cols int) Terminal {
	return New(WithSize(geom.Vec2{R: rows, C: cols}), WithHistoryLimit(1000))
}

func seqOut(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "%d\r\n", i)
	}
	return b.String()
}

func screenRow(term Terminal, r int) string {
	return strings.TrimRight(term.Screen()[r].String(), " ")
}

func TestGrowPullsHistory(t *testing.T) {
	term := growTerm(8, 20)
	term.Write([]byte(seqOut(50) + "$ "))
	hist := len(term.History())

	term.Resize(geom.Vec2{R: 24, C: 20})
	if got := len(term.History()); got != hist-16 {
		t.Fatalf("history = %d, want %d (16 pulled)", got, hist-16)
	}
	if c := term.Cursor(); c.R != 23 || c.C != 2 {
		t.Errorf("cursor = %d,%d, want 23,2", c.R, c.C)
	}
	if screenRow(term, 0) != "28" || screenRow(term, 22) != "50" || screenRow(term, 23) != "$" {
		t.Errorf("screen top %q, row22 %q, row23 %q", screenRow(term, 0), screenRow(term, 22), screenRow(term, 23))
	}

	// zoom/unzoom/zoom round-trip lands in the same place.
	term.Resize(geom.Vec2{R: 8, C: 20})
	term.Resize(geom.Vec2{R: 24, C: 20})
	if screenRow(term, 0) != "28" || term.Cursor().R != 23 {
		t.Errorf("round-trip: top %q cursor row %d", screenRow(term, 0), term.Cursor().R)
	}
}

// Cursor mid-screen: pull only the rows gained; the content below the cursor
// stays below it.
func TestGrowPullsOnlyRowsGained(t *testing.T) {
	term := growTerm(8, 20)
	term.Write([]byte(seqOut(30) + "\x1b[2;1H")) // cursor to row 1
	term.Resize(geom.Vec2{R: 12, C: 20})
	if c := term.Cursor(); c.R != 5 {
		t.Errorf("cursor row = %d, want 5", c.R)
	}
	if screenRow(term, 5) != "25" {
		t.Errorf("cursor row shows %q, want 25", screenRow(term, 5))
	}
}

// ED 2 seals history: a grow after a clear pulls nothing back.
func TestGrowAfterClearPullsNothing(t *testing.T) {
	term := growTerm(8, 20)
	term.Write([]byte(seqOut(50) + "\x1b[H\x1b[2J$ "))
	hist := len(term.History())
	term.Resize(geom.Vec2{R: 24, C: 20})
	if got := len(term.History()); got != hist {
		t.Errorf("history = %d, want %d (nothing pulled)", got, hist)
	}
	if screenRow(term, 0) != "$" || term.Cursor().R != 0 {
		t.Errorf("top %q cursor row %d, want $ at 0", screenRow(term, 0), term.Cursor().R)
	}
	// Output scrolled off after the clear is pullable again.
	term.Write([]byte("\r\n" + seqOut(30) + "$ "))
	term.Resize(geom.Vec2{R: 30, C: 20})
	if screenRow(term, 29) != "$" || screenRow(term, 28) != "30" {
		t.Errorf("after new output: row28 %q row29 %q", screenRow(term, 28), screenRow(term, 29))
	}
}

// A long line taller than the space left is split: its last rows come back,
// its head stays in history (tmux: 1600 A's, 8 -> 12 rows pulls 4 rows of it).
func TestGrowSplitsLongLine(t *testing.T) {
	term := growTerm(8, 10)
	term.Write([]byte("top\r\n" + strings.Repeat("A", 200) + "\r\nx1\r\nx2\r\nx3\r\nx4\r\nx5\r\n$ "))
	term.Resize(geom.Vec2{R: 12, C: 10})
	for r := 0; r < 6; r++ {
		if screenRow(term, r) != strings.Repeat("A", 10) {
			t.Fatalf("row %d = %q, want A's", r, screenRow(term, r))
		}
	}
	if screenRow(term, 6) != "x1" || screenRow(term, 11) != "$" || term.Cursor().R != 11 {
		t.Errorf("row6 %q row11 %q cursor row %d", screenRow(term, 6), screenRow(term, 11), term.Cursor().R)
	}
	h := term.History()
	last := h[len(h)-1]
	if !last.IsWrapped() || len(strings.TrimRight(last.String(), " ")) != 140 {
		t.Errorf("history tail = %d glyphs wrapped=%v, want 140 wrapped", len(last), last.IsWrapped())
	}

	// Shrinking back folds the split line together again.
	term.Resize(geom.Vec2{R: 8, C: 10})
	h = term.History()
	if got := strings.TrimRight(h[len(h)-1].String(), " "); got != strings.Repeat("A", 200) {
		t.Errorf("after shrink, history tail = %d A's, want 200", len(got))
	}
}

// Zoom changes width and height together; pulled lines are re-wrapped to the
// new width, and a blank history line comes back as a blank row.
func TestGrowWiderPullsRewrapped(t *testing.T) {
	term := growTerm(4, 10)
	term.Write([]byte("one\r\n\r\n" + strings.Repeat("B", 15) + "\r\nx1\r\nx2\r\nx3\r\n$ "))
	term.Resize(geom.Vec2{R: 8, C: 20})
	// All three history lines fit; the last row stays blank.
	want := []string{"one", "", strings.Repeat("B", 15), "x1", "x2", "x3", "$", ""}
	for i, w := range want {
		if got := screenRow(term, i); got != w {
			t.Errorf("row %d = %q, want %q", i, got, w)
		}
	}
	if len(term.History()) != 0 || term.Cursor().R != 6 {
		t.Errorf("history %d cursor row %d, want 0 and 6", len(term.History()), term.Cursor().R)
	}
}

// Grown while an app holds the alt screen: the primary gets the pulled lines,
// visible once the app exits.
func TestGrowInAltScreen(t *testing.T) {
	term := growTerm(8, 20)
	term.Write([]byte(seqOut(50) + "$ \x1b[?1049h"))
	term.Resize(geom.Vec2{R: 24, C: 20})
	term.Write([]byte("\x1b[?1049l"))
	if screenRow(term, 0) != "28" || screenRow(term, 23) != "$" || term.Cursor().R != 23 {
		t.Errorf("after alt exit: top %q bottom %q cursor row %d", screenRow(term, 0), screenRow(term, 23), term.Cursor().R)
	}
}
