package client

import (
	"testing"

	"github.com/FyrmForge/gtmux/internal/emu"
)

// A dock with min_cols hides below the breakpoint (giving its columns back to
// the content width), reappears above it, and toggle_dock forces either state
// over the breakpoint until toggled again.
func TestDockMinColsAndToggle(t *testing.T) {
	c := newCompositor()
	c.addDock(&textBox{dock: "left", size: 10, minCols: 80, name: "side", lines: []string{"X"}, fg: emu.White})

	c.setPhysical(100, 10)
	if len(c.docks) != 1 || c.leftInset() != 10 {
		t.Fatalf("wide: dock should be visible with inset 10, got %d docks inset %d", len(c.docks), c.leftInset())
	}
	c.setPhysical(60, 10)
	if len(c.docks) != 0 || c.leftInset() != 0 {
		t.Fatalf("narrow: dock should auto-hide, got %d docks inset %d", len(c.docks), c.leftInset())
	}
	if cols, _ := c.reportSize(); cols != 60 {
		t.Fatalf("narrow reportSize cols = %d, want 60 (hidden dock gives space back)", cols)
	}

	// Toggle while auto-hidden → forced shown, even below the breakpoint.
	if !c.toggleDock("side") {
		t.Fatal("toggleDock(side) found no dock")
	}
	if len(c.docks) != 1 {
		t.Fatal("toggle should force the hidden dock visible")
	}
	// Survives a resize that stays below the breakpoint.
	c.setPhysical(50, 10)
	if len(c.docks) != 1 {
		t.Fatal("forced-shown dock should ignore the breakpoint")
	}
	// Toggle again → forced hidden; growing wide doesn't bring it back.
	c.toggleDock("side")
	c.setPhysical(100, 10)
	if len(c.docks) != 0 {
		t.Fatal("forced-hidden dock should stay hidden at any width")
	}
	if c.toggleDock("nope") {
		t.Fatal("unknown dock name should report false")
	}
}
