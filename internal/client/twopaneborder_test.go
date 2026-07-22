package client

import (
	"testing"

	"github.com/FyrmForge/gtmux/internal/emu"
	"github.com/FyrmForge/gtmux/internal/proto"
)

// With EXACTLY two panes, tmux's pane-border-indicators=colour lights only half
// the shared divider so you can tell which pane is active (a full-length divider
// is the same cells for both). For a vertical divider: left active → TOP half
// active, right active → BOTTOM half active (split at the pane midpoint). This
// is what "the 2-pane divider is green regardless of selection" was missing.
func TestTwoPaneBorderHalfColour(t *testing.T) {
	build := func(activeID int) *compositor {
		c := newCompositor()
		c.apply(&proto.ServerMsg{
			Layout: &proto.Layout{
				Cols: 7, Rows: 4,
				Panes: []proto.PaneRect{
					{ID: 1, Row: 0, Col: 0, Rows: 4, Cols: 3, Active: activeID == 1},
					{ID: 2, Row: 0, Col: 4, Rows: 4, Cols: 3, Active: activeID == 2},
				},
				Borders: []proto.BorderSeg{{Vertical: true, Fixed: 3, Start: 0, End: 4}},
			},
			Status: &proto.StatusInfo{},
		})
		return c
	}

	// midRow = 0 + 4/2 = 2. Left active → rows 0..2 active, row 3 inactive.
	cL := build(1)
	act := cL.cfg.ActiveBorderFG
	for _, r := range []int{0, 1, 2} {
		if fg := cL.buildRow(r)[3].FG; fg != act {
			t.Errorf("left active: divider row %d FG=%v, want active (top half)", r, fg)
		}
	}
	if fg := cL.buildRow(3)[3].FG; fg == act {
		t.Errorf("left active: divider row 3 should be INACTIVE (bottom half), got active")
	}

	// Right active → bottom half active: row 3 active, rows 0..2 inactive.
	cR := build(2)
	if fg := cR.buildRow(3)[3].FG; fg != act {
		t.Errorf("right active: divider row 3 FG=%v, want active (bottom half)", fg)
	}
	for _, r := range []int{0, 1, 2} {
		if fg := cR.buildRow(r)[3].FG; fg == act {
			t.Errorf("right active: divider row %d should be INACTIVE (top half), got active", r)
		}
	}
}

// framed mode: the outer frame must take the active pane's border color on that
// pane's own edges (it used to hardcode the inactive style, so a framed window
// never showed which pane was active), and a dock strip must cover the frame
// rows (they used to fall through to the dot-fill, showing a row of dots level
// with the frame).
func TestFramedBorderActiveAndDockRows(t *testing.T) {
	c := newCompositor()
	c.cfg.PaneBorders = "framed"
	c.cfg.InactiveBorderFG = emu.Red
	c.docks = append(c.docks, &textBox{dock: "left", size: 4, fg: emu.White, bg: emu.Blue})
	c.setPhysical(20, 6)
	c.apply(&proto.ServerMsg{
		Layout: &proto.Layout{
			Cols: 14, Rows: 4,
			Panes: []proto.PaneRect{{ID: 1, Row: 0, Col: 0, Rows: 4, Cols: 14, Active: true}},
		},
		Status: &proto.StatusInfo{},
	})

	// The sole pane touches the frame on every side, so the frame is its ring:
	// the frame's left column must be active-colored, not the inactive style.
	fg, _, _ := c.borderStyleAt(-1, 0)
	if fg != c.cfg.ActiveBorderFG {
		t.Errorf("frame left column FG=%v, want active %v", fg, c.cfg.ActiveBorderFG)
	}

	// The dock occupies physical cols 0-3 on the frame row; those cells must be
	// the dock's style-fill, not the dot-fill glyph.
	frameRow := c.buildFrameRow(true)
	fill := c.fillGlyph()
	for x := 0; x < 4; x++ {
		if frameRow[x].Char == fill.Char && frameRow[x].FG == fill.FG {
			t.Errorf("frame row col %d is dot-fill; the dock strip should cover it", x)
		}
	}
}

