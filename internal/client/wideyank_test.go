package client

import (
	"testing"

	"github.com/FyrmForge/gtmux/internal/emu"
)

// wideLine builds the grid for "🔨ab c": a double-width rune occupies two cells
// (itself plus a ' ' placeholder), so the cells are:
//
//	0:'🔨' 1:' '(placeholder) 2:'a' 3:'b' 4:' '(real space) 5:'c'
func wideLine() emu.Line {
	return emu.Line{
		{Char: '🔨'}, {Char: ' '}, {Char: 'a'}, {Char: 'b'}, {Char: ' '}, {Char: 'c'},
	}
}

// Yanked text must not carry the wide rune's placeholder cell — that ' ' is a
// grid artifact keeping cell indices aligned, not something the user selected.
// A REAL space elsewhere on the line must survive.
func TestYankDropsWidePlaceholderKeepsRealSpace(t *testing.T) {
	cm := &copyMode{
		lines: []emu.Line{wideLine()},
		selY:  0, selX: 0, cy: 0, cx: 5, // whole line
	}
	if got, want := cm.selectedText(), "🔨ab c"; got != want {
		t.Errorf("yanked %q, want %q", got, want)
	}
}

// The same for a rectangle selection, which renders through its own branch.
func TestYankRectDropsWidePlaceholder(t *testing.T) {
	cm := &copyMode{
		lines: []emu.Line{wideLine()},
		selY:  0, selX: 0, cy: 0, cx: 3, rectSel: true,
	}
	if got, want := cm.selectedText(), "🔨ab"; got != want {
		t.Errorf("rect-yanked %q, want %q", got, want)
	}
}

// A selection starting after the wide rune is unaffected — the placeholder isn't
// in range, and nothing else shifts.
func TestYankAfterWideRuneUnchanged(t *testing.T) {
	cm := &copyMode{
		lines: []emu.Line{wideLine()},
		selY:  0, selX: 2, cy: 0, cx: 5,
	}
	if got, want := cm.selectedText(), "ab c"; got != want {
		t.Errorf("yanked %q, want %q", got, want)
	}
}

// Search matches against the line's text, so a query spanning a wide rune finds
// it — and the recorded match is a CELL index, because that's what moves the
// cursor.
func TestSearchAcrossWideRune(t *testing.T) {
	cm := &copyMode{lines: []emu.Line{wideLine()}}
	cm.runSearch("🔨a")
	if len(cm.matches) != 1 {
		t.Fatalf("matches = %v, want exactly one hit for %q", cm.matches, "🔨a")
	}
	if got := cm.matches[0]; got != [2]int{0, 0} {
		t.Errorf("match at %v, want row 0 cell 0 (the wide rune's own cell)", got)
	}
}

// A match sitting AFTER a wide rune must report the cell index, not the text
// index — off-by-one here would park the cursor on the wrong column.
func TestSearchAfterWideRuneMapsToCellIndex(t *testing.T) {
	cm := &copyMode{lines: []emu.Line{wideLine()}}
	cm.runSearch("b")
	if len(cm.matches) != 1 {
		t.Fatalf("matches = %v, want one hit", cm.matches)
	}
	// 'b' is text index 2 but cell index 3 — the placeholder sits between them.
	if got := cm.matches[0]; got != [2]int{0, 3} {
		t.Errorf("match at %v, want row 0 cell 3 (text index 2 + the placeholder)", got)
	}
}

// One `l` from the emoji puts the cursor on its placeholder cell — and the
// cursor BLOCK renders on the emoji there (buildCopyRow snaps it). So anchoring
// a selection at that spot must take the emoji, or the user yanks something
// different from what they saw highlighted.
func TestYankAnchoredOnPlaceholderTakesWideRune(t *testing.T) {
	cm := &copyMode{lines: []emu.Line{wideLine()}, selY: 0, selX: 1, cy: 0, cx: 3}
	if got, want := cm.selectedText(), "🔨ab"; got != want {
		t.Errorf("yanked %q, want %q — selection anchored on the placeholder dropped the rune", got, want)
	}
	// The highlight must agree: the emoji's own cell is in the selection.
	if !cm.inSelection(0, 0) {
		t.Error("emoji cell not highlighted, but it IS yanked — highlight and copy disagree")
	}
}

// The same when the CURSOR (not the anchor) is the low edge — selecting leftward
// onto the placeholder.
func TestYankCursorLowEdgeOnPlaceholder(t *testing.T) {
	cm := &copyMode{lines: []emu.Line{wideLine()}, selY: 0, selX: 3, cy: 0, cx: 1}
	if got, want := cm.selectedText(), "🔨ab"; got != want {
		t.Errorf("yanked %q, want %q", got, want)
	}
	if !cm.inSelection(0, 0) {
		t.Error("emoji cell not highlighted, but it IS yanked — highlight and copy disagree")
	}
}

// And for a rectangle whose left edge lands on a placeholder column.
func TestYankRectLeftEdgeOnPlaceholder(t *testing.T) {
	cm := &copyMode{lines: []emu.Line{wideLine()}, selY: 0, selX: 1, cy: 0, cx: 3, rectSel: true}
	if got, want := cm.selectedText(), "🔨ab"; got != want {
		t.Errorf("rect-yanked %q, want %q", got, want)
	}
	if !cm.inSelection(0, 0) {
		t.Error("emoji cell not highlighted in the rectangle, but it IS yanked")
	}
}

// Lines with no wide runes must behave exactly as before.
func TestYankAndSearchNarrowUnchanged(t *testing.T) {
	line := emu.Line{{Char: 'a'}, {Char: ' '}, {Char: 'b'}}
	cm := &copyMode{lines: []emu.Line{line}, selY: 0, selX: 0, cy: 0, cx: 2}
	if got, want := cm.selectedText(), "a b"; got != want {
		t.Errorf("yanked %q, want %q (real space preserved)", got, want)
	}
	cm.runSearch("b")
	if len(cm.matches) != 1 || cm.matches[0] != [2]int{0, 2} {
		t.Errorf("matches = %v, want one hit at cell 2", cm.matches)
	}
}
