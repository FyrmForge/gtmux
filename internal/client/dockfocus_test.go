package client

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/FyrmForge/gtmux/internal/config"
	"github.com/FyrmForge/gtmux/internal/proto"
)

// A focusable dock: nav at the window edge steps into it, keys hit its on_key
// (state.focused visible), ui:close() steps back out; focus_dock toggles too.
func TestDockFocus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	src := `
gtmux.widget{
  dock = "left", size = 10, name = "list", focus = "both",
  component = function(props, ui)
    local st = ui:state()
    ui:text(0, 0, st.focused and "F" or "-")
  end,
  on_key = function(key, ui)
    local st = ui:state(); st.keys = (st.keys or "") .. key
    if key == "Escape" then ui:close() end
  end,
}
gtmux.bind("e", function() gtmux.focus_dock("list") end)
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, binds := config.LoadClient(path)
	defer binds.Close()

	c := newCompositor()
	c.cfg = cfg
	c.setPhysical(80, 24)
	c.rebuildWidgets(binds)
	// Two side-by-side panes: pane 1 at the left edge (active), pane 2 not.
	c.layout = &proto.Layout{Cols: 70, Rows: 22, Panes: []proto.PaneRect{
		{ID: 1, Row: 0, Col: 0, Rows: 22, Cols: 34, Active: true},
		{ID: 2, Row: 0, Col: 35, Rows: 22, Cols: 35},
	}}

	for _, d := range c.docks { // apply() sizes docks on the status tick
		d.w, d.h = d.size, c.layout.Rows
	}

	if c.focusDockNav("-R") {
		t.Fatal("nav away from the dock's side must not focus it")
	}
	if !c.focusDockNav("-L") {
		t.Fatal("nav left at the left edge should focus the left dock")
	}
	if c.focusedDock == nil || c.focusedDock.name != "list" {
		t.Fatalf("focusedDock = %+v, want the list dock", c.focusedDock)
	}
	if g, _ := c.focusedDock.canvas.At(0, 0); g.Char != 'F' {
		t.Fatalf("focused render shows %q, want F (state.focused)", g.Char)
	}

	if _, closed := c.dockKey("j"); closed {
		t.Fatal("plain key must not unfocus")
	}
	_, closed := c.dockKey("Escape")
	if !closed {
		t.Fatal("Escape should ask to unfocus")
	}
	c.setDockFocus(c.focusedDock, false)
	if c.focusedDock != nil {
		t.Fatal("dock still focused after close")
	}

	c.toggleDockFocus("list")
	if c.focusedDock == nil {
		t.Fatal("focus_dock should focus the named dock")
	}
	c.toggleDockFocus("list")
	if c.focusedDock != nil {
		t.Fatal("focus_dock again should unfocus")
	}
}

// addDock registers a dock the way rebuildWidgets does (allDocks + visibility
// refresh), so tests exercise the same path as config-built docks.
func (c *compositor) addDock(d *textBox) {
	c.allDocks = append(c.allDocks, d)
	c.refreshDocks()
}
