package client

import (
	"testing"

	"github.com/FyrmForge/gtmux/internal/proto"
)

// gtmux.responsive{cols_below}: crossing below the breakpoint zooms, crossing
// back above unzooms only if the zoom was ours, and no action repeats while
// staying on one side (so a manual unzoom on a small screen sticks).
func TestResponsiveAction(t *testing.T) {
	c := newCompositor()
	c.cfg.RespBelow, c.cfg.RespMode = 80, "maximize"
	c.layout = &proto.Layout{Panes: []proto.PaneRect{{ID: 1}, {ID: 2}}}
	c.status = &proto.StatusInfo{Windows: []proto.WindowInfo{{Active: true}}}
	zoomed := func(z bool) { c.status.Windows[0].Zoomed = z }

	c.setPhysical(100, 20)
	if act := c.responsiveAction(); act != nil {
		t.Fatalf("wide first eval: want nil, got %v", act)
	}
	c.setPhysical(60, 20)
	if act := c.responsiveAction(); len(act) != 2 || act[1] != "-Z" {
		t.Fatalf("crossing below: want resize-pane -Z, got %v", act)
	}
	zoomed(true) // server did the zoom
	if act := c.responsiveAction(); act != nil {
		t.Fatalf("still below: want nil (no re-fire), got %v", act)
	}
	// Manual unzoom while below must stick — no crossing, no action.
	zoomed(false)
	if act := c.responsiveAction(); act != nil {
		t.Fatalf("manual unzoom below: want nil, got %v", act)
	}
	zoomed(true)
	c.setPhysical(100, 20)
	if act := c.responsiveAction(); len(act) != 2 || act[1] != "-Z" {
		t.Fatalf("crossing above with our zoom: want resize-pane -Z, got %v", act)
	}
	zoomed(false)

	// A zoom NOT ours survives the crossing back above.
	c2 := newCompositor()
	c2.cfg.RespBelow, c2.cfg.RespMode = 80, "maximize"
	c2.layout = c.layout
	c2.status = &proto.StatusInfo{Windows: []proto.WindowInfo{{Active: true, Zoomed: true}}}
	c2.setPhysical(60, 20)
	if act := c2.responsiveAction(); act != nil {
		t.Fatalf("already zoomed below: want nil, got %v", act)
	}
	c2.setPhysical(100, 20)
	if act := c2.responsiveAction(); act != nil {
		t.Fatalf("manual zoom crossing above: want nil (not ours), got %v", act)
	}

	// First eval already below (phone attach): zoom straight away.
	c3 := newCompositor()
	c3.cfg.RespBelow, c3.cfg.RespMode = 80, "maximize"
	c3.layout = c.layout
	c3.status = &proto.StatusInfo{Windows: []proto.WindowInfo{{Active: true}}}
	c3.setPhysical(60, 20)
	if act := c3.responsiveAction(); len(act) != 2 || act[1] != "-Z" {
		t.Fatalf("attach below: want resize-pane -Z, got %v", act)
	}
}
