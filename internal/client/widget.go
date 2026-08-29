package client

import (
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"

	"github.com/FyrmForge/gtmux/internal/config"
	"github.com/FyrmForge/gtmux/internal/emu"
)

// widget is the compositor's overlay primitive: a thing that composites its own
// slice of one window-space row on top of already-drawn pane content. The
// existing overlays (popup, picker, clock, lock) all already have this exact
// shape as ad-hoc compositor methods; this names the contract so user-defined
// UIs are "just another widget" registered in c.overlays — the same mechanism
// gtmux's built-in chrome uses, not a second render path.
//
// row is window-space (post-contentOffset, matching popupRow/overlayRow); cols
// is the physical width for bounds. Widgets paint over window content only, not
// the reserved status rows (buildRow returns those early) — a known boundary
// for now.
type widget interface {
	paintRow(row, cols int, line emu.Line)
}

// formatWidget is a widget whose content is a status-style format string,
// re-expanded on each Status tick (same vars/#client()/#server() as the status
// bar). dirtyRows reports the window-space rows it occupies so apply can repaint
// them when the expansion changes.
type formatWidget interface {
	reexpand(exp *statusExpander, vars, serverShell map[string]string)
	dirtyRows() (start, count int)
}

// textBox is the minimal concrete widget: a rectangle of text lines at a fixed
// window-space position, one style. lines is the currently-displayed content;
// format (if set) is the status-format string it re-expands from each tick.
type textBox struct {
	row, col int
	format   string // status-format source; "" = static lines only
	lines    []string
	fg, bg   emu.Color
	attr     int16
	dock     string // "" = float; "left"/"right" = docked column strip
	size     int    // reserved columns when docked
	// A dynamic widget: textFn returns the text (run on binds' VM each refresh),
	// drawFn paints a 2D canvas instead, onClick runs when clicked. interval
	// throttles re-runs (0 = every tick). binds owns the VM the functions live in.
	// w,h are the widget's region size (set by the compositor for docks, from the
	// spec for floats), used to size the draw canvas.
	textFn    *lua.LFunction
	drawFn    *lua.LFunction
	component *lua.LFunction
	onClick   *lua.LFunction
	onKey     *lua.LFunction // modal keyboard handler (gtmux.open on_key / focusable dock)
	focus     string         // dock focusability: "nav" | "bind" | "both" | ""
	name      string         // dock name for gtmux.focus_dock(name)
	minCols   int            // auto-hide when client width < minCols (0 = never)
	forced    int8           // toggle_dock override: 0 auto, 1 shown, -1 hidden
	binds     *config.ClientBinds
	canvas    *config.Canvas
	regions   []config.Region // clickable rects emitted by the last draw (component)
	state     *lua.LTable     // component's persistent ui:state() store (survives redraws)
	ops       []config.BindOp // ops the last draw emitted (gtmux.run_command), drained by the client
	w, h      int
	interval  int
	lastRun   time.Time
	lastSig   string // last canvasSig, so the animation ticker skips unchanged frames
}

// hidden reports whether a docked widget should be dropped from the visible
// set at client width cols: a toggle_dock override wins outright; otherwise
// the min_cols breakpoint decides (cols 0 = size unknown yet, show it).
func (b *textBox) hidden(cols int) bool {
	if b.forced != 0 {
		return b.forced < 0
	}
	return b.minCols > 0 && cols > 0 && cols < b.minCols
}

// canvasSig is a cheap content signature (glyph chars per row) used to detect
// whether a re-run draw actually changed — so the animation ticker only repaints
// when something moved. Chars only: colour-only changes ride the status tick.
func (b *textBox) canvasSig() string {
	if b.canvas == nil {
		return ""
	}
	var sb strings.Builder
	for r := 0; r < b.canvas.H; r++ {
		sb.WriteString(b.lineText(r))
		sb.WriteByte('\n')
	}
	return sb.String()
}

func (b *textBox) reexpand(exp *statusExpander, vars, serverShell map[string]string) {
	if b.throttled() {
		return
	}
	if b.component != nil || b.drawFn != nil || b.textFn != nil {
		b.rerender()
		return
	}
	if b.format == "" {
		return
	}
	b.lines = strings.Split(exp.expand(b.format, vars, serverShell), "\n")
}

// rerender re-runs a dynamic widget's Lua source (component/draw/text) now,
// bypassing the interval throttle. reexpand calls it on the status tick; the
// click path calls it directly so a state change from an on_click shows
// immediately. A component threads its persistent state table through so
// ui:state() survives across renders.
func (b *textBox) rerender() {
	b.lastRun = time.Now()
	switch {
	case b.component != nil:
		var ops []config.BindOp
		b.canvas, b.regions, b.state, ops = b.binds.RunComponent(b.component, b.state, b.w, b.h, b.fg, b.bg, b.attr)
		b.ops = append(b.ops, ops...)
	case b.drawFn != nil:
		var ops []config.BindOp
		b.canvas, b.regions, ops = b.binds.RunDraw(b.drawFn, b.w, b.h, b.fg, b.bg, b.attr)
		b.ops = append(b.ops, ops...)
	case b.textFn != nil:
		b.lines = strings.Split(b.binds.RunText(b.textFn), "\n")
	}
}

