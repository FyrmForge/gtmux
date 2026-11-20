package client

import (
	"testing"

	"github.com/FyrmForge/gtmux/internal/emu"
	"github.com/FyrmForge/gtmux/internal/proto"
)

// A centered modal sits in the middle of the content area. modalRow paints
// into the content line (docks wrap it afterwards), so centering on the full
// terminal width shifted it right by the left dock's width.
func TestModalCenteredWithLeftDock(t *testing.T) {
	c := newCompositor()
	c.addDock(&textBox{dock: "left", size: 20, name: "side", lines: []string{"X"}, fg: emu.White})
	c.setPhysical(120, 30)
	c.layout = &proto.Layout{Cols: 100, Rows: 30}
	c.modal = &textBox{w: 40, h: 10}
	ox, _ := c.modalOffset()
	if want := (c.contentCols() - 40) / 2; ox != want {
		t.Fatalf("modal x = %d in a %d-col content area, want %d (centered)", ox, c.contentCols(), want)
	}
}
