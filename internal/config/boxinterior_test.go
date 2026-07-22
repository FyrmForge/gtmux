package config

import (
	"os"
	"path/filepath"
	"testing"
)

// ui:box returns its INTERIOR as a clipped child ui. Drawing through that child
// cannot spill onto the border — a long session name in a 15-wide dock used to
// overwrite the box's right │, because text drawn at the parent's coords wins
// over the border cells painted earlier. A config that ignores the return value
// keeps the old (unclipped) behaviour, so this is backward-compatible.
func TestBoxReturnsClippedInterior(t *testing.T) {
	draw := func(body string) [][]rune {
		d := t.TempDir()
		p := filepath.Join(d, "c.lua")
		src := `gtmux.widget{ dock="left", size=15, fg="white", bg="black",
  draw = function(c) ` + body + ` end }`
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, b := LoadClient(p)
		defer b.Close()
		w := cfg.Widgets[len(cfg.Widgets)-1]
		cv, _ := b.RunDraw(w.Draw, 15, 6, 0, 0, 0)
		rows := make([][]rune, cv.H)
		for y := 0; y < cv.H; y++ {
			rows[y] = make([]rune, cv.W)
			for x := 0; x < cv.W; x++ {
				g, _ := cv.At(x, y)
				rows[y][x] = g.Char
			}
		}
		return rows
	}

	// Through the returned interior: the border survives, text is truncated.
	inner := draw(`local i = c:box(0,0,c.w,c.h,"fg=cyan")
    i:text(1,1,"> ws-usb-esp3","fg=green")`)
	if got := inner[2][14]; got != '│' {
		t.Errorf("via interior: col14 = %q, want the border │ intact", got)
	}

	// Drawing on the parent still overwrites it (unchanged old behaviour), which
	// is what made the dock's border disappear.
	parent := draw(`c:box(0,0,c.w,c.h,"fg=cyan")
    c:text(2,2,"> ws-usb-esp3","fg=green")`)
	if got := parent[2][14]; got == '│' {
		t.Error("via parent: expected the old unclipped behaviour to still overwrite the border")
	}
}
