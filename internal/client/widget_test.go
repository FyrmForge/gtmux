package client

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/FyrmForge/gtmux/internal/config"
	"github.com/FyrmForge/gtmux/internal/emu"
	"github.com/FyrmForge/gtmux/internal/proto"
)

// lastWidget returns the widget a test's own lua registered — the default config
// ships a status component at index 0, so a test's widget appends after it.
func lastWidget(cfg config.ClientConfig) config.WidgetSpec {
	return cfg.Widgets[len(cfg.Widgets)-1]
}

// A modal keyboard widget (gtmux.open) receives keys via on_key: navigation
// mutates its state (re-rendering shows it), and Enter records an action + closes.
func TestModalKeyboardWidget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	src := `
gtmux.bind("s", function()
  gtmux.open{
    width = 12, height = 3,
    component = function(props, ui)
      local st = ui:state(); st.sel = st.sel or 1
      ui:text(0, 0, "sel=" .. st.sel)
    end,
    on_key = function(key, ui)
      local st = ui:state(); st.sel = st.sel or 1
      if key == "Down" then st.sel = st.sel + 1
      elseif key == "Enter" then gtmux.switch_session("s" .. st.sel); ui:close()
      elseif key == "Escape" then ui:close() end
    end,
  }
end)
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, binds := config.LoadClient(path)
	defer binds.Close()

	ops := binds.Resolve("s")
	if len(ops) != 1 || ops[0].Modal == nil {
		t.Fatalf("bind s should emit a Modal open, got %+v", ops)
	}
	c := newCompositor()
	c.cfg = cfg
	c.openModal(ops[0].Modal, binds)

	row0 := func() string {
		rs := make([]rune, 5)
		for x := range rs {
			g, _ := c.modal.canvas.At(x, 0)
			rs[x] = g.Char
		}
		return string(rs)
	}
	if got := row0(); got != "sel=1" {
		t.Fatalf("initial modal = %q, want sel=1", got)
	}
	// Down navigates; re-render reflects the state change (as the input loop does).
	if _, closed := c.modalKey("Down"); closed {
		t.Fatal("Down should not close")
	}
	c.modal.rerender()
	if got := row0(); got != "sel=2" {
		t.Fatalf("after Down = %q, want sel=2 (state persisted + re-rendered)", got)
	}
	// Enter records the action and closes.
	mops, closed := c.modalKey("Enter")
	if !closed {
		t.Error("Enter should close the modal")
	}
	if len(mops) != 1 || len(mops[0].Action) < 3 || mops[0].Action[2] != "s2" {
		t.Fatalf("Enter ran %+v, want switch-client -t s2", mops)
	}
}

// A text-input prompt modal edits a string via on_key (printables append,
// BSpace deletes) and dispatches it on Enter — no new mechanism, pure Lua over
// the modal. Feeds keys through RunKey (the reliable path) rather than a live TTY.
func TestModalTextInputPrompt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	src := `
gtmux.bind("r", function()
  gtmux.open{
    position = "status",
    component = function(props, ui)
      local st = ui:state(); if st.text == nil then st.text = "ab" end
      ui:text(0, 0, "(rename-window) " .. st.text)
    end,
    on_key = function(key, ui)
      local st = ui:state(); if st.text == nil then st.text = "ab" end
      if key == "Enter" then gtmux.rename_window(st.text); ui:close()
      elseif key == "BSpace" then st.text = st.text:sub(1, -2)
      elseif #key == 1 then st.text = st.text .. key end
    end,
  }
end)
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, binds := config.LoadClient(path)
	defer binds.Close()
	ops := binds.Resolve("r")
	if len(ops) != 1 || ops[0].Modal == nil || ops[0].Modal.Position != "status" {
		t.Fatalf("bind r should open a status-position modal, got %+v", ops)
	}
	c := newCompositor()
	c.cfg = cfg
	c.setPhysical(40, 4)
	c.openModal(ops[0].Modal, binds)
	// Start "ab"; BSpace -> "a"; type "x" -> "ax"; Enter -> rename-window ax.
	for _, k := range []string{"BSpace", "x"} {
		if _, closed := c.modalKey(k); closed {
			t.Fatalf("key %q closed early", k)
		}
		c.modal.rerender()
	}
	mops, closed := c.modalKey("Enter")
	if !closed {
		t.Error("Enter should close the prompt")
	}
	if len(mops) != 1 || len(mops[0].Action) != 2 ||
		mops[0].Action[0] != "rename-window" || mops[0].Action[1] != "ax" {
		t.Fatalf("submit ran %+v, want [rename-window ax]", mops)
	}
}

