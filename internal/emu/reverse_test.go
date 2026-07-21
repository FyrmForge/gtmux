package emu

import (
	"strings"
	"testing"

	"github.com/FyrmForge/gtmux/internal/geom"
)

// Reverse video must survive as the ";7" SGR param rather than being baked into
// the cell's FG/BG at write time. Baking it in silently dropped the highlight
// for default-colored text: swapping DefaultFG/DefaultBG yields two colors that
// are both still Default(), and writeColor emits nothing for those — so a
// reversed default cell serialized to a bare reset. That is zsh's completion
// menu, whose default `ma` style is standout with no explicit colors.
func TestReverseVideoSurvivesRender(t *testing.T) {
	cases := []struct {
		name string
		seq  string
	}{
		{"default colors", "\033[7mX"},
		{"explicit colors", "\033[7m\033[31;42mX"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			term := New(WithSize(geom.Vec2{R: 5, C: 20}))
			if _, err := term.Write([]byte(tc.seq)); err != nil {
				t.Fatalf("write: %v", err)
			}

			line := term.Screen()[0]
			if line[0].Mode&AttrReverse == 0 {
				t.Error("cell lost the AttrReverse bit")
			}
			if got := RenderLine(line); !strings.Contains(got, ";7") {
				t.Errorf("rendered line missing \";7\": %q", got)
			}
		})
	}
}

// A Glyph built directly (Mode: AttrReverse) — never through setChar — must
// still render ";7". This is the status bar / widget / border path (styleRun in
// statusrender.go builds glyphs this way), which was fully broken before: no
// FG/BG swap happened AND sgr suppressed ";7", so `set -g status-style reverse`
// did nothing. Regression guard for that path specifically.
func TestReverseVideoOnDirectGlyph(t *testing.T) {
	line := Line{{Char: 'X', FG: DefaultFG, BG: DefaultBG, Mode: AttrReverse}}
	if got := RenderLine(line); !strings.Contains(got, ";7") {
		t.Errorf("direct-glyph reverse missing \";7\": %q", got)
	}
}

// Faint (SGR 2) was dropped on the floor — setAttr had no case for it, so dim
// text rendered at normal intensity (Claude Code's tab-completion suggestion
// showed as normal white instead of gray).
func TestDimSurvivesRender(t *testing.T) {
	cases := []struct {
		name string
		seq  string
		want bool
	}{
		{"faint set", "\033[2mX", true},
		{"SGR 0 clears faint", "\033[2m\033[0mX", false},
		{"SGR 22 clears faint", "\033[2m\033[22mX", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			term := New(WithSize(geom.Vec2{R: 5, C: 20}))
			if _, err := term.Write([]byte(tc.seq)); err != nil {
				t.Fatalf("write: %v", err)
			}

			line := term.Screen()[0]
			if got := line[0].Mode&AttrDim != 0; got != tc.want {
				t.Errorf("AttrDim = %v, want %v", got, tc.want)
			}
			if got := strings.Contains(RenderLine(line), ";2"); got != tc.want {
				t.Errorf("rendered %q;2 = %v, want %v", RenderLine(line), got, tc.want)
			}
		})
	}
}

// bold+faint must not hit setChar's bold FG+8 brightening — faint wins.
func TestDimSuppressesBoldBrightening(t *testing.T) {
	term := New(WithSize(geom.Vec2{R: 5, C: 20}))
	if _, err := term.Write([]byte("\033[1;2;31mX")); err != nil {
		t.Fatalf("write: %v", err)
	}

	g := term.Screen()[0][0]
	if fg, ok := g.FG.ANSI(); !ok || fg != 1 {
		t.Errorf("FG = %v (ok=%v), want ANSI 1 — bold+faint must not brighten to 9", fg, ok)
	}
}

// The two attr iota blocks in state.go and module.go are separate declarations
// that must agree bit-for-bit; they had already drifted (attrOpaque existed in
// only one). A mismatch is silent — setAttr sets one bit, sgr reads another.
func TestAttrConstantsAligned(t *testing.T) {
	pairs := []struct {
		name              string
		private, exported int
	}{
		{"Reverse", attrReverse, AttrReverse},
		{"Bold", attrBold, AttrBold},
		{"Gfx", attrGfx, AttrGfx},
		{"Italic", attrItalic, AttrItalic},
		{"Strikethrough", attrStrikethrough, AttrStrikethrough},
		{"Blink", attrBlink, AttrBlink},
		{"Wrap", attrWrap, AttrWrap},
		{"Blank", attrBlank, AttrBlank},
		{"Transparent", attrTransparent, AttrTransparent},
		{"Opaque", attrOpaque, AttrOpaque},
		{"Dim", attrDim, AttrDim},
	}
	for _, p := range pairs {
		if p.private != p.exported {
			t.Errorf("attr%s = %d but Attr%s = %d", p.name, p.private, p.name, p.exported)
		}
	}
}

// The colors themselves must NOT be pre-swapped, or the terminal's own ";7"
// swap would undo them and reverse would render as plain text.
func TestReverseVideoDoesNotPreSwapColors(t *testing.T) {
	term := New(WithSize(geom.Vec2{R: 5, C: 20}))
	if _, err := term.Write([]byte("\033[7m\033[31;42mX")); err != nil {
		t.Fatalf("write: %v", err)
	}

	g := term.Screen()[0][0]
	if fg, ok := g.FG.ANSI(); !ok || fg != 1 {
		t.Errorf("FG = %v (ok=%v), want ANSI 1 (red) unswapped", fg, ok)
	}
	if bg, ok := g.BG.ANSI(); !ok || bg != 2 {
		t.Errorf("BG = %v (ok=%v), want ANSI 2 (green) unswapped", bg, ok)
	}
}
