package client

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/FyrmForge/gtmux/internal/config"
	"github.com/FyrmForge/gtmux/internal/emu"
	"github.com/FyrmForge/gtmux/internal/proto"
)

// truncCells caps s to n cells (n<=0 = unlimited). ponytail: rune-count, so a
// wide char counts as one cell — refine if double-width truncation ever matters.
func truncCells(s string, n int) string {
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// styleRun renders s as a fresh glyph run in fg/bg with mode attr.
func styleRun(s string, fg, bg emu.Color, attr int16) []emu.Glyph {
	out := make([]emu.Glyph, 0, len(s))
	for _, r := range s {
		out = append(out, emu.Glyph{Char: r, FG: fg, BG: bg, Mode: attr})
	}
	return out
}

// renderPromptLine draws a "(label) text" status line — the copy-mode help,
// an active prompt, or a transient server message. Keeps the tail visible if
// it overflows (so a long typed line shows its cursor end).
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

// windowHit records the column span [start,end) of one window's label in the
// status bar, so resolveMouse can map a click to a select-window without
// re-deriving renderBar's justify/format layout.
type windowHit struct {
	index      int
	start, end int
}

// windowVars is the per-entry var map for window-status[-current]-format
// expansion, built from the WindowInfo the server sent.
func windowVars(w proto.WindowInfo) map[string]string {
	flags := ""
	if w.Active {
		flags += "*"
	}
	if w.Activity {
		flags += "#"
	}
	if w.Bell {
		flags += "!"
	}
	if w.Silence {
		flags += "~"
	}
	if w.Zoomed {
		flags += "Z"
	}
	active := ""
	if w.Active {
		active = "1"
	}
	return map[string]string{
		"window_index":  strconv.Itoa(w.Index),
		"window_name":   w.Name,
		"window_flags":  flags,
		"window_active": active,
		"window_panes":  strconv.Itoa(w.Panes),
	}
}

// renderExtraStatus draws one of the additional status lines (tmux `status`
// 2..5): the already-expanded ExtraStatusFormats[idx], full-width, left-
// justified, in the status style. A blank format yields a blank styled line.
func (c *compositor) renderExtraStatus(idx int) emu.Line {
	fg, bg, attr := c.cfg.StatusFG, c.cfg.StatusBG, c.cfg.StatusAttr
	cols := c.cols()
	var s string
	if idx >= 0 && idx < len(c.stExtra) {
		s = c.stExtra[idx]
	}
	line := styleRun(truncCells(s, cols), fg, bg, attr)
	for len(line) < cols {
		line = append(line, emu.Glyph{Char: ' ', FG: fg, BG: bg, Mode: attr})
	}
	return emu.Line(line)
}

// renderBar lays out the normal status bar: the (already-expanded) left format,
// the window list (each entry from window-status[-current]-format, joined by
// the separator, positioned by status-justify), then the right format. It also
// records each window's click span in c.windowHits for resolveMouse.
func (c *compositor) renderBar() emu.Line {
	cfg := c.cfg
	fg, bg, attr := cfg.StatusFG, cfg.StatusBG, cfg.StatusAttr
	cols := c.cols()

	styled := func(s string, f, b emu.Color) []emu.Glyph { return styleRun(s, f, b, attr) }

	// status-left/right-length cap each segment (0 = unlimited).
	left := styled(truncCells(c.stLeft, cfg.StatusLeftLength), fg, bg)
	right := styled(truncCells(c.stRight, cfg.StatusRightLength), fg, bg)

	// Window list, tracking each entry's column span (relative to the list's
	// own start; the justify offset is added when the hits are recorded).
	c.windowHits = c.windowHits[:0]
	var wlist []emu.Glyph
	type span struct {
		index      int
		start, end int
	}
	var spans []span
	for i, w := range c.status.Windows {
		if i > 0 {
			wlist = append(wlist, styled(cfg.WindowStatusSeparator, fg, bg)...)
		}
		f, b := fg, bg
		fmtStr := cfg.WindowStatusFormat
		if w.Active {
			f, b, fmtStr = cfg.ActiveWindowFG, cfg.ActiveWindowBG, cfg.WindowStatusCurrentFormat
		}
		label := c.expander.expand(fmtStr, windowVars(w), c.status.ServerShell)
		start := len(wlist)
		wlist = append(wlist, styled(label, f, b)...)
		spans = append(spans, span{w.Index, start, len(wlist)})
	}

	fill := func(n int) []emu.Glyph {
		if n < 0 {
			n = 0
		}
		return styled(strings.Repeat(" ", n), fg, bg)
	}
	slack := cols - len(left) - len(wlist) - len(right)
	if slack < 0 {
		slack = 0
	}
	var gapL int
	switch cfg.StatusJustify {
	case "right":
		gapL = slack
	case "centre", "center":
		gapL = slack / 2
	}
	gapR := slack - gapL

	listStart := len(left) + gapL
	for _, s := range spans {
		c.windowHits = append(c.windowHits, windowHit{s.index, listStart + s.start, listStart + s.end})
	}

	line := left
	line = append(line, fill(gapL)...)
	line = append(line, wlist...)
	line = append(line, fill(gapR)...)
	line = append(line, right...)

	if len(line) > cols {
		line = line[:cols]
	}
	for len(line) < cols {
		line = append(line, emu.Glyph{Char: ' ', FG: fg, BG: bg, Mode: attr})
	}
	return emu.Line(line)
}
