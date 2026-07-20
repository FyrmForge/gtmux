package client

import (
	"fmt"

	"github.com/FyrmForge/gtmux/internal/config"
	"github.com/FyrmForge/gtmux/internal/emu"
)

// styleRun renders s as a fresh glyph run in fg/bg with mode attr.
func styleRun(s string, fg, bg emu.Color, attr int16) []emu.Glyph {
	out := make([]emu.Glyph, 0, len(s))
	for _, r := range s {
		out = append(out, emu.Glyph{Char: r, FG: fg, BG: bg, Mode: attr})
	}
	return out
}

// renderPromptLine draws a "(label) text" status line — the copy-mode help,
// an active prompt, or a transient server message. These are client input
// modes, not status content, so they still override the status row directly
// (the status bar itself is now a component; see compositor.statusWidget).
// Keeps the tail visible if it overflows (so a long typed line shows its
// cursor end).
func renderPromptLine(cols int, label, text string, cfg config.ClientConfig) emu.Line {
	// message-style: transient messages and the command prompt share this style.
	fg, bg, attr := cfg.MessageFG, cfg.MessageBG, cfg.MessageAttr
	line := styleRun(fmt.Sprintf("(%s) %s", label, text), fg, bg, attr)
	if len(line) > cols {
		line = line[len(line)-cols:]
	}
	for len(line) < cols {
		line = append(line, emu.Glyph{Char: ' ', FG: fg, BG: bg, Mode: attr})
	}
	return emu.Line(line)
}