// A widget mounted with dock="status" owns the status rows: buildRow paints its
// canvas there (in place of renderBar), and a click on a status row routes to its
// regions (window-list click-to-select), not the bespoke windowHits path.
func TestStatusComponentOwnsStatusRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	src := `
gtmux.widget{ dock = "status", component = function(props, ui)
  ui:text(0, 0, "HELLO")
  ui:child(6, 0, 3, 1, function(p, c)
    c:text(0, 0, "BTN")
    c:on_click(function() gtmux.switch_session("target") end)
  end)
end }
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, binds := config.LoadClient(path)
	defer binds.Close()

	c := newCompositor()
	c.cfg = cfg
	c.setPhysical(20, 4) // 3 content rows + 1 status row (bottom)
	c.statusWidget = &textBox{
		dock: "status", component: lastWidget(cfg).Component,
		binds: binds, fg: cfg.StatusFG, bg: cfg.StatusBG,
	}
	c.apply(&proto.ServerMsg{
		Layout: &proto.Layout{
			Cols: 20, Rows: 3,
			Panes: []proto.PaneRect{{ID: 1, Row: 0, Col: 0, Rows: 3, Cols: 20, Active: true}},
		},
		Status: &proto.StatusInfo{},
	})
	// The status row shows the component's text, not a bespoke bar.
	row := c.buildRow(c.statusRow())
	got := make([]rune, 5)
	for i := range got {
		got[i] = row[i].Char
	}
	if string(got) != "HELLO" {
		t.Fatalf("status row = %q, want it to start HELLO (component owns it)", string(got))
	}
	// A click on the BTN region (physical col 6, the status row) routes to its
	// on_click, not windowHits.
	_, fn, li, lt, cc := c.clickWidget(proto.MouseEvent{X: 7, Y: c.statusRow() + 1, Press: true})
	if fn == nil {
		t.Fatal("click on status component region resolved no handler")
	}
	ops := binds.RunClick(fn, li, lt, cc)
	if len(ops) != 1 || len(ops[0].Action) < 3 || ops[0].Action[2] != "target" {
		t.Fatalf("status click ran %+v, want switch-client -t target", ops)
	}
}

// A component's click regions are widget-LOCAL, and clickWidget maps a physical
// click through the dock's own offset before matching. This mounts the component
// in a RIGHT dock (physical col offset 20, non-zero on purpose): a click on the
// child fires it, a click one cell left — inside the dock but outside the child —
// resolves nothing. A widget-at-offset-0 test would pass even with a double-
// offset bug; this discriminates it.
func TestComponentClickMapsThroughDockOffset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	src := `
gtmux.widget{ dock = "right", size = 10, component = function(props, ui)
  ui:child(2, 1, 3, 1, function(p, c)
    c:on_click(function() gtmux.switch_session("target") end)
  end)
end }
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, binds := config.LoadClient(path)
	defer binds.Close()

	c := newCompositor()
	c.cfg = cfg
	c.setPhysical(30, 4) // 30 cols; rows: 3 content + 1 status
	b := &textBox{
		dock: "right", size: 10, component: lastWidget(cfg).Component,
		binds: binds, fg: emu.White, bg: emu.Black,
	}
	c.addDock(b) // before apply so rightInset counts it
	c.apply(&proto.ServerMsg{
		Layout: &proto.Layout{
			Cols: 20, Rows: 3,
			Panes: []proto.PaneRect{{ID: 1, Row: 0, Col: 0, Rows: 3, Cols: 20, Active: true}},
		},
		Status: &proto.StatusInfo{},
	})
	if len(b.regions) != 1 {
		t.Fatalf("regions after apply = %d, want 1 (component didn't run?)", len(b.regions))
	}
	// Right dock physical cols start at 30-10=20. Child is widget-local (2,1), so
	// its cell is physical col 22, content row 1 -> me.X=23, me.Y=2 (1-based).
	_, fn, li, lt, cc := c.clickWidget(proto.MouseEvent{X: 23, Y: 2, Press: true})
	if fn == nil {
		t.Fatal("click inside child resolved no handler (dock/region offset wrong)")
	}
	ops := binds.RunClick(fn, li, lt, cc)
	if len(ops) != 1 || len(ops[0].Action) < 3 || ops[0].Action[2] != "target" {
		t.Fatalf("click ran %+v, want switch-client -t target", ops)
	}
	// One cell left (physical col 21 -> me.X=22): inside the dock, outside the
	// child. Must resolve no handler — this is what a double-offset bug breaks.
	if _, fn2, _, _, _ := c.clickWidget(proto.MouseEvent{X: 22, Y: 2, Press: true}); fn2 != nil {
		t.Error("click outside child (still in dock) should resolve no handler")
	}
}