// A left/right dock spans the whole strip INCLUDING the two rows the framed
// outer border occupies — the frame wraps the window's panes, not the chrome
// beside them. Sizing the dock to layout.Rows alone left its box two rows short,
// so a dock that boxes itself (c:box(0,0,c.w,c.h)) had its bottom border sitting
// a row above the frame's: visibly misaligned.
func TestDockSpansFrameRows(t *testing.T) {
	c := newCompositor()
	c.cfg.PaneBorders = "framed"
	c.docks = append(c.docks, &textBox{dock: "left", size: 6, fg: emu.White, bg: emu.Black})
	c.setPhysical(20, 6)
	c.apply(&proto.ServerMsg{
		Layout: &proto.Layout{Cols: 12, Rows: 3,
			Panes: []proto.PaneRect{{ID: 1, Row: 0, Col: 0, Rows: 3, Cols: 12, Active: true}}},
		Status: &proto.StatusInfo{},
	})

	d := c.docks[0]
	if want := c.layout.Rows + 2*c.frameInset(); d.h != want {
		t.Fatalf("dock h = %d, want %d (content rows + both frame rows)", d.h, want)
	}
	// Mark the dock's first and last canvas rows; they must render on the same
	// physical rows as the frame's top and bottom lines.
	d.lines = make([]string, d.h)
	d.lines[0], d.lines[d.h-1] = "TOP", "BOT"
	top, bot := c.contentOffset()-c.frameInset(), c.totalRows()-c.bottomReserve()+c.frameInset()-1
	if got := c.buildRow(top)[0].Char; got != 'T' {
		t.Errorf("dock row 0 renders at physical %d as %q, want the frame's top row", top, got)
	}
	if got := c.buildRow(bot)[0].Char; got != 'B' {
		t.Errorf("dock last row renders at physical %d as %q, want the frame's bottom row", bot, got)
	}
}

// framed + exactly two panes: the two-pane half-colour indicator must apply ONLY
// to the shared interior divider, not the outer frame. The active pane's outer
// frame edge (touched by one pane) must light FULLY; splitting it half-active
// looked broken.
func TestFramedTwoPaneOuterFrameFullyActive(t *testing.T) {
	c := newCompositor()
	c.cfg.PaneBorders = "framed"
	c.setPhysical(30, 8)
	c.apply(&proto.ServerMsg{
		Layout: &proto.Layout{
			Cols: 27, Rows: 6, // two side-by-side panes, divider at content col 13
			Panes: []proto.PaneRect{
				{ID: 1, Row: 0, Col: 0, Rows: 6, Cols: 13, Active: true}, // left, active
				{ID: 2, Row: 0, Col: 14, Rows: 6, Cols: 13},
			},
		},
		Status: &proto.StatusInfo{},
	})
	act := c.cfg.ActiveBorderFG

	// The active left pane's OUTER frame column (content col -1) must be active on
	// EVERY row — no half-split. Row 0 and the last content row both.
	for _, r := range []int{0, 5} {
		fg, _, _ := c.borderStyleAt(-1, r)
		if fg != act {
			t.Errorf("outer frame left col, row %d: FG=%v, want fully active %v", r, fg, act)
		}
	}

	// The shared interior divider (content col 13 = left pane's right edge) IS
	// half-split: top half active, bottom half inactive (left pane active).
	topFg, _, _ := c.borderStyleAt(13, 0)
	botFg, _, _ := c.borderStyleAt(13, 5)
	if topFg != act {
		t.Errorf("shared divider top: FG=%v, want active", topFg)
	}
	if botFg == act {
		t.Errorf("shared divider bottom should be inactive (two-pane indicator), got active")
	}
}
