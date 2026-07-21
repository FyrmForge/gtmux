package client

import (
	"testing"

	"github.com/FyrmForge/gtmux/internal/emu"
)

// A wide rune (CJK) must occupy two cells — the glyph plus a Char==0 spacer —
// and must NOT spill past maxCols. Before this, dock/widget text laid one rune
// per column with no spacer, so a wide rune advanced the terminal two columns
// and pushed the rest of the strip across the dock border.
func TestLayTextWideRuneSpacerAndClip(t *testing.T) {
	newLine := func(n int) emu.Line {
		l := make(emu.Line, n)
		for i := range l {
			l[i] = emu.Glyph{Char: ' '}
		}
		return l
	}

	// "a世b" into 4 columns: a@0, 世@1 (+spacer@2), b@3.
	line := newLine(6)
	used := layText(line, "a世b", 0, 4, emu.DefaultFG, emu.DefaultBG, 0)
	if used != 4 {
		t.Fatalf("used = %d, want 4", used)
	}
	if line[0].Char != 'a' || line[1].Char != '世' || line[2].Char != 0 || line[3].Char != 'b' {
		t.Errorf("cells = %q %q %v %q, want a 世 <spacer:0> b",
			line[0].Char, line[1].Char, line[2].Char, line[3].Char)
	}

	// maxCols=2: 'a' fits (col 0), '世' needs cols 1-2 → would straddle the
	// boundary, so it is dropped rather than spilling. used=1.
	line2 := newLine(6)
	used2 := layText(line2, "a世b", 0, 2, emu.DefaultFG, emu.DefaultBG, 0)
	if used2 != 1 {
		t.Errorf("used = %d, want 1 (wide rune dropped at boundary)", used2)
	}
	if line2[1].Char == '世' {
		t.Error("wide rune spilled past maxCols instead of being dropped")
	}
}