// A registered widget composites on top of pane content through the same
// buildRow layer pass the built-in overlays use — this is the whole point of
// the widget abstraction: a custom UI is "register a widget", no second render
// path. Pane 1 fills "abcde"; a textBox at col 1 overwrites the middle.
func TestWidgetOverlaysPaneContent(t *testing.T) {
	c := newCompositor()
	c.apply(&proto.ServerMsg{
		Layout: &proto.Layout{
			Cols: 5, Rows: 1,
			Panes: []proto.PaneRect{{ID: 1, Row: 0, Col: 0, Rows: 1, Cols: 5, Active: true}},
		},
		PaneContent: []proto.PaneContent{{PaneID: 1, Lines: map[int]emu.Line{0: lineOf("abcde")}}},
		Status:      &proto.StatusInfo{},
	})
	c.overlays = append(c.overlays, &textBox{row: 0, col: 1, lines: []string{"XY"}, fg: emu.Red, bg: emu.Blue})

	row := c.buildRow(0)
	got := make([]rune, len(row))
	for i, g := range row {
		got[i] = g.Char
	}
	if string(got) != "aXYde" {
		t.Fatalf("buildRow(0) = %q, want %q", string(got), "aXYde")
	}
	if row[1].FG != emu.Red || row[1].BG != emu.Blue {
		t.Errorf("widget cell style = fg %v/bg %v, want red/blue", row[1].FG, row[1].BG)
	}
}

// A format-driven widget re-expands its text against Status vars on the Status
// tick — this is what makes a widget a live data source (vars/#client/#server)
// rather than static text, riding the existing status pipeline.
func TestWidgetFormatReexpandsOnStatus(t *testing.T) {
	c := newCompositor()
	c.overlays = append(c.overlays, &textBox{row: 0, col: 0, format: "s=#{session}", fg: emu.White})
	c.apply(&proto.ServerMsg{
		Layout: &proto.Layout{
			Cols: 20, Rows: 1,
			Panes: []proto.PaneRect{{ID: 1, Row: 0, Col: 0, Rows: 1, Cols: 20, Active: true}},
		},
		PaneContent: []proto.PaneContent{{PaneID: 1, Lines: map[int]emu.Line{0: lineOf("....................")}}},
		Status:      &proto.StatusInfo{Vars: map[string]string{"session": "work"}},
	})

	row := c.buildRow(0)
	got := make([]rune, 6)
	for i := range got {
		got[i] = row[i].Char
	}
	if string(got) != "s=work" {
		t.Fatalf("widget row = %q, want %q (format expanded against Status vars)", string(got), "s=work")
	}
}

// A left dock reserves N columns: pane content shifts right by N, and the dock's
// own text paints in the reserved strip. This exercises the contentColOffset
// mirror end to end (canvas width, compose, strip paint).
func TestLeftDockShiftsContent(t *testing.T) {
	c := newCompositor()
	c.setPhysical(10, 2) // 10 physical cols; row 0 = content, row 1 = status bar
	c.addDock(&textBox{dock: "left", size: 3, lines: []string{"AB"}, fg: emu.White})
	// The window is laid out at the content width (10 - 3 = 7).
	c.apply(&proto.ServerMsg{
		Layout: &proto.Layout{
			Cols: 7, Rows: 1,
			Panes: []proto.PaneRect{{ID: 1, Row: 0, Col: 0, Rows: 1, Cols: 7, Active: true}},
		},
		PaneContent: []proto.PaneContent{{PaneID: 1, Lines: map[int]emu.Line{0: lineOf("xxxxxxx")}}},
		Status:      &proto.StatusInfo{},
	})

	row := c.buildRow(0)
	if len(row) != 10 {
		t.Fatalf("row width = %d, want 10 (physical)", len(row))
	}
	got := make([]rune, 10)
	for i, g := range row {
		got[i] = g.Char
	}
	// "AB " = dock strip (3 cols, text left-aligned + bg pad), then 7 pane cells.
	if string(got) != "AB xxxxxxx" {
		t.Fatalf("row = %q, want %q (dock strip + shifted content)", string(got), "AB xxxxxxx")
	}
}