// throttled reports whether a dynamic widget's interval hasn't elapsed yet (only
// applies to textFn/drawFn widgets; static/format ones always re-expand).
func (b *textBox) throttled() bool {
	if b.textFn == nil && b.drawFn == nil && b.component == nil {
		return false
	}
	return b.interval > 0 && !b.lastRun.IsZero() && time.Since(b.lastRun) < time.Duration(b.interval)*time.Second
}

// rowCount is the widget's drawn height: canvas rows for a draw widget, text
// lines otherwise.
func (b *textBox) rowCount() int {
	if b.canvas != nil {
		return b.canvas.H
	}
	return len(b.lines)
}

// lineText reconstructs row i's text (for on_click hit info): from the canvas row
// if drawing, else the text line.
func (b *textBox) lineText(i int) string {
	if b.canvas != nil {
		if i < 0 || i >= b.canvas.H {
			return ""
		}
		rs := make([]rune, b.canvas.W)
		for x := 0; x < b.canvas.W; x++ {
			g, _ := b.canvas.At(x, i)
			rs[x] = g.Char
		}
		return string(rs)
	}
	if i < 0 || i >= len(b.lines) {
		return ""
	}
	return b.lines[i]
}

func (b *textBox) dirtyRows() (int, int) { return b.row, b.rowCount() }

// regionAt returns the on_click fn of the topmost region containing widget-local
// (x=col, y=row), or nil. Reverse order so the last-emitted (deepest child) wins.
func (b *textBox) regionAt(x, y int) *lua.LFunction {
	for i := len(b.regions) - 1; i >= 0; i-- {
		r := b.regions[i]
		if x >= r.X && x < r.X+r.W && y >= r.Y && y < r.Y+r.H {
			return r.OnClick
		}
	}
	return nil
}

// layText lays s's runes into line starting at col0, clipped to maxCols DISPLAY
// columns, and returns the number of columns written. A wide rune (CJK/emoji)
// occupies its true width: the rune in one cell, then a Char==0 spacer cell per
// extra column — the invariant emu.WriteLine relies on to keep columns aligned
// (it emits one rune per non-zero cell, advancing by the glyph's own width). A
// wide rune that would straddle the maxCols boundary is dropped, not spilled —
// spilling is exactly what bled dock text across the border. Shared by the
// left/right-dock strip and the float-widget row so both stay width-correct.
func layText(line emu.Line, s string, col0, maxCols int, fg, bg emu.Color, attr int16) int {
	used := 0
	for _, r := range s {
		g := emu.Glyph{Char: r, FG: fg, BG: bg, Mode: attr}
		w := g.Width()
		if used+w > maxCols {
			break
		}
		if col := col0 + used; col >= 0 && col < len(line) {
			line[col] = g
		}
		for k := 1; k < w; k++ { // spacer cells under a wide glyph
			if c := col0 + used + k; c >= 0 && c < len(line) {
				line[c] = emu.Glyph{Char: 0, FG: fg, BG: bg, Mode: attr}
			}
		}
		used += w
	}
	return used
}

// paintStrip fills a docked widget's reserved column strip for one window row:
// a solid style background across `size` cols starting at colStart, with the
// row's text (if any) left-aligned into it. Used for left/right docks.
func (b *textBox) paintStrip(winRow, colStart, size int, line emu.Line) {
	// Draw widget: blit canvas row winRow directly (per-cell style).
	if b.canvas != nil {
		for x := 0; x < size; x++ {
			col := colStart + x
			if col < 0 || col >= len(line) {
				continue
			}
			if g, ok := b.canvas.At(x, winRow); ok {
				line[col] = g
			} else {
				line[col] = emu.Glyph{Char: ' ', FG: b.fg, BG: b.bg, Mode: b.attr}
			}
		}
		return
	}
	used := 0
	if winRow >= 0 && winRow < len(b.lines) {
		used = layText(line, b.lines[winRow], colStart, size, b.fg, b.bg, b.attr)
	}
	for x := used; x < size; x++ { // style-fill the strip's remaining columns
		if col := colStart + x; col >= 0 && col < len(line) {
			line[col] = emu.Glyph{Char: ' ', FG: b.fg, BG: b.bg, Mode: b.attr}
		}
	}
}

func (b *textBox) paintRow(row, cols int, line emu.Line) {
	i := row - b.row
	// Draw widget: blit canvas row i starting at b.col.
	if b.canvas != nil {
		if i < 0 || i >= b.canvas.H {
			return
		}
		for x := 0; x < b.canvas.W; x++ {
			col := b.col + x
			if col < 0 || col >= cols || col >= len(line) {
				continue
			}
			if g, ok := b.canvas.At(x, i); ok {
				line[col] = g
			}
		}
		return
	}
	if i < 0 || i >= len(b.lines) {
		return
	}
	if maxCols := cols - b.col; maxCols > 0 {
		layText(line, b.lines[i], b.col, maxCols, b.fg, b.bg, b.attr)
	}
}
