package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FyrmForge/gtmux/internal/config"
	"github.com/FyrmForge/gtmux/internal/emu"
	"github.com/FyrmForge/gtmux/internal/proto"
)

// previewRow reads the window-content text of one physical row, skipping the
// left dock strip, so a preview's paint can be compared as a string.
func previewRow(c *compositor, row int) string {
	line := c.buildRow(row + c.contentOffset())
	out := ""
	for i := c.contentColOffset(); i < c.contentColOffset()+c.layout.Cols && i < len(line); i++ {
		out += string(line[i].Char)
	}
	return out
}

func snapOf(rows ...string) []emu.Line {
	out := make([]emu.Line, len(rows))
	for i, r := range rows {
		l := make(emu.Line, 0, len(r))
		for _, ch := range r {
			g := emu.EmptyGlyph()
			g.Char = ch
			l = append(l, g)
		}
		out[i] = l
	}
	return out
}

// A dock preview paints the target's screen in place of the window content,
// top row first; a reply for a target the cursor has left is dropped; and
// losing dock focus clears it (a stuck preview reads as a hung session).
func TestDockPreview(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	src := `
gtmux.widget{ dock = "left", size = 0, name = "list", focus = "both",
  component = function(props, ui) end,
  on_key = function(key, ui) if key == "Escape" then ui:close() end end }
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, binds := config.LoadClient(path)
	defer binds.Close()

	c := newCompositor()
	c.cfg = cfg
	c.setPhysical(20, 6)
	c.rebuildWidgets(binds)
	c.layout = &proto.Layout{Cols: 20, Rows: 5, Panes: []proto.PaneRect{
		{ID: 1, Row: 0, Col: 0, Rows: 5, Cols: 20, Active: true},
	}}
	if len(c.docks) != 1 {
		t.Fatalf("want 1 dock, got %d", len(c.docks))
	}
	d := c.docks[0]

	// A reply nobody asked for never paints.
	if c.setPreview("other", snapOf("nope")) {
		t.Fatal("unrequested preview applied")
	}

	c.wantPreview("other")
	if !c.setPreview("other", snapOf("top line", "second")) {
		t.Fatal("requested preview rejected")
	}
	if got := previewRow(c, 0); got[:8] != "top line" {
		t.Fatalf("row 0 = %q, want the target's FIRST row (untrimmed capture)", got)
	}
	if got := previewRow(c, 1); got[:6] != "second" {
		t.Fatalf("row 1 = %q, want the target's second row", got)
	}
	if _, _, vis := c.activeCursor(); vis {
		t.Fatal("cursor must be hidden over another session's screen")
	}

	// Cursor moved on: the in-flight capture for the old row is stale.
	c.wantPreview("third")
	if c.setPreview("other", snapOf("stale")) {
		t.Fatal("stale preview applied after the target changed")
	}

	c.setDockFocus(d, true)
	c.setPreview("third", snapOf("shown"))
	c.setDockFocus(d, false)
	if c.preview != nil || c.previewTarget != "" {
		t.Fatal("preview survived the dock losing focus")
	}
}

// A preview replaces the window CONTENT, not the layers above it: lock the
// screen mid-browse and you still get the lock banner, not another session's
// pane with no way to tell you're locked.
func TestPreviewKeepsOverlays(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	src := `gtmux.widget{ dock = "left", size = 0, name = "list", focus = "both",
  component = function(props, ui) end }`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, binds := config.LoadClient(path)
	defer binds.Close()

	c := newCompositor()
	c.cfg = cfg
	c.setPhysical(40, 6)
	c.rebuildWidgets(binds)
	c.layout = &proto.Layout{Cols: 40, Rows: 5, Panes: []proto.PaneRect{
		{ID: 1, Row: 0, Col: 0, Rows: 5, Cols: 40, Active: true},
	}}

	c.wantPreview("other")
	c.setPreview("other", snapOf("aaaa", "bbbb", "cccc", "dddd", "eeee"))
	c.locked = true
	found := false
	for r := 0; r < c.layout.Rows; r++ {
		if strings.Contains(previewRow(c, r), "locked") {
			found = true
		}
	}
	if !found {
		t.Fatal("lock banner never painted over a preview")
	}
}
