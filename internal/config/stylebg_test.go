package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/FyrmForge/gtmux/internal/emu"
)

// A draw verb whose style has no bg= keeps the cell's background (a ui:fill
// underneath), not the canvas base: "fg=yellow" text in a black-filled modal
// used to paint the status bar's grey behind each glyph.
func TestStyleWithoutBGKeepsFill(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.lua")
	src := `gtmux.widget{ dock="left", size=10, fg="white", bg="dark_grey",
  draw = function(c)
    c:fill("bg=black")
    local i = c:box(0,0,c.w,c.h,"fg=cyan")
    i:text(0,0,"ab","fg=yellow")
    i:text(0,1,"cd","fg=yellow,bg=red")
  end }`
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, b := LoadClient(p)
	defer b.Close()
	w := cfg.Widgets[len(cfg.Widgets)-1]
	cv, _, _ := b.RunDraw(w.Draw, 10, 4, 0, emu.White, 0)
	for _, c := range []struct {
		x, y int
		want emu.Color
		what string
	}{
		{0, 0, emu.Black, "border"},
		{1, 1, emu.Black, "text without bg"},
		{1, 2, emu.Red, "text with bg=red"},
	} {
		if g, _ := cv.At(c.x, c.y); g.BG != c.want {
			t.Errorf("%s: bg = %v, want %v", c.what, g.BG, c.want)
		}
	}
}
