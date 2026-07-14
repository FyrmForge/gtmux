package server

import "testing"

func TestSplitSizes(t *testing.T) {
	cases := []struct {
		frac  float64
		total int
		a, b  int
	}{
		{0.5, 11, 5, 5}, // even split, 1 cell to the divider
		{0.8, 11, 8, 2}, // clamped: b keeps its 2-cell minimum
		{0.0, 11, 2, 8}, // clamped: a keeps its 2-cell minimum
		{0.5, 4, 1, 2},  // too cramped for clamps: just halve
	}
	for _, c := range cases {
		n := &layoutNode{frac: c.frac}
		a, b := n.splitSizes(c.total)
		if a != c.a || b != c.b {
			t.Errorf("splitSizes(frac=%v, total=%d) = %d,%d, want %d,%d", c.frac, c.total, a, b, c.a, c.b)
		}
	}
}

func TestZoomedLayout(t *testing.T) {
	active := &pane{id: 1, rect: rect{Row: 0, Col: 0, Rows: 5, Cols: 10}}
	other := &pane{id: 2, rect: rect{Row: 0, Col: 5, Rows: 5, Cols: 4}}
	w := &window{
		cols: 10, rows: 5,
		panes:   []*pane{active, other},
		active:  active,
		zoomed:  true,
		borders: []borderSeg{{vertical: true, fixed: 4, start: 0, end: 5}},
	}

	l := w.layout()
	if len(l.Panes) != 1 || l.Panes[0].ID != 1 || !l.Panes[0].Active {
		t.Errorf("zoomed layout should report only the active pane, got %+v", l.Panes)
	}
	if len(l.Borders) != 0 {
		t.Errorf("zoomed layout should have no borders, got %+v", l.Borders)
	}
}

func TestWindowLayout(t *testing.T) {
	active := &pane{id: 1, rect: rect{Row: 0, Col: 0, Rows: 5, Cols: 4}}
	other := &pane{id: 2, rect: rect{Row: 0, Col: 5, Rows: 5, Cols: 4}}
	w := &window{
		cols: 10, rows: 5,
		panes:   []*pane{active, other},
		active:  active,
		borders: []borderSeg{{vertical: true, fixed: 4, start: 0, end: 5}},
	}

	l := w.layout()
	if l.Cols != 10 || l.Rows != 5 {
		t.Fatalf("Layout size = %dx%d, want 10x5", l.Cols, l.Rows)
	}
	if len(l.Panes) != 2 {
		t.Fatalf("expected 2 panes, got %d", len(l.Panes))
	}
	if !l.Panes[0].Active || l.Panes[1].Active {
		t.Errorf("expected pane 0 active, pane 1 inactive; got %v / %v", l.Panes[0].Active, l.Panes[1].Active)
	}
	if len(l.Borders) != 1 {
		t.Errorf("expected one border, got %+v", l.Borders)
	}
}
