package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FyrmForge/gtmux/internal/emu"
)

// ui:text lays runes by display width: "☎️" is one cell marked emoji (the
// selector takes none), "🤝" is two cells (glyph + spacer), and what follows
// lands after them. The selector used to get a cell of its own, which hung
// the renderer (repo description "☎️ 🤝 React UI ...").
func TestTextEmojiWidths(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.lua")
	src := `gtmux.widget{ dock="left", size=20, draw = function(c) c:text(0, 0, "☎️ 🤝 x") end }`
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, b := LoadClient(p)
	defer b.Close()
	cv, _, _ := b.RunDraw(cfg.Widgets[len(cfg.Widgets)-1].Draw, 20, 1, 0, 0, 0)
	cell := func(x int) emu.Glyph { g, _ := cv.At(x, 0); return g }
	if g := cell(0); g.Char != '☎' || g.Mode&emu.AttrEmoji == 0 {
		t.Fatalf("cell0 = %+v, want ☎ marked emoji", g)
	}
	if cell(1).Char != ' ' || cell(2).Char != '🤝' || cell(3).Char != 0 || cell(4).Char != ' ' || cell(5).Char != 'x' {
		t.Fatalf("cells = %q %q %q %q %q", cell(1).Char, cell(2).Char, cell(3).Char, cell(4).Char, cell(5).Char)
	}
	line := make(emu.Line, cv.W)
	for x := range line {
		line[x] = cell(x)
	}
	var sb strings.Builder
	emu.WriteLine(&sb, line)
	if !strings.Contains(sb.String(), "☎️") || !strings.Contains(sb.String(), "🤝") {
		t.Fatalf("rendered %q", sb.String())
	}
}
