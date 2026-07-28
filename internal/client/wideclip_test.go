package client

import (
	"strings"
	"testing"

	"github.com/FyrmForge/gtmux/internal/emu"
	"github.com/FyrmForge/gtmux/internal/proto"
)

// A double-width rune sitting in a pane's LAST column has nowhere to put its
// second cell: the terminal advances two columns on it regardless, so the cell
// it eats is the pane border to its right. buildRow must narrow it to a space.
// (WriteLine skips the placeholder cell after a wide rune; at the pane edge that
// placeholder isn't in the pane at all — the border is.)
func TestWideRuneAtPaneEdgeClipped(t *testing.T) {
	c := newCompositor()
	c.setPhysical(7, 4)
	c.apply(&proto.ServerMsg{
		Layout: &proto.Layout{
			Cols: 7, Rows: 4,
			Panes: []proto.PaneRect{
				{ID: 1, Row: 0, Col: 0, Rows: 4, Cols: 3, Active: true},
				{ID: 2, Row: 0, Col: 4, Rows: 4, Cols: 3},
			},
			Borders: []proto.BorderSeg{{Vertical: true, Fixed: 3, Start: 0, End: 4}},
		},
		Status: &proto.StatusInfo{},
	})

	// Pane 1 is 3 wide. Put a wide rune at its last column (x=2) and one safely
	// inside (x=0, whose placeholder lands at x=1, still in the pane).
	c.apply(&proto.ServerMsg{
		PaneContent: []proto.PaneContent{{
			PaneID: 1,
			Lines: map[int]emu.Line{
				0: {{Char: '🔨'}, {Char: ' '}, {Char: '🔨'}},
			},
		}},
	})

	row := c.buildRow(0)
	if got := row[0].Char; got != '🔨' {
		t.Errorf("col 0 = %q, want the wide rune kept (its second cell fits inside the pane)", got)
	}
	if got := row[2].Char; got != ' ' {
		t.Errorf("col 2 (pane's last column) = %q, want ' ' — a wide rune here would push the border right", got)
	}
	if got := row[3].Char; got != '│' {
		t.Errorf("col 3 = %q, want the border glyph '│' intact", got)
	}

	// End-to-end: the bytes actually emitted for the row must span exactly one
	// column per cell, so the border lands on the column the compositor picked.
	if cols := renderedCols(row); cols != len(row) {
		t.Errorf("row renders to %d columns across %d cells; a wide rune is overflowing its cell", cols, len(row))
	}
}

// renderedCols is how many terminal columns a row's emitted bytes occupy, which
// is what decides where the border actually lands. SGR sequences are interleaved
// between the runs, so they're skipped rather than measured.
func renderedCols(line emu.Line) int {
	s := emu.RenderLine(line)
	n := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b { // ESC [ ... m
			for i < len(s) && s[i] != 'm' {
				i++
			}
			i++
			continue
		}
		r := []rune(s[i:])[0]
		n += emu.Glyph{Char: r}.Width()
		i += len(string(r))
	}
	return n
}

// Copy-mode's cursor is a cell index, so moving right over a wide rune lands it
// on that rune's ' ' placeholder — a cell the renderer skips, which would leave
// the cursor block invisible. It must snap onto the rune's own cell instead.
func TestCopyCursorOnWideRuneStaysVisible(t *testing.T) {
	c := newCompositor()
	c.setPhysical(8, 3)
	c.apply(&proto.ServerMsg{
		Layout: &proto.Layout{Cols: 8, Rows: 3,
			Panes: []proto.PaneRect{{ID: 1, Row: 0, Col: 0, Rows: 3, Cols: 8, Active: true}}},
		Status: &proto.StatusInfo{},
	})
	// Cursor on cell 1 — the placeholder belonging to the wide rune at cell 0.
	c.copy = &copyMode{
		paneID: 1,
		lines:  []emu.Line{{{Char: '🔨'}, {Char: ' '}, {Char: 'a'}}},
		cy:     0, cx: 1, top: 0,
	}

	row := c.buildRow(0)
	if row[0].BG != c.cfg.CopyCursorBG {
		t.Errorf("wide rune's own cell BG = %v, want the copy-cursor BG %v — the cursor landed on the skipped placeholder and rendered nothing",
			row[0].BG, c.cfg.CopyCursorBG)
	}
	// And it must actually survive into the emitted bytes.
	if !strings.Contains(emu.RenderLine(row), "🔨") {
		t.Errorf("wide rune missing from the rendered row: %q", emu.RenderLine(row))
	}
}

// The same guard applies at the right edge of the window content, where the
// neighbouring cell belongs to a docked strip rather than a pane border.
func TestWideRuneAtContentEdgeClipped(t *testing.T) {
	c := newCompositor()
	c.setPhysical(6, 2)
	c.apply(&proto.ServerMsg{
		Layout: &proto.Layout{
			Cols: 6, Rows: 2,
			Panes: []proto.PaneRect{{ID: 1, Row: 0, Col: 0, Rows: 2, Cols: 6, Active: true}},
		},
		Status: &proto.StatusInfo{},
	})
	c.apply(&proto.ServerMsg{
		PaneContent: []proto.PaneContent{{
			PaneID: 1,
			Lines:  map[int]emu.Line{0: {{Char: 'a'}, {Char: 'b'}, {Char: 'c'}, {Char: 'd'}, {Char: 'e'}, {Char: '🔨'}}},
		}},
	})

	if got := c.buildRow(0)[5].Char; got != ' ' {
		t.Errorf("last content column = %q, want ' ' — a wide rune here would overflow the row", got)
	}
}
