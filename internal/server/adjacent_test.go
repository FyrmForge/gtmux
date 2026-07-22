package server

import "testing"

// Left pane full-height, two stacked on the right. From bottom-right, go left
// then right: tmux returns to the bottom-right (the most-recently-active
// neighbor), not whichever right pane is biggest. The MRU tiebreak
// (pane.activePoint) is what makes left→right remember where you came from.
func TestAdjacentRemembersMRU(t *testing.T) {
	//  cols 0..9 = L, divider at 10, cols 11..21 = right column
	//  rows 0..4 = TR, divider at 5, rows 6..11 = BR
	l := &pane{id: 1, rect: rect{Row: 0, Col: 0, Rows: 12, Cols: 10}}
	tr := &pane{id: 2, rect: rect{Row: 0, Col: 11, Rows: 5, Cols: 11}}
	br := &pane{id: 3, rect: rect{Row: 6, Col: 11, Rows: 6, Cols: 11}}
	w := &window{panes: []*pane{l, tr, br}}

	// Simulate history: TR active at some point, then BR (BR is more recent).
	w.setActive(tr)
	w.setActive(br)

	// From BR, go left → must reach L (the only left neighbor).
	if got := w.adjacent("left"); got != l {
		t.Fatalf("left from BR = %v, want L", id(got))
	}
	w.setActive(l)

	// From L, go right → both TR and BR are valid; MRU picks BR (activated after
	// TR). The old overlap tiebreak would pick whichever is taller.
	if got := w.adjacent("right"); got != br {
		t.Fatalf("right from L = %v, want BR (most-recently-active neighbor)", id(got))
	}

	// If TR is then made most-recent, right from L flips to TR.
	w.setActive(tr)
	w.setActive(l)
	if got := w.adjacent("right"); got != tr {
		t.Fatalf("right from L after visiting TR = %v, want TR", id(got))
	}
}

func id(p *pane) any {
	if p == nil {
		return "nil"
	}
	return p.id
}