// A top dock reserves a full-width row at the top; the window content shifts
// down past it. Exercises the row mirror (contentOffset absorbing top docks,
// buildRow intercepting the strip). Status is the default single bottom line.
func TestTopDockShiftsContentDown(t *testing.T) {
	c := newCompositor()
	c.setPhysical(8, 4) // row0 = top dock, rows1-2 = content, row3 = bottom status
	c.addDock(&textBox{dock: "top", size: 1, lines: []string{"TOPBAR"}, fg: emu.White})
	c.apply(&proto.ServerMsg{
		Layout: &proto.Layout{
			Cols: 8, Rows: 2, // physical 4 - 1 top dock - 1 status
			Panes: []proto.PaneRect{{ID: 1, Row: 0, Col: 0, Rows: 2, Cols: 8, Active: true}},
		},
		PaneContent: []proto.PaneContent{{PaneID: 1, Lines: map[int]emu.Line{0: lineOf("abcdefgh"), 1: lineOf("ijklmnop")}}},
		Status:      &proto.StatusInfo{},
	})

	rowText := func(l emu.Line) string {
		r := make([]rune, len(l))
		for i, g := range l {
			r[i] = g.Char
		}
		return string(r)
	}
	if got := rowText(c.buildRow(0)); got[:6] != "TOPBAR" {
		t.Fatalf("row 0 = %q, want the top dock strip (TOPBAR...)", got)
	}
	if got := rowText(c.buildRow(1)); got != "abcdefgh" {
		t.Fatalf("row 1 = %q, want %q (content shifted down past the top dock)", got, "abcdefgh")
	}
}

// pane_borders="joined" replaces straight dividers with box-drawing junctions:
// a vertical divider crossing a horizontal one becomes ┼, and the tees/corners
// resolve from which neighbors carry a stroke. Layout: a 5-wide × 3-tall window
// split into 4 quadrants — vertical divider at col 2 (rows 0..3), horizontal at
// row 1 (cols 0..5). They cross at (1,2).
func TestJoinedBorderJunctions(t *testing.T) {
	c := newCompositor()
	c.setPhysical(5, 4) // rows 0..2 content, row 3 status
	c.cfg.PaneBorders = "joined"
	c.apply(&proto.ServerMsg{
		Layout: &proto.Layout{
			Cols: 5, Rows: 3,
			Panes: []proto.PaneRect{ // 4 quadrants, leaving col 2 and row 1 for dividers
				{ID: 1, Row: 0, Col: 0, Rows: 1, Cols: 2, Active: true},
				{ID: 2, Row: 0, Col: 3, Rows: 1, Cols: 2},
				{ID: 3, Row: 2, Col: 0, Rows: 1, Cols: 2},
				{ID: 4, Row: 2, Col: 3, Rows: 1, Cols: 2},
			},
			Borders: []proto.BorderSeg{
				{Vertical: true, Fixed: 2, Start: 0, End: 3},
				{Vertical: false, Fixed: 1, Start: 0, End: 5},
			},
		},
		Status: &proto.StatusInfo{},
	})
	rowStr := func(r int) string {
		l := c.buildRow(r)
		out := make([]rune, len(l))
		for i, g := range l {
			out[i] = g.Char
		}
		return string(out)
	}
	// row 0: vertical divider at col 2 → │
	if got := []rune(rowStr(0)); got[2] != '│' {
		t.Errorf("row0 col2 = %q, want │ (%q)", got[2], string(got))
	}
	// row 1: horizontal divider, crossing the vertical at col 2 → ┼
	got := rowStr(1)
	if got != "──┼──" {
		t.Errorf("row1 = %q, want ──┼── (crossing junction)", got)
	}
}

// pane_borders="framed" wraps the whole window in an outer frame: content shrinks
// by 1 cell on every side, and the compositor draws ┌─┐ / │ / └─┘ around it.
// 6×4 physical, 1 status row → content is 4×1 (one pane), framed by row0/row2 and
// col0/col5.
func TestFramedOuterBorder(t *testing.T) {
	c := newCompositor()
	c.setPhysical(6, 4)
	c.cfg.PaneBorders = "framed"
	c.apply(&proto.ServerMsg{
		Layout: &proto.Layout{
			Cols: 4, Rows: 1,
			Panes: []proto.PaneRect{{ID: 1, Row: 0, Col: 0, Rows: 1, Cols: 4, Active: true}},
		},
		PaneContent: []proto.PaneContent{{PaneID: 1, Lines: map[int]emu.Line{0: lineOf("abcd")}}},
		Status:      &proto.StatusInfo{},
	})
	rowStr := func(r int) string {
		l := c.buildRow(r)
		out := make([]rune, len(l))
		for i, g := range l {
			out[i] = g.Char
		}
		return string(out)
	}
	if got := rowStr(0); got != "┌────┐" {
		t.Errorf("top frame = %q, want ┌────┐", got)
	}
	if got := rowStr(1); got != "│abcd│" {
		t.Errorf("content row = %q, want │abcd│ (content bracketed by frame)", got)
	}
	if got := rowStr(2); got != "└────┘" {
		t.Errorf("bottom frame = %q, want └────┘", got)
	}
}
