package client

import (
	"testing"

	"github.com/FyrmForge/gtmux/internal/proto"
)

// A left-click on a non-active, non-mouse-tracking pane must return a
// select-pane action targeting that pane (click-to-focus). No dock, no frame,
// so screen coords map straight to window coords.
func TestClickToFocusPane(t *testing.T) {
	c := newCompositor()
	c.setPhysical(20, 4) // 3 content rows + 1 status; no docks, no frame inset
	c.apply(&proto.ServerMsg{
		Layout: &proto.Layout{
			Cols: 20, Rows: 3,
			Panes: []proto.PaneRect{
				{ID: 1, Row: 0, Col: 0, Rows: 3, Cols: 10, Active: true},
				{ID: 2, Row: 0, Col: 10, Rows: 3, Cols: 10},
			},
		},
		Status: &proto.StatusInfo{},
	})

	// Left-press (Cb=0) at screen col 16 (1-based) → winCol 15 → pane 2.
	mr := c.mouseAction(proto.MouseEvent{Cb: 0, X: 16, Y: 1, Press: true})
	if len(mr.actions) != 1 {
		t.Fatalf("click on pane 2 returned %d actions, want 1 (select-pane)", len(mr.actions))
	}
	got := mr.actions[0]
	if len(got) != 3 || got[0] != "select-pane" || got[2] != "%2" {
		t.Fatalf("click returned %v, want [select-pane -t %%2]", got)
	}
}

// A left-click on an unfocused MOUSE-TRACKING pane (nvim, Claude Code, less)
// must both focus it (select-pane) and forward the event to the app. Before
// this, WantsMouse panes only forwarded — so clicking into them never switched
// panes, which is what "can't click to focus panes" was.
func TestClickToFocusTrackingPane(t *testing.T) {
	c := newCompositor()
	c.setPhysical(20, 4)
	c.apply(&proto.ServerMsg{
		Layout: &proto.Layout{
			Cols: 20, Rows: 3,
			Panes: []proto.PaneRect{
				{ID: 1, Row: 0, Col: 0, Rows: 3, Cols: 10, Active: true, WantsMouse: true},
				{ID: 2, Row: 0, Col: 10, Rows: 3, Cols: 10, WantsMouse: true},
			},
		},
		Status: &proto.StatusInfo{},
	})

	mr := c.mouseAction(proto.MouseEvent{Cb: 0, X: 16, Y: 1, Press: true})
	if !mr.forward {
		t.Error("click on tracking pane must still forward the event to the app")
	}
	if len(mr.actions) != 1 || mr.actions[0][0] != "select-pane" || mr.actions[0][2] != "%2" {
		t.Fatalf("click returned actions %v, want [select-pane -t %%2] + forward", mr.actions)
	}

	// Clicking the ALREADY-active tracking pane must not re-focus — just forward.
	mr2 := c.mouseAction(proto.MouseEvent{Cb: 0, X: 5, Y: 1, Press: true})
	if !mr2.forward || len(mr2.actions) != 0 {
		t.Errorf("click on active tracking pane = %+v, want forward only, no select-pane", mr2)
	}
}
