package client

import (
	"testing"

	"github.com/FyrmForge/gtmux/internal/emu"
)

func mkLine(s string) emu.Line {
	l := make(emu.Line, len(s))
	for i, r := range []rune(s) {
		l[i] = emu.Glyph{Char: r}
	}
	return l
}

func mkCopyMode(rows int, lines ...string) *copyMode {
	cm := &copyMode{rows: rows}
	for _, s := range lines {
		cm.lines = append(cm.lines, mkLine(s))
	}
	return cm
}

// Reverse search (?) jumps to the last match at/before the cursor, and n/N
// repeat relative to the search direction.
func TestCopyModeReverseSearch(t *testing.T) {
	cm := mkCopyMode(10, "foo", "bar foo", "baz", "foo end")
	cm.cy, cm.cx = 3, 2 // start partway down the last line

	// reverse search for "foo": nearest match at/before cursor is (3,0).
	cm.searchFwd = false
	cm.runSearch("foo")
	if cm.cy != 3 || cm.cx != 0 {
		t.Fatalf("reverse search landed at (%d,%d), want (3,0)", cm.cy, cm.cx)
	}
	// n continues the reverse direction: previous match upward is (1,4).
	cm.jumpMatch(cm.searchFwd)
	if cm.cy != 1 || cm.cx != 4 {
		t.Fatalf("reverse n landed at (%d,%d), want (1,4)", cm.cy, cm.cx)
	}
	// N reverses it, going forward/downward again to (3,0).
	cm.jumpMatch(!cm.searchFwd)
	if cm.cy != 3 || cm.cx != 0 {
		t.Fatalf("reverse N landed at (%d,%d), want (3,0)", cm.cy, cm.cx)
	}
}

// Forward search skips the match under the cursor and wraps.
func TestCopyModeForwardSearch(t *testing.T) {
	cm := mkCopyMode(10, "foo", "foo", "foo")
	cm.cy, cm.cx = 0, 0
	cm.searchFwd = true
	cm.runSearch("foo") // cursor sits on (0,0); forward jumps past it to (1,0)
	if cm.cy != 1 || cm.cx != 0 {
		t.Fatalf("forward search landed at (%d,%d), want (1,0)", cm.cy, cm.cx)
	}
}

// Rectangle selection yanks a column block, not full lines.
func TestCopyModeRectangleSelect(t *testing.T) {
	cm := mkCopyMode(10, "abcde", "fghij", "klmno")
	cm.cy, cm.cx = 0, 1
	cm.handleByte(0x16) // C-v: start rectangle at (0,1)
	if !cm.rectSel || !cm.selecting {
		t.Fatal("C-v did not start a rectangle selection")
	}
	cm.cy, cm.cx = 2, 3 // drag to (2,3): columns 1..3, rows 0..2
	got := cm.selectedText()
	want := "bcd\nghi\nlmn"
	if got != want {
		t.Fatalf("rectangle selectedText = %q, want %q", got, want)
	}
	if !cm.inSelection(1, 2) || cm.inSelection(1, 0) || cm.inSelection(1, 4) {
		t.Fatal("rectangle inSelection bounds wrong")
	}
}

// Emacs keytable: control keys move via emacsCtrl, Meta keys (ESC+letter) via
// feed's emacsMeta. The M-v/page-up collision (0x02) must not re-translate.
func TestCopyModeEmacsKeys(t *testing.T) {
	cm := mkCopyMode(10, "one two three", "four five six", "seven eight")
	cm.emacs = true

	cm.handleByte(0x0e) // C-n: down
	if cm.cy != 1 {
		t.Fatalf("C-n cy = %d, want 1", cm.cy)
	}
	cm.handleByte(0x05) // C-e: line end
	if cm.cx != len("four five six")-1 {
		t.Fatalf("C-e cx = %d, want %d", cm.cx, len("four five six")-1)
	}
	cm.handleByte(0x01) // C-a: line start
	if cm.cx != 0 {
		t.Fatalf("C-a cx = %d, want 0", cm.cx)
	}
	cm.feed([]byte{0x1b, 'f'}) // M-f: word forward -> start of "five"
	if cm.cx != 5 {
		t.Fatalf("M-f cx = %d, want 5", cm.cx)
	}

	// M-v is page-up (0x02); it must not get re-mapped to 'h' (left) by emacsCtrl.
	cm.cy, cm.cx = 2, 3
	cm.feed([]byte{0x1b, 'v'})
	if cm.cx != 3 {
		t.Fatalf("M-v moved cx to %d; expected no left-move (page-up only)", cm.cx)
	}
}

// Word motions honor word-separators: with punctuation as a boundary,
// "alpha.beta gamma" is three words; without, the dot is part of one word.
func TestCopyModeWordSeparators(t *testing.T) {
	const line = "alpha.beta gamma" // alpha=0-4 .=5 beta=6-9 space=10 gamma=11-15

	withSep := mkCopyMode(10, line)
	withSep.wordSep = "!\"#$%&'()*+,-./:;<=>?@[\\]^\x60{|}~" // tmux default
	withSep.cy, withSep.cx = 0, 0
	withSep.wordForward()
	if withSep.cx != 6 {
		t.Fatalf("with separators: w landed at cx=%d, want 6 (beta)", withSep.cx)
	}
	withSep.cx = 11
	withSep.wordBack()
	if withSep.cx != 6 {
		t.Fatalf("with separators: b landed at cx=%d, want 6 (beta)", withSep.cx)
	}

	noSep := mkCopyMode(10, line) // wordSep "" => whitespace-only boundaries
	noSep.cy, noSep.cx = 0, 0
	noSep.wordForward()
	if noSep.cx != 11 {
		t.Fatalf("no separators: w landed at cx=%d, want 11 (gamma)", noSep.cx)
	}
}
