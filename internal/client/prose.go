package client

import (
	"strings"
	"unicode"

	"github.com/FyrmForge/gtmux/internal/emu"
)

// Prose highlighting: "syntax highlighting for English" in agent panes
// (claude/codex/… per gtmux.agents{}), a dyslexia aid — a wall of white prose
// gets the same categorical coloring that makes code readable. Only cells
// still in the default fg/bg are touched, so the agent's own colors (diffs,
// code blocks, spinners) always win.
//
// Categories:
//   `code spans`     yellow
//   "quoted text"    green
//   numbers          cyan
//   function words   dim   (the/of/and… — recede so content words pop)
//   Capitalized      bold  (names, sentence anchors)
//   punctuation      dim
//
// ponytail: line-local only — a code span or quote broken across a wrap loses
// its color on the continuation line. Per-word category, no NLP.

var functionWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(
		"the a an of to in on at by for from with as and or but not no nor so " +
			"if then else when while that this these those which who whom whose " +
			"what how why where is are was were be been being am do does did " +
			"done can could will would shall should may might must have has had " +
			"it its i you he she we they me him her us them my your his our " +
			"their mine yours theirs there here than into onto over under about " +
			"after before between through during against up down out off again " +
			"once just only also very too more most some any each both few own " +
			"same such now let lets") {
		functionWords[w] = true
	}
}

// proseEligible reports whether a cell is still unstyled by the app — the only
// cells prose highlighting may recolor (or trust while scanning for spans).
func proseEligible(g emu.Glyph) bool {
	return g.FG == emu.DefaultFG && g.BG == emu.DefaultBG
}

// proseLine returns a recolored copy of one pane row. The source line is never
// mutated (the toggle must revert cleanly on the next repaint).
func proseLine(src emu.Line) emu.Line {
	out := make(emu.Line, len(src))
	copy(out, src)

	const (
		catNone = iota
		catCode
		catQuote
		catNumber
		catFunc
		catCap
		catPunct
	)
	cat := make([]uint8, len(src))

	// Span pass: backtick code spans and double-quoted spans. A '`' or '"'
	// inside app-styled cells is the app's own output — never toggles ours.
	inCode, inQuote := false, false
	for i, g := range src {
		if !proseEligible(g) {
			continue
		}
		switch g.Char {
		case '`':
			cat[i] = catCode
			inCode = !inCode
			continue
		case '"', '“', '”':
			if !inCode {
				cat[i] = catQuote
				inQuote = g.Char == '“' || (g.Char == '"' && !inQuote)
				continue
			}
		}
		switch {
		case inCode:
			cat[i] = catCode
		case inQuote:
			cat[i] = catQuote
		}
	}

	// Word pass: tokenize the remaining eligible cells. Char 0 spacer cells
	// (under a wide rune) stay part of the current token.
	isWordRune := func(r rune) bool {
		return r == 0 || r == '\'' || r == '’' || r == '-' || r == '_' ||
			unicode.IsLetter(r) || unicode.IsDigit(r)
	}
	i := 0
	for i < len(src) {
		g := src[i]
		if cat[i] != catNone || !proseEligible(g) || g.Char == ' ' || g.Char == 0 {
			i++
			continue
		}
		if !isWordRune(g.Char) {
			cat[i] = catPunct
			i++
			continue
		}
		j := i
		var word []rune
		hasDigit, hasLetter := false, false
		for j < len(src) && cat[j] == catNone && proseEligible(src[j]) && isWordRune(src[j].Char) {
			r := src[j].Char
			if r != 0 {
				word = append(word, r)
				hasDigit = hasDigit || unicode.IsDigit(r)
				hasLetter = hasLetter || unicode.IsLetter(r)
			}
			j++
		}
		wcat := uint8(catNone)
		switch {
		case hasDigit && !hasLetter:
			wcat = catNumber
		case len(word) > 0 && unicode.IsUpper(word[0]):
			wcat = catCap
		case functionWords[strings.ToLower(string(word))]:
			wcat = catFunc
		}
		for k := i; k < j; k++ {
			cat[k] = wcat
		}
		i = j
	}

	for i, g := range out {
		switch cat[i] {
		case catCode:
			g.FG = emu.Yellow
		case catQuote:
			g.FG = emu.Green
		case catNumber:
			g.FG = emu.Cyan
		case catFunc, catPunct:
			g.Mode |= emu.AttrDim
		case catCap:
			g.Mode |= emu.AttrBold
		default:
			continue
		}
		out[i] = g
	}
	return out
}
