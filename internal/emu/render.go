package emu

import (
	"fmt"
	"strings"
)

// WriteLine emits SGR-attributed runs of a line's glyphs, resetting attrs only
// between runs that differ so we don't emit an SGR code per character. Shared by
// the client's screen renderer and the server's capture-pane -e.
func WriteLine(b *strings.Builder, line Line) {
	haveAttrs := false
	var last Glyph

	for _, g := range line {
		if g.Char == 0 {
			continue
		}
		if !haveAttrs || !g.SameAttrs(last) {
			b.WriteString(sgr(g))
			last = g
			haveAttrs = true
		}
		b.WriteRune(g.Char)
	}
	b.WriteString("\x1b[0m")
}

// RenderLine returns one line as an SGR-escaped string (capture-pane -e).
func RenderLine(line Line) string {
	var b strings.Builder
	WriteLine(&b, line)
	return b.String()
}

// sgr builds the escape sequence selecting a glyph's colors and attributes.
func sgr(g Glyph) string {
	var b strings.Builder
	b.WriteString("\x1b[0")

	if g.Mode&AttrBold != 0 {
		b.WriteString(";1")
	}
	if g.Mode&AttrItalic != 0 {
		b.WriteString(";3")
	}
	if g.Underline.Mode != UnderlineNone {
		b.WriteString(";4")
	}
	if g.Mode&AttrBlink != 0 {
		b.WriteString(";5")
	}
	// Deliberately not re-emitting ";7" (reverse video) here: emu's setChar
	// already bakes reverse video into FG/BG at write time (swaps them and
	// stores the swapped colors), but leaves the AttrReverse bit set on the
	// cell. Re-sending ";7" would make the real terminal swap a second time,
	// undoing it.
	if g.Mode&AttrStrikethrough != 0 {
		b.WriteString(";9")
	}

	writeColor(&b, g.FG, 38)
	writeColor(&b, g.BG, 48)

	b.WriteString("m")
	return b.String()
}

// writeColor emits the SGR color-select code for a foreground (base=38) or
// background (base=48) color: truecolor if set, else the basic 16-color
// codes (30-37/90-97, or 40-47/100-107 for background) when in that range so
// terminals apply the user's actual theme, else the xterm 256-color index.
func writeColor(b *strings.Builder, c Color, base int) {
	if r, g, bl, ok := c.RGB(); ok {
		fmt.Fprintf(b, ";%d;2;%d;%d;%d", base, r, g, bl)
		return
	}
	if idx, ok := c.ANSI(); ok {
		basic := base - 8 // 38->30, 48->40
		if idx < 8 {
			fmt.Fprintf(b, ";%d", basic+idx)
		} else {
			fmt.Fprintf(b, ";%d", basic+60+idx-8)
		}
		return
	}
	if idx, ok := c.XTerm(); ok {
		fmt.Fprintf(b, ";%d;5;%d", base, idx)
	}
}
