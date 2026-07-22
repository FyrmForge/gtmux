package server

import "testing"

// A named layout applied while the window is small (workspacer builds it while
// the session is detached, so window width == main-pane-width and the side
// column clamps to its 2-col minimum) must RE-APPLY when the window grows, not
// scale the frozen sliver fractions. Before the fix, the side column stayed ~2
// cols forever after the client attached wide — a nvim pane hogging the width
// with a 2-column shell strip glued to the edge.
func TestNamedLayoutReAppliesOnResize(t *testing.T) {
	p1 := &pane{id: 1}
	p2 := &pane{id: 2}
	p3 := &pane{id: 3}
	w := &window{cols: 80, rows: 24, panes: []*pane{p1, p2, p3}, active: p1}

	// main-vertical at 80 cols with main-pane-width 80 → side column clamps.
	w.setLayout("main-vertical", 80, 24)
	if p2.rect.Cols > 10 {
		t.Fatalf("precondition: side pane should be clamped small at 80c, got W=%d", p2.rect.Cols)
	}

	// Client attaches wide. The named layout must re-apply: main back to ~80
	// (main-pane-width), side column absorbing the rest.
	w.resize(221, 40)
	if p1.rect.Cols > 100 {
		t.Errorf("main pane W=%d after resize, want ~80 (main-pane-width) — layout scaled instead of re-applying", p1.rect.Cols)
	}
	if p2.rect.Cols < 100 || p3.rect.Cols < 100 {
		t.Errorf("side panes W=%d/%d after resize, want ~140 — stayed a sliver", p2.rect.Cols, p3.rect.Cols)
	}
}

// A manual pane resize clears the named layout, so a later window resize scales
// the hand-tuned split instead of snapping it back to the preset.
func TestManualResizeClearsNamedLayout(t *testing.T) {
	p1 := &pane{id: 1}
	p2 := &pane{id: 2}
	w := &window{cols: 200, rows: 24, panes: []*pane{p1, p2}, active: p1}
	w.setLayout("even-horizontal", 80, 24)
	if w.layoutName == "" {
		t.Fatal("precondition: setLayout should record the name")
	}
	w.resizePane("right", 20) // hand-tune
	if w.layoutName != "" {
		t.Errorf("manual resize left layoutName=%q, want cleared", w.layoutName)
	}
}
