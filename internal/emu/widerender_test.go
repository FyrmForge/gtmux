package emu

import (
	"strings"
	"testing"
)

// A double-width rune occupies two grid cells: the rune plus a ' ' placeholder
// written by setChar. The terminal advances two columns on the rune alone, so
// emitting the placeholder too shifts everything after it one column right —
// which is how hamr's "🔨" status bar pushed the pane border off its column.
// WriteLine must skip the placeholder.
func TestWriteLineSkipsWidePlaceholder(t *testing.T) {
	// Cells: 🔨 | placeholder | 'x' | '│'  → four grid cells, four screen columns.
	line := Line{
		{Char: '🔨', FG: DefaultFG, BG: DefaultBG},
		{Char: ' ', FG: DefaultFG, BG: DefaultBG},
		{Char: 'x', FG: DefaultFG, BG: DefaultBG},
		{Char: '│', FG: DefaultFG, BG: DefaultBG},
	}
	got := stripSGR(RenderLine(line))
	if want := "🔨x│"; got != want {
		t.Errorf("RenderLine = %q, want %q (placeholder after the wide rune must be skipped)", got, want)
	}
	// The rendered text must occupy exactly as many columns as the line has cells.
	if cols := lineCols(got); cols != len(line) {
		t.Errorf("rendered %q spans %d columns, want %d (one per grid cell)", got, cols, len(line))
	}
}

// Narrow runes are unaffected: every cell still emits exactly one character.
func TestWriteLineNarrowUnchanged(t *testing.T) {
	line := Line{
		{Char: 'a', FG: DefaultFG, BG: DefaultBG},
		{Char: 'b', FG: DefaultFG, BG: DefaultBG},
		{Char: '│', FG: DefaultFG, BG: DefaultBG},
	}
	if got, want := stripSGR(RenderLine(line)), "ab│"; got != want {
		t.Errorf("RenderLine = %q, want %q", got, want)
	}
}

// Unwritten cells (Char == 0) are still skipped entirely, and must not be
// mistaken for a wide-rune placeholder.
func TestWriteLineSkipsUnwritten(t *testing.T) {
	line := Line{
		{Char: 'a', FG: DefaultFG, BG: DefaultBG},
		{Char: 0},
		{Char: 'b', FG: DefaultFG, BG: DefaultBG},
	}
	if got, want := stripSGR(RenderLine(line)), "ab"; got != want {
		t.Errorf("RenderLine = %q, want %q", got, want)
	}
}

// A wide rune printed through the emulator round-trips at its true width: the
// grid holds rune+placeholder, the render emits one rune spanning two columns.
func TestPrintWideRoundTrip(t *testing.T) {
	term := New()
	if _, err := term.Write([]byte("🔨ab")); err != nil {
		t.Fatalf("write: %v", err)
	}
	line := term.Screen()[0]
	if line[0].Char != '🔨' {
		t.Fatalf("cell 0 = %q, want the wide rune", line[0].Char)
	}
	if line[1].Char != ' ' {
		t.Errorf("cell 1 = %q, want the ' ' placeholder (storage convention unchanged)", line[1].Char)
	}
	if line[2].Char != 'a' || line[3].Char != 'b' {
		t.Errorf("cells 2,3 = %q,%q, want 'a','b'", line[2].Char, line[3].Char)
	}
	if got := stripSGR(RenderLine(line)); !strings.HasPrefix(got, "🔨ab") {
		t.Errorf("rendered %q, want it to start with %q", got, "🔨ab")
	}
}

// stripSGR removes the escape sequences so a test can compare visible text.
func stripSGR(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			i++ // past the 'm'
			continue
		}
		r := []rune(s[i:])[0]
		b.WriteRune(r)
		i += len(string(r))
	}
	return b.String()
}

// lineCols is the column count the given text occupies on a real terminal.
func lineCols(s string) int {
	n := 0
	for _, r := range s {
		n += Glyph{Char: r}.Width()
	}
	return n
}
