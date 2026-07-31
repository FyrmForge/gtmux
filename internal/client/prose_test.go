package client

import (
	"testing"

	"github.com/FyrmForge/gtmux/internal/emu"
)

func proseTestLine(s string) emu.Line {
	l := make(emu.Line, len([]rune(s)))
	for i, r := range []rune(s) {
		l[i] = emu.Glyph{Char: r, FG: emu.DefaultFG, BG: emu.DefaultBG}
	}
	return l
}

func TestProseLine(t *testing.T) {
	src := proseTestLine(`the Server ran 3 tests, see "docs" or ` + "`go test`")
	out := proseLine(src)

	at := func(i int) emu.Glyph { return out[i] }
	if at(0).Mode&emu.AttrDim == 0 {
		t.Error("'the' should be dim (function word)")
	}
	if at(4).Mode&emu.AttrBold == 0 {
		t.Error("'Server' should be bold (capitalized)")
	}
	if at(15).FG != emu.Cyan {
		t.Errorf("'3' should be cyan, got %v", at(15).FG)
	}
	if at(31).FG != emu.Green { // inside "docs"
		t.Errorf(`"docs" should be green, got %v`, at(31).FG)
	}
	if at(len(out)-2).FG != emu.Yellow { // inside `go test`
		t.Errorf("code span should be yellow, got %v", at(len(out)-2).FG)
	}

	// App-styled cells are untouchable: a red word keeps its color and never
	// leaks category state into neighbors.
	styled := proseTestLine("ok the end")
	styled[3].FG = emu.Red // 't' of "the"
	out = proseLine(styled)
	if out[3].FG != emu.Red || out[3].Mode&emu.AttrDim != 0 {
		t.Error("app-styled cell was recolored")
	}
	// Source must never be mutated (toggle-off must revert on repaint).
	if src[0].Mode&emu.AttrDim != 0 {
		t.Error("proseLine mutated its input")
	}
}
