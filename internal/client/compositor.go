// Package client's compositor takes over what the server used to do itself:
// combine per-pane content with borders, the number overlay, and the status
// bar into one physical screen, and turn only what changed into ANSI bytes.
// The server now only ever sends structural/content data (Layout,
// PaneContent, StatusInfo) — this file is the "client decides how it looks"
// half of that split.
package client

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"

	"github.com/FyrmForge/gtmux/internal/config"
	"github.com/FyrmForge/gtmux/internal/emu"
	"github.com/FyrmForge/gtmux/internal/proto"
)

// paneMeta is what the compositor remembers about a pane beyond its raw
// content: where its cursor is (pane-local) and whether it's visible.
type paneMeta struct {
	cursor  emu.Cursor
	visible bool
}

// compositor holds everything needed to redraw the real terminal: the last
// layout received, each pane's content buffered by ID (so a partial update
// can be recombined with what's already known about the rest of the row),
// and the current status bar data.
//
// phyCols/phyRows are this client's own terminal size, which can differ from
// the window (layout) size when a session is attached by several differently
// sized clients: the window follows the acting client, and every other client
// clips it (if smaller) or dot-fills the slack (if larger).
type compositor struct {
	layout           *proto.Layout
	panes            map[int]map[int]emu.Line // paneID -> local row -> line
	meta             map[int]paneMeta
	status           *proto.StatusInfo
	cfg              config.ClientConfig
	phyCols, phyRows int
	// copy/prompt/picker are this client's local input modes — non-nil while
	// browsing scrollback, typing a rename/command, or choosing a
	// window/session. Mutually exclusive; each renders its own chrome (copy
	// pane / status line / overlay box) and intercepts keys in the input loop.
	copy    *copyMode
	prompt  *prompt
	picker  *picker
	popup   *popupOverlay // display-popup floating terminal, or nil
	clock   bool          // clock-mode overlay (shows the time; any key dismisses)
	locked  bool          // lock overlay (unlocks on any key, or on the configured password)
	lockBuf []byte        // typed password so far while locked (never rendered)

	// expander expands #{...} formats for status components that call
	// gtmux.expand() (via the client's Expand hook); it also serves #client/
	// #server shell caching.
	expander *statusExpander
	// set-titles: lastTitle dedupes the outer-terminal title OSC (only emit on
	// change); titlePushed records that we saved the original title on the
	// terminal's title stack (\e[22;2t) so restoreTitle can pop it on detach.
	lastTitle   string
	titlePushed bool

	// extended-keys: the kitty-keyboard flags we last pushed to the outer terminal
	// to match the active pane's app (0 = nothing pushed). We keep at most one
	// entry on the terminal's kitty stack; renegotiated when the active pane's
	// KeyFlags change and popped on detach.
	kittyFlags int

	// Client-owned mouse gesture state (the client recognizes focus/border/
	// drag-copy from its own Layout, sending semantic requests to the server).
	// dragBorder is the index into layout.Borders being dragged, or -1. dcArmed
	// marks a left-press over a non-tracking pane; the first motion after it
	// fires drag-to-copy (dcActive suppresses re-entry). All cleared on release.
	dragBorder   int
	dcPane       int
	dcRow, dcCol int
	dcArmed      bool
	dcActive     bool

	// overlays are user-registered floating widgets composited on top of window
	// content, the same way popup/picker/clock/lock are (they'll migrate here).
	overlays []widget
	// docks are user-registered widgets that reserve a column strip on the left
	// or right edge (textBox.dock). They shrink the window content the client
	// reports to the server; content flows in the middle. Horizontal (top/bottom)
	// docking is still the status bar's job.
	docks []*textBox
	// statusWidget, if set, is a component that paints the status rows (a widget
	// registered with dock="status"). It replaces the bespoke renderBar content
	// while the scaffolding (statusLines/status_position + the copy-mode/prompt/
	// message overrides) stays put. nil = the built-in renderBar path. Held in its
	// own slot, not c.docks, so those overrides keep anchoring to statusRow().
	statusWidget *textBox
	// modal, if set, is a modal keyboard widget (gtmux.open{...}): a centered
	// component overlay that grabs every key via its on_key until it closes.
	// Mutually exclusive with the other input modes; rendered topmost.
	modal    *textBox
	modalPos string // "center" (default) | "status" (renders on the status line)
	// snapshot is the whole-server state the server pushes on each status tick when
	// this client uses widget queries; the Lua primitives (gtmux.sessions/panes/…)
	// read it. Nil until the first snapshot arrives.
	snapshot *proto.StateSnapshot
	// alertPrev holds the last-seen [bell,activity,silence] flags per window
	// (keyed session\x00index) so apply() can fire gtmux.on callbacks on a
	// false→true edge only. alertSeeded gates the first snapshot: it primes
	// alertPrev without firing, so pre-existing alerts don't fire on attach.
	alertPrev     map[string][3]bool
	alertSeeded   bool
	pendingAlerts []config.AlertEvent
	// paneBorderColor overrides a pane's border color (pane:set_border, e.g. from
	// gtmux.on("command-exited")). Keyed by pane ID; cleared when the pane gains
	// focus. Empty map = no overrides.
	paneBorderColor map[int]emu.Color
	// prevCommand tracks each pane's last-seen foreground command (from the
	// snapshot) so apply() can fire gtmux.on("program-changed") on a change.
	// progSeeded gates the first snapshot (prime without firing), like alerts.
	prevCommand     map[int]string
	progSeeded      bool
	pendingProgram  []programChange
	// borderRunes maps a window-space border cell (row,col) to its box-drawing
	// glyph for pane_borders="joined": junctions (┼├┤┬┴) computed from the divider
	// segments, so crossings connect instead of overwriting. Rebuilt on layout/
	// config change; empty (nil) for "simple" mode.
	borderRunes map[[2]int]rune
}

// rebuildBorders recomputes the joined-mode junction glyphs from the current
// layout's divider segments. A cell's glyph is chosen from which of its four
// neighbors also carry a border stroke (vertical above/below, horizontal
// left/right). No-op (clears the map) unless pane_borders is joined/framed.
func (c *compositor) rebuildBorders() {
	c.borderRunes = nil
	if c.layout == nil || (c.cfg.PaneBorders != "joined" && c.cfg.PaneBorders != "framed") {
		return
	}
	vset := map[[2]int]bool{} // cells carrying a vertical stroke
	hset := map[[2]int]bool{} // cells carrying a horizontal stroke
	for _, b := range c.layout.Borders {
		if b.Vertical {
			for r := b.Start; r < b.End; r++ {
				vset[[2]int{r, b.Fixed}] = true
			}
		} else {
			for col := b.Start; col < b.End; col++ {
				hset[[2]int{b.Fixed, col}] = true
			}
		}
	}
	// framed: add the outer frame as border segments in content space (top row -1,
	// bottom row H, left col -1, right col W), so interior dividers reaching an edge
	// form a proper ┬/┴/├/┤ into the frame instead of a plain corner.
	W, H := c.layout.Cols, c.layout.Rows
	if c.frameInset() > 0 {
		for col := -1; col <= W; col++ {
			hset[[2]int{-1, col}] = true
			hset[[2]int{H, col}] = true
		}
		for r := -1; r <= H; r++ {
			vset[[2]int{r, -1}] = true
			vset[[2]int{r, W}] = true
		}
	}
	runes := make(map[[2]int]rune, len(vset)+len(hset))
	mark := func(cell [2]int) {
		r, col := cell[0], cell[1]
		up := vset[[2]int{r - 1, col}]
		down := vset[[2]int{r + 1, col}]
		left := hset[[2]int{r, col - 1}]
		right := hset[[2]int{r, col + 1}]
		runes[cell] = boxRune(up, down, left, right)
	}
	for cell := range vset {
		mark(cell)
	}
	for cell := range hset {
		mark(cell)
	}
	if c.frameInset() > 0 && c.cfg.PaneBorderRounded {
		runes[[2]int{-1, -1}] = '╭'
		runes[[2]int{-1, W}] = '╮'
		runes[[2]int{H, -1}] = '╰'
		runes[[2]int{H, W}] = '╯'
	}
	c.borderRunes = runes
}

// boxRune picks the box-drawing glyph for a border cell from which of its four
// sides connect to another border cell.
func boxRune(up, down, left, right bool) rune {
	switch {
	case up && down && left && right:
		return '┼'
	case up && down && right:
		return '├'
	case up && down && left:
		return '┤'
	case down && left && right:
		return '┬'
	case up && left && right:
		return '┴'
	case down && right:
		return '┌'
	case down && left:
		return '┐'
	case up && right:
		return '└'
	case up && left:
		return '┘'
	case up || down:
		return '│'
	default:
		return '─'
	}
}

// fillGlyph is the dot painted in the area a larger client has beyond the
// window it's displaying — the empty space around a smaller session's grid.
// Its color is the client's configurable fill_fg (default dim grey).
func (c *compositor) fillGlyph() emu.Glyph {
	return emu.Glyph{Char: '·', FG: c.cfg.FillFG, BG: emu.DefaultBG}
}

func newCompositor() *compositor {
	return &compositor{panes: map[int]map[int]emu.Line{}, meta: map[int]paneMeta{}, cfg: config.DefaultClientConfig(), dragBorder: -1}
}

// setPhysical records this client's real terminal size, used to clip/pad the
// window it displays. Called on attach and on SIGWINCH.
func (c *compositor) setPhysical(cols, rows int) {
	c.phyCols, c.phyRows = cols, rows
}

// cols is the physical width the compositor draws to — the client's own
// terminal, falling back to the window width when the physical size isn't
// known yet (e.g. in tests).
func (c *compositor) cols() int {
	if c.phyCols > 0 {
		return c.phyCols
	}
	if c.layout == nil {
		return 0
	}
	return c.layout.Cols
}

// statusLines is how many rows the status bar occupies (tmux `status` 1..5),
// clamped to a sane range.
func (c *compositor) statusLines() int {
	if c.cfg.StatusLines < 0 {
		return 0 // status off: bar hidden, window gets the full height
	}
	if c.cfg.StatusLines > 5 {
		return 5
	}
	return c.cfg.StatusLines
}

// totalRows is the physical height the compositor draws to (content rows plus
// the status rows), falling back to the window height + status when the
// physical size isn't known yet.
func (c *compositor) totalRows() int {
	if c.phyRows > 0 {
		return c.phyRows
	}
	if c.layout == nil {
		return 0
	}
	return c.layout.Rows + c.statusLines()
}

// statusTopRows/statusBottomRows are the status bar's rows on each edge (all of
// statusLines on whichever edge status-position picks, 0 on the other).
func (c *compositor) statusTopRows() int {
	if c.cfg.StatusPosition == "top" {
		return c.statusLines()
	}
	return 0
}

func (c *compositor) statusBottomRows() int {
	if c.cfg.StatusPosition != "top" {
		return c.statusLines()
	}
	return 0
}

// topDockRows/bottomDockRows are the rows reserved by top/bottom docked widgets,
// stacked just inward of the status bar (status keeps the screen edge).
func (c *compositor) topDockRows() int {
	n := 0
	for _, d := range c.docks {
		if d.dock == "top" {
			n += d.size
		}
	}
	return n
}

func (c *compositor) bottomDockRows() int {
	n := 0
	for _, d := range c.docks {
		if d.dock == "bottom" {
			n += d.size
		}
	}
	return n
}

// frameInset is 1 when pane_borders="framed": an outer frame reserved on every
// window edge (just inside any docks/status), so the pane content shrinks by it
// on all four sides and the compositor draws the enclosing box in the reserve.
func (c *compositor) frameInset() int {
	if c.cfg.PaneBorders == "framed" {
		return 1
	}
	return 0
}

// bottomReserve is the rows the compositor holds below the window content:
// bottom docks, a bottom status bar, and the framed outer border.
func (c *compositor) bottomReserve() int {
	return c.statusBottomRows() + c.bottomDockRows() + c.frameInset()
}

// contentOffset is the number of physical rows above the window content: a top
// status bar, any top docks, and the framed outer border. Window row R draws at
// physical row R+contentOffset.
func (c *compositor) contentOffset() int {
	return c.statusTopRows() + c.topDockRows() + c.frameInset()
}

// topBottomDockRow returns the top/bottom dock occupying physical row `row` and
// the row's index within that dock, or nil. Top docks stack downward from just
// inside a top status bar; bottom docks stack upward from just inside a bottom
// status bar.
func (c *compositor) topBottomDockRow(row int) (*textBox, int) {
	start := c.statusTopRows()
	for _, d := range c.docks {
		if d.dock == "top" {
			if row >= start && row < start+d.size {
				return d, row - start
			}
			start += d.size
		}
	}
	start = c.totalRows() - c.bottomReserve()
	for _, d := range c.docks {
		if d.dock == "bottom" {
			if row >= start && row < start+d.size {
				return d, row - start
			}
			start += d.size
		}
	}
	return nil, 0
}

// optionValue reads a client option by name for gtmux.get_option(). Covers the
// options a widget is likely to want; unknown names return "". ponytail: a small
// switch, not reflection — extend as widgets need more.
func (c *compositor) optionValue(name string) string {
	switch name {
	case "status_interval":
		return strconv.Itoa(c.cfg.StatusInterval)
	case "status_left":
		return c.cfg.StatusLeft
	case "status_right":
		return c.cfg.StatusRight
	case "status_position":
		return c.cfg.StatusPosition
	case "mouse":
		if c.cfg.Mouse {
			return "on"
		}
		return "off"
	}
	return ""
}

// clickWidget maps a mouse position to the docked/float widget under it that has
// an on_click handler, returning the widget, the clicked line's index within it,
// that line's text, and the column within the line. nil if nothing clickable is
// there. Column bands mirror composeContentRow's left/right stacking exactly.
func (c *compositor) clickWidget(me proto.MouseEvent) (*textBox, *lua.LFunction, int, string, int) {
	row, col := me.Y-1, me.X-1
	// Resolve the fn to run: a component region under the point (widget-local
	// x=col, y=row) wins; otherwise the widget's flat onClick. Nil = no handler.
	hit := func(b *textBox, i, cc int) (*textBox, *lua.LFunction, int, string, int) {
		if fn := b.regionAt(cc, i); fn != nil {
			return b, fn, i, b.lineText(i), cc
		}
		if b.onClick == nil {
			return nil, nil, 0, "", 0
		}
		return b, b.onClick, i, b.lineText(i), cc
	}
	// Status rows: a mounted status component owns them (window-list clicks etc.
	// route through its regions instead of resolveMouse/windowHits).
	if c.statusWidget != nil {
		if is, extra := c.statusRowKind(row); is {
			crow := 0
			if extra >= 0 {
				crow = extra + 1
			}
			return hit(c.statusWidget, crow, col)
		}
	}
	// Top/bottom dock strips (full width).
	if d, lr := c.topBottomDockRow(row); d != nil {
		return hit(d, lr, col)
	}
	winRow := row - c.contentOffset()
	contentH := c.totalRows() - c.contentOffset() - c.bottomReserve()
	if winRow >= 0 && winRow < contentH {
		x := 0 // left docks fill [0, leftInset) in c.docks order
		for _, d := range c.docks {
			if d.dock == "left" {
				if col >= x && col < x+d.size {
					return hit(d, winRow, col-x)
				}
				x += d.size
			}
		}
		rx := c.cols() - c.rightInset() // right docks fill [cols-rightInset, cols)
		for _, d := range c.docks {
			if d.dock == "right" {
				if col >= rx && col < rx+d.size {
					return hit(d, winRow, col-rx)
				}
				rx += d.size
			}
		}
	}
	// Float overlays, topmost first.
	for i := len(c.overlays) - 1; i >= 0; i-- {
		b, ok := c.overlays[i].(*textBox)
		if !ok || (b.onClick == nil && len(b.regions) == 0) {
			continue
		}
		li, cc := winRow-b.row, col-b.col
		w := b.w
		if b.canvas == nil {
			w = len([]rune(b.lineText(li)))
		}
		if li >= 0 && li < b.rowCount() && cc >= 0 && cc < w {
			return hit(b, li, cc)
		}
	}
	return nil, nil, 0, "", 0
}

// leftInset/rightInset are the columns reserved by docked widgets on each edge.
// contentColOffset is the horizontal mirror of contentOffset: window column C
// draws at physical column C+contentColOffset. contentCols is the window content
// width (physical minus both docks). With no docks all three are the trivial
// values and every path reduces byte-identically to the pre-dock code.
func (c *compositor) leftInset() int {
	n := 0
	for _, d := range c.docks {
		if d.dock == "left" {
			n += d.size
		}
	}
	return n
}

func (c *compositor) rightInset() int {
	n := 0
	for _, d := range c.docks {
		if d.dock == "right" {
			n += d.size
		}
	}
	return n
}

func (c *compositor) contentColOffset() int { return c.leftInset() + c.frameInset() }

func (c *compositor) contentCols() int {
	if w := c.cols() - c.leftInset() - c.rightInset() - 2*c.frameInset(); w > 0 {
		return w
	}
	return 1
}

// statusRow is the physical row of the MAIN bar (status line 1: left + window
// list + right). Extra lines stack inward from it (see statusRowKind).
// openModal opens a modal keyboard widget from a gtmux.open{...} request: build
// the component textBox, render it once (which also creates its state table), and
// store it. A fresh modal each open — state starts empty (selection at 0).
func (c *compositor) openModal(m *config.ModalOpen, binds *config.ClientBinds) {
	c.modalPos = m.Position
	if c.modalPos == "" {
		c.modalPos = "center"
	}
	b := &textBox{
		component: m.Component, onKey: m.OnKey, binds: binds,
		w: m.Width, h: m.Height,
		fg: c.cfg.StatusFG, bg: c.cfg.StatusBG,
	}
	switch c.modalPos {
	case "status": // a one-line prompt on the status/message row
		b.w, b.h = c.cols(), 1
	case "full": // cover the whole content area (e.g. a lock screen)
		b.w, b.h = c.cols(), c.totalRows()-c.contentOffset()-c.bottomReserve()
	}
	b.rerender() // paints its canvas + creates the persistent state table
	c.modal = b
}

// modalKey feeds one key name to the open modal's on_key and reports whether it
// asked to close. The handler shares the modal's state table (RunKey mutates it
// in place), so a following rerender reflects the change.
func (c *compositor) modalKey(key string) ([]config.BindOp, bool) {
	if c.modal == nil {
		return nil, false
	}
	return c.modal.binds.RunKey(c.modal.onKey, key, c.modal.state)
}

// modalOffset centers the modal box in the content area (over panes, inside the
// status/dock reserves).
func (c *compositor) modalOffset() (ox, oy int) {
	b := c.modal
	if c.modalPos == "full" {
		return 0, c.contentOffset()
	}
	ox = (c.cols() - b.w) / 2
	contentH := c.totalRows() - c.contentOffset() - c.bottomReserve()
	oy = c.contentOffset() + (contentH-b.h)/2
	if ox < 0 {
		ox = 0
	}
	if oy < 0 {
		oy = 0
	}
	return ox, oy
}

// modalRow blits the centered modal's canvas slice for one physical row.
func (c *compositor) modalRow(row int, line emu.Line) {
	if c.modal == nil || c.modal.canvas == nil || c.layout == nil || c.modalPos == "status" {
		return // status-position modal is drawn on the status row, not centered
	}
	ox, oy := c.modalOffset()
	cr := row - oy
	if cr < 0 || cr >= c.modal.canvas.H {
		return
	}
	for x := 0; x < c.modal.canvas.W; x++ {
		col := ox + x
		if col < 0 || col >= len(line) {
			continue
		}
		if g, ok := c.modal.canvas.At(x, cr); ok {
			line[col] = g
		}
	}
}

// statusModalLine blits a status-position modal's single row (a prompt/input
// widget) full-width onto the status row, overriding the status component.
func (c *compositor) statusModalLine() emu.Line {
	b := c.modal
	line := make(emu.Line, c.cols())
	for x := range line {
		if b.canvas != nil {
			if g, ok := b.canvas.At(x, 0); ok {
				line[x] = g
				continue
			}
		}
		line[x] = emu.Glyph{Char: ' ', FG: b.fg, BG: b.bg, Mode: b.attr}
	}
	return line
}

// statusRowLine blits the status component's canvas row for a physical status
// row. Canvas row 0 is the main bar (the screen edge); rows 1.. are the extra
// lines stacking toward content, matching statusRowKind's extra (-1 = main).
func (c *compositor) statusRowLine(row, extra int) emu.Line {
	crow := 0
	if extra >= 0 {
		crow = extra + 1
	}
	b := c.statusWidget
	line := make(emu.Line, c.cols())
	for x := range line {
		if b.canvas != nil {
			if g, ok := b.canvas.At(x, crow); ok {
				line[x] = g
				continue
			}
		}
		line[x] = emu.Glyph{Char: ' ', FG: b.fg, BG: b.bg, Mode: b.attr}
	}
	return line
}

func (c *compositor) statusRow() int {
	if c.cfg.StatusPosition == "top" {
		return 0
	}
	return c.totalRows() - 1
}

// statusRowKind classifies a physical row within the status block: isStatus is
// true for any status row; extra is -1 for the main bar and 0-based into
// ExtraStatusFormats (line 2 = 0) for the additional lines. The main bar sits at
// the screen edge (tmux status-format[0]); extra lines stack toward the content.
func (c *compositor) statusRowKind(row int) (isStatus bool, extra int) {
	sl := c.statusLines()
	if c.cfg.StatusPosition == "top" {
		if row < 0 || row >= sl {
			return false, 0
		}
		if row == 0 {
			return true, -1
		}
		return true, row - 1
	}
	tr := c.totalRows()
	if row < tr-sl || row >= tr {
		return false, 0
	}
	if row == tr-1 {
		return true, -1
	}
	return true, tr - 2 - row
}

func (c *compositor) rectFor(paneID int) (proto.PaneRect, bool) {
	for _, pr := range c.layout.Panes {
		if pr.ID == paneID {
			return pr, true
		}
	}
	return proto.PaneRect{}, false
}

// activeCursor reports where the real terminal cursor should sit: the
// active pane's rect plus its last-known local cursor position — or the
// copy-mode block position while this client is browsing scrollback.
func (c *compositor) activeCursor() (row, col int, visible bool) {
	if c.layout == nil {
		return 0, 0, false
	}
	// Cursor rows are computed in window-row space, then shifted to physical by
	// contentOffset (status-at-top pushes content down). A row is visible only
	// if it lands inside the window content, not the status/dot-fill slack.
	off := c.contentOffset()
	coff := c.contentColOffset() // window column C sits at physical col C+coff (left dock)
	inContent := func(rWin int) bool { return rWin >= 0 && rWin < c.layout.Rows }
	// A popup grabs the cursor: place it inside the box at the popup's cursor.
	if c.popup != nil {
		if sr, sc, ok := c.popupBounds(); ok {
			r, col := sr+c.popup.cursor.R, sc+c.popup.cursor.C+coff
			return r + off, col, c.popup.cursorVisible && inContent(r) && col >= 0 && col < c.cols()
		}
	}
	if c.copy != nil {
		if pr, ok := c.rectFor(c.copy.paneID); ok {
			r := pr.Row + (c.copy.cy - c.copy.top)
			col := pr.Col + c.copy.cx + coff
			return r + off, col, inContent(r) && col >= 0 && col < c.cols()
		}
	}
	for _, pr := range c.layout.Panes {
		if !pr.Active {
			continue
		}
		m := c.meta[pr.ID]
		r, col := pr.Row+m.cursor.R, pr.Col+m.cursor.C+coff
		visible := m.visible && inContent(r) && col >= 0 && col < c.cols()
		return r + off, col, visible
	}
	return 0, 0, false
}

// markAll marks every physical row dirty (whole-screen repaint).
func (c *compositor) markAll(dirty map[int]bool) {
	for row := 0; row < c.totalRows(); row++ {
		dirty[row] = true
	}
}

// apply folds one ServerMsg into the compositor's state and returns the
// ANSI bytes needed to bring the real terminal up to date.
// programChange is a pane whose foreground command changed between snapshots,
// fed to gtmux.on("program-changed").
type programChange struct {
	session       string
	window, pane  int
	command, from string
}

// detectProgramChanges queues a programChange for every pane whose foreground
// command changed since the last snapshot. The first snapshot only primes the
// map (progSeeded) so nothing fires for commands already running at attach.
func (c *compositor) detectProgramChanges(snap *proto.StateSnapshot) {
	if c.prevCommand == nil {
		c.prevCommand = map[int]string{}
	}
	seen := map[int]bool{}
	for _, s := range snap.Sessions {
		for _, w := range s.Windows {
			for _, p := range w.Panes {
				seen[p.ID] = true
				from, had := c.prevCommand[p.ID]
				c.prevCommand[p.ID] = p.Command
				if c.progSeeded && had && from != p.Command {
					c.pendingProgram = append(c.pendingProgram, programChange{
						session: s.Name, window: w.Index, pane: p.ID,
						command: p.Command, from: from,
					})
				}
			}
		}
	}
	for id := range c.prevCommand {
		if !seen[id] {
			delete(c.prevCommand, id)
		}
	}
	c.progSeeded = true
}

// drainProgramChanges returns and clears the queued program changes.
func (c *compositor) drainProgramChanges() []programChange {
	if len(c.pendingProgram) == 0 {
		return nil
	}
	out := c.pendingProgram
	c.pendingProgram = nil
	return out
}

// detectAlerts queues an AlertEvent for every window whose bell/activity/
// silence flag rose since the last snapshot. The first snapshot only primes
// the map (alertSeeded) so alerts already standing at attach don't fire.
func (c *compositor) detectAlerts(snap *proto.StateSnapshot) {
	if c.alertPrev == nil {
		c.alertPrev = map[string][3]bool{}
	}
	seen := map[string]bool{}
	for _, s := range snap.Sessions {
		for _, w := range s.Windows {
			key := s.Name + "\x00" + strconv.Itoa(w.Index)
			seen[key] = true
			cur := [3]bool{w.Bell, w.Activity, w.Silence}
			prev := c.alertPrev[key]
			c.alertPrev[key] = cur
			if !c.alertSeeded {
				continue
			}
			cmd, title := "", ""
			for _, p := range w.Panes {
				if p.Active {
					cmd, title = p.Command, p.Title
					break
				}
			}
			names := [3]string{"alert-bell", "alert-activity", "alert-silence"}
			for i, name := range names {
				if cur[i] && !prev[i] {
					c.pendingAlerts = append(c.pendingAlerts, config.AlertEvent{
						Event: name, Session: s.Name, Window: w.Index,
						Name: w.Name, Command: cmd, Title: title,
					})
				}
			}
		}
	}
	// Drop windows that vanished so a reused index doesn't inherit stale flags.
	for key := range c.alertPrev {
		if !seen[key] {
			delete(c.alertPrev, key)
		}
	}
	c.alertSeeded = true
}

// setPaneBorder overrides pane id's border color (pane:set_border). An empty
// or unknown color clears the override. Returns the bytes to repaint the
// affected border rows. Caller holds compMu.
func (c *compositor) setPaneBorder(id int, color string) []byte {
	if c.paneBorderColor == nil {
		c.paneBorderColor = map[int]emu.Color{}
	}
	if col, ok := config.ColorByName(color); ok {
		c.paneBorderColor[id] = col
	} else {
		delete(c.paneBorderColor, id)
	}
	return c.redraw()
}

// drainAlerts returns and clears the queued alert edges. Called by the client
// after apply, so gtmux.on callbacks run outside compMu.
func (c *compositor) drainAlerts() []config.AlertEvent {
	if len(c.pendingAlerts) == 0 {
		return nil
	}
	out := c.pendingAlerts
	c.pendingAlerts = nil
	return out
}

func (c *compositor) apply(msg *proto.ServerMsg) []byte {
	dirty := map[int]bool{}
	var titleOut []byte // set-titles OSC, prepended to the emitted diff below
	var kittyOut []byte // extended-keys negotiation with the outer terminal

	if msg.Layout != nil {
		c.layout = msg.Layout
		// Focusing a pane clears any border override on it (pane:set_border):
		// visiting the flagged pane acknowledges it, like the bell ! flag.
		for _, pr := range msg.Layout.Panes {
			if pr.Active && c.paneBorderColor != nil {
				delete(c.paneBorderColor, pr.ID)
			}
		}
		c.rebuildBorders() // recompute joined-mode junctions for the new arrangement
		// A new arrangement can change the dot-filled slack too, so redraw
		// every physical row, not just the window's.
		c.markAll(dirty)
		// extended-keys: match the outer terminal to the new active pane's kitty
		// state (the pane may have changed, or its app toggled the protocol).
		kittyOut = c.negotiateKitty()
	}

	for _, pc := range msg.PaneContent {
		buf, ok := c.panes[pc.PaneID]
		if !ok {
			buf = map[int]emu.Line{}
			c.panes[pc.PaneID] = buf
		}
		for localRow, line := range pc.Lines {
			buf[localRow] = line
		}
		c.meta[pc.PaneID] = paneMeta{cursor: pc.Cursor, visible: pc.CursorVisible}

		if c.layout == nil {
			continue
		}
		if pr, ok := c.rectFor(pc.PaneID); ok {
			for localRow := range pc.Lines {
				dirty[pr.Row+localRow] = true
			}
			// The cursor/selection highlight can move even over rows that
			// didn't get new Lines (rare in practice, since copy-mode
			// resends every visible row on each keystroke) — mark it too.
			dirty[pr.Row+pc.Cursor.R] = true
		}
	}

	if msg.CopyModeEnter != nil {
		rows := 1
		if c.layout != nil {
			if pr, ok := c.rectFor(msg.CopyModeEnter.PaneID); ok {
				rows = pr.Rows
			}
		}
		c.copy = newCopyMode(msg.CopyModeEnter, rows, c.cfg.ModeKeys == "emacs", c.cfg.WordSeparators)
		c.markAll(dirty)
	}

	if msg.Status != nil {
		c.status = msg.Status
		if msg.Status.Snapshot != nil {
			c.snapshot = msg.Status.Snapshot
			c.detectAlerts(msg.Status.Snapshot)
			c.detectProgramChanges(msg.Status.Snapshot)
		}
		if c.expander == nil {
			iv := c.cfg.StatusInterval
			if iv <= 0 {
				iv = 15
			}
			c.expander = newStatusExpander(time.Duration(iv) * time.Second)
		}
		// Format-driven widgets re-expand on the same tick; mark their rows (old
		// and new span, in case the line count changed) dirty so the repaint shows
		// the new content and clears any vacated rows.
		off := c.contentOffset()
		markWidget := func(start, count int) {
			for i := 0; i < count; i++ {
				dirty[start+i+off] = true
			}
		}
		for _, w := range c.overlays {
			fw, ok := w.(formatWidget)
			if !ok {
				continue
			}
			markWidget(fw.dirtyRows())
			fw.reexpand(c.expander, msg.Status.Vars, msg.Status.ServerShell)
			markWidget(fw.dirtyRows())
		}
		// Docked widgets re-expand too. Left/right span the content height; mark
		// those rows. Top/bottom own their reserve strips; mark those.
		if len(c.docks) > 0 && c.layout != nil {
			for _, d := range c.docks {
				// A draw widget needs its region size to build the canvas: left/right
				// docks are size × content-height, top/bottom are full-width × size.
				if d.dock == "top" || d.dock == "bottom" {
					d.w, d.h = c.cols(), d.size
				} else {
					// Left/right docks span the whole strip, INCLUDING the two rows
					// the framed outer border occupies — the frame wraps the window's
					// panes, not the client chrome beside them. Sizing the dock to
					// layout.Rows alone left its box two rows short of the frame, so
					// its bottom border sat one row high (visibly misaligned).
					d.w, d.h = d.size, c.layout.Rows+2*c.frameInset()
				}
				d.reexpand(c.expander, msg.Status.Vars, msg.Status.ServerShell)
			}
			for r := 0; r < c.layout.Rows; r++ {
				dirty[r+off] = true
			}
			for r := c.statusTopRows(); r < c.contentOffset(); r++ {
				dirty[r] = true
			}
			for r := c.totalRows() - c.bottomReserve(); r < c.totalRows()-c.statusBottomRows(); r++ {
				dirty[r] = true
			}
		}
		// The status component (if mounted) re-renders like a dock, sized
		// full-width × statusLines; its rows are marked dirty just below.
		if c.statusWidget != nil && c.layout != nil {
			c.statusWidget.w, c.statusWidget.h = c.cols(), c.statusLines()
			c.statusWidget.reexpand(c.expander, msg.Status.Vars, msg.Status.ServerShell)
		}
		// An open modal re-renders on the tick too, so live data (e.g. a picker's
		// cross-session list) reflects fresh snapshots while it's open, not just on
		// keypress. Mark its centered band dirty.
		if c.modal != nil && c.layout != nil {
			switch c.modalPos {
			case "status":
				c.modal.w = c.cols() // full width; status rows are marked dirty below
			case "full":
				c.modal.w = c.cols()
				c.modal.h = c.totalRows() - c.contentOffset() - c.bottomReserve()
			}
			c.modal.rerender()
			if c.modalPos != "status" {
				_, oy := c.modalOffset()
				for r := oy; r < oy+c.modal.h && r < c.totalRows(); r++ {
					if r >= 0 {
						dirty[r] = true
					}
				}
			}
		}
		// Repaint the whole status block (every status line re-expands per tick).
		for r := 0; r < c.totalRows(); r++ {
			if is, _ := c.statusRowKind(r); is {
				dirty[r] = true
			}
		}

		// set-titles: push the outer terminal's title to its title stack once,
		// then emit OSC 0/2 whenever the expanded string changes (restoreTitle
		// pops it on detach).
		if c.cfg.SetTitles {
			if t := c.expander.expand(c.cfg.SetTitlesString, msg.Status.Vars, msg.Status.ServerShell); t != c.lastTitle {
				c.lastTitle = t
				if !c.titlePushed {
					titleOut = append(titleOut, "\x1b[22;2t"...)
					c.titlePushed = true
				}
				titleOut = append(titleOut, "\x1b]0;"...)
				titleOut = append(titleOut, t...)
				titleOut = append(titleOut, '\a')
			}
		}
	}

	if msg.OpenPicker != nil {
		c.picker = newPicker(msg.OpenPicker)
		c.markAll(dirty)
	}

	if msg.Popup != nil {
		c.applyPopup(msg.Popup)
		c.markAll(dirty)
	}

	return append(append(kittyOut, titleOut...), c.emit(dirty)...)
}

// restoreTitle pops the outer terminal's saved title off its title stack, if
// set-titles pushed one — emitted once on detach.
func (c *compositor) restoreTitle() []byte {
	if c.titlePushed {
		return []byte("\x1b[23;2t")
	}
	return nil
}

// redraw repaints every physical row — used when only the client's own
// terminal size changed (SIGWINCH), which the server may not answer if this
// isn't the acting client, so the clip/dot-fill has to be recomputed locally.
func (c *compositor) redraw() []byte {
	dirty := map[int]bool{}
	c.markAll(dirty)
	return c.emit(dirty)
}

// emit turns the given dirty rows into ANSI bytes and repositions the cursor.
//
// The frame is bracketed twice over: DECSET 2026 (synchronized update) so a
// terminal that supports it swaps the whole frame at once, and ?25l/?25h so
// one that doesn't still never shows the cursor tracking the write head
// mid-paint. Without the latter a large repaint (a vim scroll dirties the
// whole scroll region) flickers a stray cursor across the rows being drawn.
func (c *compositor) emit(dirty map[int]bool) []byte {
	var b strings.Builder
	b.WriteString("\x1b[?2026h\x1b[?25l")

	for row := range dirty {
		if row < 0 || row >= c.totalRows() {
			continue
		}
		fmt.Fprintf(&b, "\x1b[%d;1H\x1b[2K", row+1)
		emu.WriteLine(&b, c.buildRow(row))
	}

	if row, col, visible := c.activeCursor(); visible {
		fmt.Fprintf(&b, "\x1b[%d;%dH\x1b[?25h", row+1, col+1)
	}

	b.WriteString("\x1b[?2026l")
	return []byte(b.String())
}

// buildRow composites one physical row: the status bar, or borders + pane
// content + the display-panes number overlay for a window row.
func (c *compositor) buildRow(row int) emu.Line {
	// status off: no reserved row, but the command prompt / copy-mode help still
	// needs somewhere to show — overlay it on the bottom physical row (like tmux).
	if c.statusLines() == 0 && c.layout != nil && row == c.totalRows()-1 {
		if c.copy != nil {
			return renderPromptLine(c.cols(), "copy-mode", c.copy.helpText(), c.cfg)
		}
		if c.prompt != nil {
			return renderPromptLine(c.cols(), c.prompt.label(), string(c.prompt.buf), c.cfg)
		}
		if c.status != nil && c.status.PromptLabel != "" {
			return renderPromptLine(c.cols(), c.status.PromptLabel, c.status.PromptText, c.cfg)
		}
	}
	if isStatus, extra := c.statusRowKind(row); isStatus {
		// Main bar row: client-owned input modes draw their own line, taking
		// precedence over status content (component or bespoke bar alike).
		if extra < 0 {
			if c.copy != nil {
				return renderPromptLine(c.cols(), "copy-mode", c.copy.helpText(), c.cfg)
			}
			if c.prompt != nil {
				return renderPromptLine(c.cols(), c.prompt.label(), string(c.prompt.buf), c.cfg)
			}
			if c.status != nil && c.status.PromptLabel != "" {
				return renderPromptLine(c.cols(), c.status.PromptLabel, c.status.PromptText, c.cfg)
			}
			// A status-position modal (a prompt/input widget) overrides the status
			// content on the main bar row, above the status component.
			if c.modal != nil && c.modalPos == "status" && c.modal.canvas != nil {
				return c.statusModalLine()
			}
		}
		// Status content is a component (dock="status", the default config ships
		// one). No component configured => a blank styled bar.
		if c.statusWidget != nil {
			return c.statusRowLine(row, extra)
		}
		return make(emu.Line, c.cols())
	}

	// Top/bottom docked widgets own full-width rows just inside the status bar.
	if d, lr := c.topBottomDockRow(row); d != nil {
		line := make(emu.Line, c.cols())
		d.paintStrip(lr, 0, c.cols(), line)
		return line
	}

	// framed: the top and bottom outer-frame rows (just inside the docks/status).
	if c.frameInset() > 0 && c.layout != nil {
		if row == c.contentOffset()-1 {
			return c.buildFrameRow(true)
		}
		if row == c.totalRows()-c.bottomReserve() {
			return c.buildFrameRow(false)
		}
	}

	// Below the status/dock branches, work in window-row space: the window
	// content is shifted down by contentOffset physical rows.
	row -= c.contentOffset()

	// Cells inside the window rectangle start as window background; cells
	// outside it (a larger client's slack) are dot-filled. The canvas is the
	// window content width (physical minus docks); composeContentRow wraps it
	// into the full physical row with the dock strips at the end.
	line := make(emu.Line, c.contentCols())
	inWindowRow := c.layout != nil && row >= 0 && row < c.layout.Rows
	for i := range line {
		if c.layout != nil && !(inWindowRow && i < c.layout.Cols) {
			line[i] = c.fillGlyph()
		} else {
			line[i] = emu.EmptyGlyph()
		}
	}
	if c.layout == nil {
		return c.composeContentRow(line, row)
	}

	// Border color is decided per cell, not per divider: a cell lights active
	// (or marked) only where it lies on that pane's outline ring, so a long
	// divider shared by several stacked panes highlights just the active pane's
	// own edge instead of its full length.
	// Inactive dividers use pane-border-style (fg/bg/attr); the active/marked
	// pane's own ring cells override with their fg-only color.
	// twoPanes: tmux's pane-border-indicators=colour lights only HALF the shared
	// divider when a window has exactly two panes, so you can tell which is
	// active (a full-length divider is ambiguous between the two). See
	// onActiveBorder.
	borderGlyph := func(char rune, col int) emu.Glyph {
		fg, bg, mode := c.borderStyleAt(col, row)
		return emu.Glyph{Char: char, FG: fg, BG: bg, Mode: mode}
	}
	joined := c.borderRunes != nil // joined/framed: use precomputed junction glyphs
	for _, bd := range c.layout.Borders {
		if bd.Vertical {
			if row >= bd.Start && row < bd.End && bd.Fixed >= 0 && bd.Fixed < c.contentCols() {
				ch := '│'
				if joined {
					if r, ok := c.borderRunes[[2]int{row, bd.Fixed}]; ok {
						ch = r
					}
				}
				line[bd.Fixed] = borderGlyph(ch, bd.Fixed)
			}
		} else if row == bd.Fixed {
			for col := bd.Start; col < bd.End && col < c.contentCols(); col++ {
				ch := '─'
				if joined {
					if r, ok := c.borderRunes[[2]int{row, col}]; ok {
						ch = r
					}
				}
				line[col] = borderGlyph(ch, col)
			}
		}
	}

	for _, pr := range c.layout.Panes {
		if row < pr.Row || row >= pr.Row+pr.Rows {
			continue
		}
		localRow := row - pr.Row
		// This client's copy-mode pane renders from the frozen snapshot, with
		// the cursor block and selection highlighted locally.
		if c.copy != nil && pr.ID == c.copy.paneID {
			c.buildCopyRow(pr, localRow, line)
			continue
		}
		buf := c.panes[pr.ID]
		for x := 0; x < pr.Cols; x++ {
			col := pr.Col + x
			if col < 0 || col >= c.contentCols() {
				continue
			}
			g := emu.EmptyGlyph()
			if l, ok := buf[localRow]; ok && x < len(l) {
				g = l[x]
			}
			line[col] = g
		}
	}

	// pane-border-status: each pane reserved a row (BorderRow) for its label;
	// draw the border rule across the pane and overlay the expanded label.
	for _, pr := range c.layout.Panes {
		if pr.BorderRow != row || pr.BorderLabel == "" {
			continue
		}
		fg, bg, mode := c.cfg.InactiveBorderFG, c.cfg.InactiveBorderBG, c.cfg.InactiveBorderAttr
		if pr.Active {
			fg, bg, mode = c.cfg.ActiveBorderFG, emu.DefaultBG, int16(0)
		}
		for col := pr.Col; col < pr.Col+pr.Cols && col < c.contentCols(); col++ {
			line[col] = emu.Glyph{Char: '─', FG: fg, BG: bg, Mode: mode}
		}
		col := pr.Col + 1
		for _, r := range pr.BorderLabel {
			if col >= pr.Col+pr.Cols || col >= c.contentCols() {
				break
			}
			line[col] = emu.Glyph{Char: r, FG: fg, BG: bg, Mode: mode}
			col++
		}
	}

	if c.layout.ShowNumbers {
		for _, pr := range c.layout.Panes {
			if row != pr.Row+pr.Rows/2 {
				continue
			}
			label := fmt.Sprintf(" %d ", pr.Number)
			startCol := pr.Col + (pr.Cols-len(label))/2
			for j, r := range label {
				col := startCol + j
				if col < pr.Col || col >= pr.Col+pr.Cols {
					continue
				}
				line[col] = emu.Glyph{Char: r, FG: emu.Black, BG: emu.Yellow, Mode: emu.AttrBold}
			}
		}
	}

	for _, w := range c.overlays {
		w.paintRow(row, c.contentCols(), line)
	}
	if c.picker != nil {
		c.overlayRow(row, line)
	}
	if c.popup != nil {
		c.popupRow(row, line)
	}
	if c.modal != nil {
		c.modalRow(row, line)
	}
	// clock-mode / lock: over the window's middle. Lock is a single centered
	// line; clock is tmux-style big ASCII digits (5 rows) centered vertically.
	if c.clock || c.locked {
		mid := c.layout.Rows / 2
		drawn := false
		if c.locked {
			msg := "-- locked (press any key) --"
			if c.cfg.LockPassword != "" {
				msg = "-- locked (type password, Enter) --"
			}
			if row == mid {
				c.drawCentered(msg, line)
				drawn = true
			}
		} else {
			big := bigClock(c.clockText())
			top := mid - len(big)/2
			if row >= top && row < top+len(big) {
				c.drawCentered(big[row-top], line)
				drawn = true
			}
		}
		if !drawn {
			for i := range line {
				if i < c.layout.Cols {
					line[i] = emu.EmptyGlyph()
				}
			}
		}
	}

	return c.composeContentRow(line, row)
}

// composeContentRow wraps a content-width row (window content drawn in
// window-column space) into a full physical row, painting the docked column
// strips on the left/right edges. winRow selects each dock's own line. With no
// docks it returns content unchanged — the byte-identical pre-dock path.
func (c *compositor) composeContentRow(content emu.Line, winRow int) emu.Line {
	left, right, frame := c.leftInset(), c.rightInset(), c.frameInset()
	if left == 0 && right == 0 && frame == 0 {
		return content
	}
	phys := make(emu.Line, c.cols())
	// Dock rows are offset by the frame inset: dock row 0 is the frame's TOP row,
	// so the dock's own box lines up with the framed border (see the sizing in
	// apply). With no frame this is a no-op.
	dockRow := winRow + frame
	colStart := 0
	for _, d := range c.docks {
		if d.dock == "left" {
			d.paintStrip(dockRow, colStart, d.size, phys)
			colStart += d.size
		}
	}
	coff := c.contentColOffset() // content col 0 draws at physical coff (= left + frame)
	for i, g := range content {
		if col := coff + i; col >= 0 && col < len(phys) {
			phys[col] = g
		}
	}
	// framed: the left and right outer-frame columns bracket the content on every
	// window row (│, or ├/┤ where an interior horizontal divider meets the frame).
	if frame > 0 && c.layout != nil && winRow >= 0 && winRow < c.layout.Rows {
		if l := coff - 1; l >= 0 && l < len(phys) {
			fg, bg, attr := c.borderStyleAt(-1, winRow) // frame col in content coords
			phys[l] = emu.Glyph{Char: c.borderRuneAt(winRow, -1, '│'), FG: fg, BG: bg, Mode: attr}
		}
		if r := coff + c.contentCols(); r >= 0 && r < len(phys) {
			fg, bg, attr := c.borderStyleAt(c.contentCols(), winRow)
			phys[r] = emu.Glyph{Char: c.borderRuneAt(winRow, c.contentCols(), '│'), FG: fg, BG: bg, Mode: attr}
		}
	}
	colStart = c.cols() - right
	for _, d := range c.docks {
		if d.dock == "right" {
			d.paintStrip(dockRow, colStart, d.size, phys)
			colStart += d.size
		}
	}
	return phys
}

// borderRuneAt returns the precomputed joined/framed glyph for a content-space
// border cell, or the fallback if none.
func (c *compositor) borderRuneAt(r, col int, fallback rune) rune {
	if ch, ok := c.borderRunes[[2]int{r, col}]; ok {
		return ch
	}
	return fallback
}

// buildFrameRow renders one of the two horizontal outer-frame rows (top or
// bottom) for pane_borders="framed": the ─ line with corners and ┬/┴ tees where
// interior vertical dividers meet it, plus the frame title at its anchor.
func (c *compositor) buildFrameRow(top bool) emu.Line {
	line := make(emu.Line, c.cols())
	for i := range line {
		line[i] = c.fillGlyph()
	}
	coff := c.contentColOffset()
	rc := -1 // content-row coord of the top frame
	if !top {
		rc = c.layout.Rows
	}
	// Docks span the full client height, so their strips must cover the frame
	// rows too — otherwise those cells fall through to the dot-fill above and a
	// left/right dock shows a row of dots level with the frame. Out-of-range
	// winRow makes paintStrip style-fill, continuing the dock's background.
	colStart := 0
	for _, d := range c.docks {
		if d.dock == "left" {
			d.paintStrip(rc+c.frameInset(), colStart, d.size, line)
			colStart += d.size
		}
	}
	colStart = c.cols() - c.rightInset()
	for _, d := range c.docks {
		if d.dock == "right" {
			d.paintStrip(rc+c.frameInset(), colStart, d.size, line)
			colStart += d.size
		}
	}
	for cc := -1; cc <= c.contentCols(); cc++ {
		if phys := coff + cc; phys >= 0 && phys < len(line) {
			fg, bg, attr := c.borderStyleAt(cc, rc)
			line[phys] = emu.Glyph{Char: c.borderRuneAt(rc, cc, '─'), FG: fg, BG: bg, Mode: attr}
		}
	}
	tfg, tbg, tattr := c.cfg.InactiveBorderFG, c.cfg.InactiveBorderBG, c.cfg.InactiveBorderAttr
	c.drawFrameTitle(line, top, coff, emu.Glyph{FG: tfg, BG: tbg, Mode: tattr})
	return line
}

// drawFrameTitle embeds the outer-frame title (window identity) on the given
// frame line if pane_border_title anchors to this edge. Placement mirrors the
// widget box titles: edge × left/centre/right, plus pane_border_offset.
func (c *compositor) drawFrameTitle(line emu.Line, top bool, coff int, style emu.Glyph) {
	at := c.cfg.PaneBorderTitle
	if at == "" || c.expander == nil || c.status == nil {
		return
	}
	if top != strings.HasPrefix(at, "top") {
		return
	}
	text := c.expander.expand("#{window_index}:#{window_name}", c.status.Vars, c.status.ServerShell)
	if text == "" {
		return
	}
	rs := []rune(" " + text + " ")
	inner := c.contentCols() // cells between the two frame corners
	start := coff            // left (default): just after the left corner
	switch {
	case strings.HasSuffix(at, "centre"), strings.HasSuffix(at, "center"):
		start = coff + (inner-len(rs))/2
	case strings.HasSuffix(at, "right"):
		start = coff + inner - len(rs)
	}
	start += c.cfg.PaneBorderOffset
	lo, hi := coff, coff+inner-1 // stay strictly between the corners
	for i, r := range rs {
		if col := start + i; col >= lo && col <= hi && col < len(line) {
			g := style
			g.Char = r
			line[col] = g
		}
	}
}

// clockText is the time shown in clock-mode — the status bar's clock var, or a
// placeholder before the first status tick.
func (c *compositor) clockText() string {
	if c.status != nil {
		if t := c.status.Vars["clock"]; t != "" {
			return t
		}
	}
	return "--:--"
}

// clockFont is a 3-wide × 5-tall block font for clock-mode's big digits
// (tmux's window-clock look).
var clockFont = map[rune][5]string{
	'0': {"███", "█ █", "█ █", "█ █", "███"},
	'1': {"  █", "  █", "  █", "  █", "  █"},
	'2': {"███", "  █", "███", "█  ", "███"},
	'3': {"███", "  █", "███", "  █", "███"},
	'4': {"█ █", "█ █", "███", "  █", "  █"},
	'5': {"███", "█  ", "███", "  █", "███"},
	'6': {"███", "█  ", "███", "█ █", "███"},
	'7': {"███", "  █", "  █", "  █", "  █"},
	'8': {"███", "█ █", "███", "█ █", "███"},
	'9': {"███", "█ █", "███", "  █", "███"},
	':': {"   ", " █ ", "   ", " █ ", "   "},
	' ': {"   ", "   ", "   ", "   ", "   "},
}

// bigClock renders a time string (e.g. "15:04") as 5 rows of block glyphs,
// one space between characters. Unknown chars render as blank.
func bigClock(s string) []string {
	var rows [5]strings.Builder
	for _, ch := range s {
		g, ok := clockFont[ch]
		if !ok {
			g = clockFont[' ']
		}
		for r := 0; r < 5; r++ {
			if rows[r].Len() > 0 {
				rows[r].WriteByte(' ')
			}
			rows[r].WriteString(g[r])
		}
	}
	out := make([]string, 5)
	for r := range out {
		out[r] = rows[r].String()
	}
	return out
}

// feedLock handles a chunk of input while the lock overlay is up. With no
// configured lock_password, any key unlocks (returns true). With one, printable
// bytes accumulate (never echoed) and Enter checks: a match unlocks, a mismatch
// clears the buffer and stays locked.
func (c *compositor) feedLock(data []byte) bool {
	if c.cfg.LockPassword == "" {
		return true
	}
	for _, b := range data {
		switch b {
		case '\r', '\n':
			if string(c.lockBuf) == c.cfg.LockPassword {
				c.lockBuf = nil
				return true
			}
			c.lockBuf = nil
		case 0x7f, 0x08:
			if len(c.lockBuf) > 0 {
				c.lockBuf = c.lockBuf[:len(c.lockBuf)-1]
			}
		default:
			if b >= 0x20 && b < 0x7f {
				c.lockBuf = append(c.lockBuf, b)
			}
		}
	}
	return false
}

// drawCentered writes text centered on a window row (content-width canvas).
func (c *compositor) drawCentered(text string, line emu.Line) {
	runes := []rune(text)
	start := (c.contentCols() - len(runes)) / 2
	if start < 0 {
		start = 0
	}
	for i, r := range runes {
		col := start + i
		if col >= 0 && col < c.contentCols() {
			line[col] = emu.Glyph{Char: r, FG: c.cfg.StatusFG, BG: c.cfg.StatusBG, Mode: emu.AttrBold}
		}
	}
}

// buildCopyRow paints one pane row from the client's frozen copy-mode
// snapshot, highlighting the cursor block and any selection locally.
func (c *compositor) buildCopyRow(pr proto.PaneRect, localRow int, line emu.Line) {
	cm := c.copy
	bufY := cm.top + localRow
	var src emu.Line
	if bufY >= 0 && bufY < len(cm.lines) {
		src = cm.lines[bufY]
	}
	for x := 0; x < pr.Cols; x++ {
		col := pr.Col + x
		if col < 0 || col >= c.contentCols() {
			continue
		}
		g := emu.EmptyGlyph()
		if x < len(src) {
			g = src[x]
		}
		switch {
		case bufY == cm.cy && x == cm.cx:
			g.FG, g.BG = c.cfg.CopyCursorFG, c.cfg.CopyCursorBG
			g.Mode &^= emu.AttrReverse // forced colors win; don't let the cell re-swap them
		case cm.selecting && cm.inSelection(bufY, x):
			g.FG, g.BG = c.cfg.CopySelectionFG, c.cfg.CopySelectionBG
			g.Mode &^= emu.AttrReverse
		}
		line[col] = g
	}
}

// copyFeed drives copy-mode from a chunk of local input and returns the redraw
// bytes plus the result (exit/yank). Clears copy-mode on exit. No-op if not in
// copy-mode.
func (c *compositor) copyFeed(data []byte) ([]byte, copyResult) {
	if c.copy == nil {
		return nil, copyResult{}
	}
	res := c.copy.feed(data)
	if res.exit {
		c.copy = nil
	}
	return c.redraw(), res
}

// overlayRow paints the picker overlay's slice of one physical row. A picker
// with per-item previews gets a tmux-like two-column bordered box (list left,
// pane preview right); a plain picker gets the simple centered list box.
func (c *compositor) overlayRow(row int, line emu.Line) {
	if c.layout == nil {
		return
	}
	if len(c.picker.previews) > 0 {
		c.overlayRowSplit(row, line)
		return
	}
	c.overlayRowSimple(row, line)
}

// overlayRowSimple is the plain centered list box (choose-window/buffer/client):
// title on top, an optional filter line, one item per row, selection highlighted.
func (c *compositor) overlayRowSimple(row int, line emu.Line) {
	pk := c.picker
	view := pk.view()

	rows := []string{pk.title}
	filterRow := -1
	if pk.filterable {
		filterRow = len(rows)
		rows = append(rows, "filter: "+pk.filter)
	}
	itemStart := len(rows)
	for _, oi := range view {
		rows = append(rows, pk.items[oi])
	}

	// Width from all items (not just the filtered set) so the box doesn't jump
	// around as you type.
	width := len(pk.title)
	for _, it := range pk.items {
		if len(it) > width {
			width = len(it)
		}
	}
	if filterRow >= 0 && len(rows[filterRow]) > width {
		width = len(rows[filterRow])
	}
	width += 4 // two cells of padding each side
	if width > c.contentCols() {
		width = c.contentCols()
	}
	height := len(rows)
	startRow := (c.layout.Rows - height) / 2
	if startRow < 0 {
		startRow = 0
	}
	startCol := (c.contentCols() - width) / 2
	boxRow := row - startRow
	if boxRow < 0 || boxRow >= height {
		return
	}

	text := rows[boxRow]
	fg, bg := c.cfg.StatusFG, c.cfg.StatusBG
	bold := boxRow == 0 || boxRow == filterRow
	if boxRow >= itemStart && boxRow-itemStart == pk.sel {
		fg, bg = c.cfg.ActiveWindowFG, c.cfg.ActiveWindowBG
	}
	runes := []rune(text)
	for x := 0; x < width; x++ {
		col := startCol + x
		if col < 0 || col >= c.contentCols() {
			continue
		}
		g := emu.Glyph{Char: ' ', FG: fg, BG: bg}
		if x >= 2 && x-2 < len(runes) {
			g.Char = runes[x-2]
		}
		if bold {
			g.Mode = emu.AttrBold
		}
		line[col] = g
	}
}

// overlayRowSplit paints the tmux-choose-tree-style box: the list in a left
// column and the highlighted item's pane preview in a right column, split by a
// vertical border, the whole thing framed. Titles ride the top border.
func (c *compositor) overlayRowSplit(row int, line emu.Line) {
	pk := c.picker
	view := pk.view()

	// Left column: optional filter line, then the (filtered) items.
	var left []string
	itemBase := 0
	if pk.filterable {
		left = append(left, "filter: "+pk.filter)
		itemBase = 1
	}
	for _, oi := range view {
		left = append(left, pk.items[oi])
	}

	// Right column: the selected item's styled preview lines + a title (its list text).
	var right []emu.Line
	previewTitle := "preview"
	if pk.sel >= 0 && pk.sel < len(view) {
		right = pk.previews[view[pk.sel]]
		previewTitle = strings.TrimSpace(pk.items[view[pk.sel]])
	}

	// Left column = list content width (stable while filtering), bounded to a
	// quarter of the screen so the preview dominates like tmux's choose-tree.
	leftW := len([]rune(pk.title))
	for _, it := range pk.items {
		if n := len([]rune(it)); n > leftW {
			leftW = n
		}
	}
	if pk.filterable && len([]rune(left[0])) > leftW {
		leftW = len([]rune(left[0]))
	}
	if maxL := c.contentCols() / 4; maxL > 8 && leftW > maxL {
		leftW = maxL
	}

	// The box fills most of the screen (tmux choose-tree -Z is near-full-screen);
	// the preview takes all the width and height the list doesn't need.
	const previewMax = 60
	if len(right) > previewMax {
		right = right[:previewMax]
	}
	width := c.contentCols() - 4
	if min := leftW + 12; width < min {
		width = min
	}
	rightW := width - leftW - 7
	if rightW < 8 {
		rightW = 8
	}
	bodyH := c.layout.Rows - 4
	if bodyH < len(left) {
		bodyH = len(left) // never clip the list
	}
	if bodyH < 3 {
		bodyH = 3
	}
	height := bodyH + 2
	startRow := (c.layout.Rows - height) / 2
	if startRow < 0 {
		startRow = 0
	}
	startCol := (c.contentCols() - width) / 2
	boxRow := row - startRow
	if boxRow < 0 || boxRow >= height {
		return
	}

	pad := func(s string, w int) []rune {
		r := []rune(s)
		if len(r) > w {
			return r[:w]
		}
		for len(r) < w {
			r = append(r, ' ')
		}
		return r
	}
	// seg builds a "─ title ───" border segment of exactly w cells.
	seg := func(title string, w int) []rune {
		out := append([]rune{'─', ' '}, []rune(title)...)
		out = append(out, ' ')
		if len(out) > w {
			out = out[:w]
		}
		for len(out) < w {
			out = append(out, '─')
		}
		return out
	}

	sfg, sbg := c.cfg.StatusFG, c.cfg.StatusBG
	cells := make([]emu.Glyph, width)
	for i := range cells {
		cells[i] = emu.Glyph{Char: ' ', FG: sfg, BG: sbg}
	}
	put := func(start int, rs []rune, bold bool) {
		for j, r := range rs {
			if col := start + j; col >= 0 && col < width {
				g := emu.Glyph{Char: r, FG: sfg, BG: sbg}
				if bold {
					g.Mode = emu.AttrBold
				}
				cells[col] = g
			}
		}
	}
	switch {
	case boxRow == 0:
		row := append([]rune{'┌'}, seg(pk.title, leftW+2)...)
		row = append(row, '┬')
		row = append(row, seg(previewTitle, rightW+2)...)
		row = append(row, '┐')
		put(0, row, true)
	case boxRow == height-1:
		row := []rune{'└'}
		for i := 0; i < leftW+2; i++ {
			row = append(row, '─')
		}
		row = append(row, '┴')
		for i := 0; i < rightW+2; i++ {
			row = append(row, '─')
		}
		row = append(row, '┘')
		put(0, row, false)
	default:
		i := boxRow - 1
		put(0, []rune{'│'}, false)         // left frame
		put(3+leftW, []rune{'│'}, false)   // middle divider
		put(width-1, []rune{'│'}, false)   // right frame
		// Left column: the item text, active-colored if it's the selection.
		lc := pad("", leftW)
		if i < len(left) {
			lc = pad(left[i], leftW)
		}
		selected := i == itemBase+pk.sel
		for j, r := range lc {
			g := emu.Glyph{Char: r, FG: sfg, BG: sbg}
			if selected {
				g.FG, g.BG = c.cfg.ActiveWindowFG, c.cfg.ActiveWindowBG
			}
			cells[2+j] = g
		}
		// Right column: the preview line's own glyphs (real pane colors).
		if i < len(right) {
			ln := right[i]
			for x := 0; x < rightW; x++ {
				if x < len(ln) {
					g := ln[x]
					if g.Char == 0 {
						g.Char = ' '
					}
					cells[5+leftW+x] = g
				}
			}
		}
	}

	for x := 0; x < width; x++ {
		if col := startCol + x; col >= 0 && col < c.contentCols() {
			line[col] = cells[x]
		}
	}
}

// mouseResult is what the client should do with a mouse event after the
// compositor recognizes the gesture from its own Layout: dispatch string
// actions (0-2), send a ResizeBorder / CopyDrag request, and/or forward the
// raw event to the server (only for a pane whose app tracks the mouse).
type mouseResult struct {
	actions  [][]string
	border   *proto.ResizeBorder
	copyDrag *proto.CopyDrag
	forward  bool
}

// mouseAction is the client's whole mouse-gesture recognizer (client-owned
// mouse): status-bar clicks, pane-border drags, focus-clicks, wheel→copy-mode,
// and drag-to-copy are all decided here from the Layout the client already
// holds. Only an event over a pane whose app requested mouse tracking is
// forwarded to the server for that app.
func (c *compositor) mouseAction(me proto.MouseEvent) mouseResult {
	// Status-bar window-label clicks are handled earlier via the status
	// component's regions (clickWidget), not here.
	if c.layout == nil || c.status == nil {
		return mouseResult{forward: true} // no layout yet: forward raw, as before
	}

	isMotion := me.Cb&0x20 != 0
	isWheel := me.Cb&0x40 != 0
	isLeft := me.Cb&3 == 0
	winRow, winCol := me.Y-1-c.contentOffset(), me.X-1-c.contentColOffset()

	// Border drag: a left-press grabs the divider under the pointer; each motion
	// moves it (ResizeBorder to the server); release lets go.
	if isLeft && me.Press && !isMotion {
		for i, b := range c.layout.Borders {
			onV := b.Vertical && winCol == b.Fixed && winRow >= b.Start && winRow < b.End
			onH := !b.Vertical && winRow == b.Fixed && winCol >= b.Start && winCol < b.End
			if onV || onH {
				c.dragBorder = i
				return mouseResult{}
			}
		}
	}
	if c.dragBorder >= 0 {
		if isMotion && isLeft && c.dragBorder < len(c.layout.Borders) {
			pos := winCol
			if !c.layout.Borders[c.dragBorder].Vertical {
				pos = winRow
			}
			return mouseResult{border: &proto.ResizeBorder{Index: c.dragBorder, Pos: pos}}
		}
		if !me.Press {
			c.dragBorder = -1
		}
		return mouseResult{}
	}

	target, ok := c.paneAt(winRow, winCol)
	if !ok {
		return mouseResult{}
	}
	// A pane whose app tracks the mouse gets the event forwarded verbatim — but
	// a left-press on an unfocused one still focuses it first (like tmux), then
	// forwards. Without this, clicking into a mouse-tracking pane (nvim, less,
	// Claude Code) sent the click to the app but never switched panes.
	if target.WantsMouse {
		mr := mouseResult{forward: true}
		if isLeft && me.Press && !isMotion && !target.Active {
			mr.actions = [][]string{{"select-pane", "-t", fmt.Sprintf("%%%d", target.ID)}}
		}
		return mr
	}

	// Non-tracking pane — the client owns the gesture.
	if isWheel {
		if me.Cb&1 == 0 { // wheel up enters copy-mode on that pane
			return mouseResult{actions: [][]string{
				{"select-pane", "-t", fmt.Sprintf("%%%d", target.ID)},
				{"copy-mode"},
			}}
		}
		return mouseResult{}
	}
	localRow, localCol := winRow-target.Row, winCol-target.Col
	if isLeft && me.Press && !isMotion {
		// Focus the pane (select-pane runs the server's focus-events path) and
		// arm drag-to-copy from this cell.
		c.dcPane, c.dcRow, c.dcCol, c.dcArmed, c.dcActive = target.ID, localRow, localCol, true, false
		return mouseResult{actions: [][]string{{"select-pane", "-t", fmt.Sprintf("%%%d", target.ID)}}}
	}
	if !me.Press {
		c.dcArmed, c.dcActive = false, false
	}
	if isMotion && isLeft && c.dcArmed && !c.dcActive && target.ID == c.dcPane {
		c.dcActive = true
		return mouseResult{copyDrag: &proto.CopyDrag{PaneID: c.dcPane, Row: c.dcRow, Col: c.dcCol}}
	}
	return mouseResult{}
}

// paneAt returns the pane rect covering a window-space cell.
func (c *compositor) paneAt(row, col int) (proto.PaneRect, bool) {
	if c.layout == nil {
		return proto.PaneRect{}, false
	}
	for _, p := range c.layout.Panes {
		if row >= p.Row && row < p.Row+p.Rows && col >= p.Col && col < p.Col+p.Cols {
			return p, true
		}
	}
	return proto.PaneRect{}, false
}

// copyMouse drives copy-mode from a mouse event over the frozen snapshot:
// wheel scrolls, a left press positions the cursor and anchors a selection,
// drag extends it, and release yanks. Returns redraw bytes + result (like
// copyFeed). Coordinates are mapped to buffer indices via the pane's rect.
func (c *compositor) copyMouse(me proto.MouseEvent) ([]byte, copyResult) {
	cm := c.copy
	if cm == nil {
		return nil, copyResult{}
	}
	pr, ok := c.rectFor(cm.paneID)
	if !ok {
		return c.redraw(), copyResult{}
	}
	row, col := me.Y-1, me.X-1-c.contentColOffset()
	isWheel := me.Cb&0x40 != 0
	isMotion := me.Cb&0x20 != 0
	isLeft := me.Cb&3 == 0
	switch {
	case isWheel:
		n := c.cfg.CopyWheelLines
		if n <= 0 {
			n = 3
		}
		if me.Cb&1 == 0 { // 64 = wheel up, 65 = wheel down
			cm.cy -= n
		} else {
			cm.cy += n
		}
	case isLeft && me.Press && !isMotion:
		cm.cy, cm.cx = cm.top+(row-pr.Row), col-pr.Col
		cm.clamp()
		cm.selY, cm.selX, cm.selecting, cm.lineSel = cm.cy, cm.cx, true, false
	case isLeft && isMotion && cm.selecting:
		cm.cy, cm.cx = cm.top+(row-pr.Row), col-pr.Col
	case !me.Press && cm.selecting:
		cm.clamp()
		cm.scroll()
		if cm.cy != cm.selY || cm.cx != cm.selX {
			// copy-drag-finish (tmux's MouseDragEnd1Pane copy-selection-and-cancel):
			// default yanks + exits; when off the selection persists for a manual `y`.
			if c.cfg.CopyDragFinish {
				return c.redraw(), copyResult{exit: true, yank: cm.selectedText()}
			}
			return c.redraw(), copyResult{}
		}
		cm.selecting = false
	}
	cm.clamp()
	cm.scroll()
	return c.redraw(), copyResult{}
}

// reload swaps in a freshly-loaded client config (source-file) and repaints.
// The caller holds compMu. Status formats (stLeft/stRight) refresh on the next
// server status tick; colors/borders take effect on this redraw.
//
// binds is the VM the new config's widget fns live in (curBinds() after the
// swap); nil keeps the existing widgets, for callers that only changed options.
func (c *compositor) reload(cfg config.ClientConfig, binds *config.ClientBinds) []byte {
	c.cfg = cfg
	c.rebuildBorders() // pane_borders may have changed
	if binds != nil {
		// Widgets are built from the config, so a reload has to rebuild them —
		// otherwise a re-sourced config keeps the OLD status bar (its fg/bg and
		// format are captured per widget at build time), and added/removed
		// widgets are ignored. Mirrors the build at attach (client.go).
		c.rebuildWidgets(binds)
	}
	// ponytail: no extended-keys renegotiation here — every path that changes the
	// option produces a Layout that reconciles; a source-file with no following
	// output self-heals on the next Layout event. Add back if that ever bites.
	return c.redraw()
}

// rebuildWidgets recreates the status/dock/float widgets from c.cfg.Widgets,
// dropping the previous set. Shared by attach-time setup and reload.
func (c *compositor) rebuildWidgets(binds *config.ClientBinds) {
	c.statusWidget, c.docks, c.overlays = nil, nil, nil
	for _, w := range c.cfg.Widgets {
		b := &textBox{
			row: w.Row, col: w.Col, format: w.Text,
			lines: strings.Split(w.Text, "\n"), // shown until the first refresh re-expands
			fg:    w.FG, bg: w.BG, attr: w.Attr,
			dock: w.Dock, size: w.Size,
			textFn: w.TextFn, drawFn: w.Draw, component: w.Component, onClick: w.OnClick, binds: binds, interval: w.Interval,
			w: w.Width, h: w.Height, // float draw size; docks override w/h per tick
		}
		switch w.Dock {
		case "status":
			c.statusWidget = b // owns the status rows (replaces renderBar)
		case "":
			c.overlays = append(c.overlays, b)
		default:
			c.docks = append(c.docks, b)
		}
	}
}

// openLocal opens a prompt/picker overlay from the client's own mirrored state
// (a keybind's BindOp.Local), so no server round-trip is needed and there's no
// window where the user types before the prompt has opened. Returns redraw bytes.
// editKeys reports whether the command prompt uses emacs line-editing keys
// (status-keys); vi is plain since ESC cancels the prompt (no modal editing).
func (c *compositor) editKeys() bool { return c.cfg.StatusKeys != "vi" }

func (c *compositor) openLocal(kind string) []byte {
	switch kind {
	case "command-prompt":
		c.prompt = newPrompt(&proto.OpenPrompt{Kind: "command"}, c.editKeys())
	case "rename-window":
		c.prompt = newPrompt(&proto.OpenPrompt{Kind: "window", Prefill: c.activeWindowName()}, c.editKeys())
	case "rename-session":
		name := ""
		if c.status != nil {
			name = c.status.Vars["session"]
		}
		c.prompt = newPrompt(&proto.OpenPrompt{Kind: "session", Prefill: name}, c.editKeys())
	case "choose-window":
		c.picker = c.buildWindowPicker()
	}
	return c.redraw()
}

// openFlowPrompt opens the overlay for a parameterized command-prompt or
// confirm-before (built by the config's gtmux.command_prompt/confirm_before
// primitives, encoded as an Action). Args:
//
//	command-prompt [-p label] [-I initial] [-- template...]
//	confirm-before  -p label   --  command...
func (c *compositor) openFlowPrompt(action []string) []byte {
	var initial string
	var labels, tail []string
	hasTail := false
	for i := 1; i < len(action); {
		switch action[i] {
		case "-p":
			if i+1 < len(action) {
				// tmux -p "a,b" asks once per comma-separated label.
				labels = strings.Split(action[i+1], ",")
			}
			i += 2
		case "-I":
			if i+1 < len(action) {
				initial = action[i+1]
			}
			i += 2
		case "--":
			tail, hasTail = action[i+1:], true
			i = len(action)
		default:
			i++
		}
	}
	if action[0] == "confirm-before" {
		c.prompt = &prompt{kind: "confirm", labels: labels, cmd: tail}
	} else {
		p := &prompt{kind: "command", labels: labels, buf: []byte(initial), editKeys: c.editKeys()}
		if hasTail {
			p.tmpl = tail
		}
		c.prompt = p
	}
	return c.redraw()
}

// popupOverlay is the client-side state for a display-popup: a floating
// terminal's content (popup-local coords), rendered centered in a bordered box.
type popupOverlay struct {
	cols, rows    int
	x, y          int              // box left/top, or -1 to center that axis
	lines         map[int]emu.Line // local row -> line
	cursor        emu.Cursor
	cursorVisible bool
}

// applyPopup opens/updates/closes the popup overlay from a server PopupMsg.
func (c *compositor) applyPopup(m *proto.PopupMsg) {
	if m.Close {
		c.popup = nil
		return
	}
	if m.Open {
		c.popup = &popupOverlay{cols: m.Cols, rows: m.Rows, x: m.X, y: m.Y, lines: map[int]emu.Line{}}
	}
	if c.popup == nil {
		return
	}
	if m.Content != nil {
		for row, line := range m.Content.Lines {
			c.popup.lines[row] = line
		}
		c.popup.cursor = m.Content.Cursor
		c.popup.cursorVisible = m.Content.CursorVisible
	}
}

// popupBounds returns the top-left content cell (startRow,startCol) of the
// centered popup, and whether it fits. The bordered box occupies one extra cell
// on every side.
func (c *compositor) popupBounds() (startRow, startCol int, ok bool) {
	p := c.popup
	if p == nil || c.layout == nil {
		return 0, 0, false
	}
	// p.x/p.y are the box's left/top (border corner); content sits one cell in.
	// -1 on an axis means center it.
	if p.x >= 0 {
		startCol = p.x + 1
	} else {
		startCol = (c.contentCols() - p.cols) / 2
	}
	if p.y >= 0 {
		startRow = p.y + 1
	} else {
		startRow = (c.layout.Rows - p.rows) / 2
	}
	if startCol < 1 {
		startCol = 1
	}
	if startRow < 1 {
		startRow = 1
	}
	return startRow, startCol, true
}

// popupRow composites the popup box's slice of one physical row: a border on the
// box edges, the popup's terminal cells inside.
func (c *compositor) popupRow(row int, line emu.Line) {
	p := c.popup
	startRow, startCol, ok := c.popupBounds()
	if !ok {
		return
	}
	top, bottom := startRow-1, startRow+p.rows
	left, right := startCol-1, startCol+p.cols
	if row < top || row > bottom {
		return
	}
	border := emu.Glyph{Char: '─', FG: c.cfg.ActiveBorderFG, BG: emu.DefaultBG}
	put := func(col int, g emu.Glyph) {
		if col >= 0 && col < c.contentCols() {
			line[col] = g
		}
	}
	if row == top || row == bottom {
		for col := left; col <= right; col++ {
			put(col, border)
		}
		return
	}
	// Interior row: side borders + content cells.
	put(left, emu.Glyph{Char: '│', FG: c.cfg.ActiveBorderFG, BG: emu.DefaultBG})
	put(right, emu.Glyph{Char: '│', FG: c.cfg.ActiveBorderFG, BG: emu.DefaultBG})
	src := p.lines[row-startRow]
	for x := 0; x < p.cols; x++ {
		g := emu.EmptyGlyph()
		if x < len(src) {
			g = src[x]
		}
		put(startCol+x, g)
	}
}

// openMenu opens a display-menu overlay (reusing the picker): a list of items,
// each of which runs a command line when selected. Args:
//
//	display-menu [-T title] -- name1 cmd1 name2 cmd2 ...
//
// Built by gtmux.display_menu (encoded as an Action). Each cmd is one arg;
// multi-word commands survive, quoting doesn't (same limitation as elsewhere).
func (c *compositor) openMenu(action []string) []byte {
	title := "menu"
	var pairs []string
	for i := 1; i < len(action); {
		switch action[i] {
		case "-T":
			if i+1 < len(action) {
				title = action[i+1]
			}
			i += 2
		case "--":
			pairs = action[i+1:]
			i = len(action)
		default:
			i++
		}
	}
	var items, cmds []string
	for i := 0; i+1 < len(pairs); i += 2 {
		items = append(items, pairs[i])
		cmds = append(cmds, pairs[i+1])
	}
	if len(items) > 0 {
		c.picker = &picker{title: title, verb: "run", items: items, targets: cmds}
	}
	return c.redraw()
}

// activeWindowName returns the mirrored name of the active window (for the
// rename-window prefill), or "" if unknown.
func (c *compositor) activeWindowName() string {
	if c.status != nil {
		for _, w := range c.status.Windows {
			if w.Active {
				return w.Name
			}
		}
	}
	return ""
}

// buildWindowPicker builds the choose-window picker from the mirrored window
// list, matching the server's old item/target format so selecting sends
// {select-window, index}.
func (c *compositor) buildWindowPicker() *picker {
	var items, targets []string
	if c.status != nil {
		for _, w := range c.status.Windows {
			items = append(items, fmt.Sprintf("%d: %s (%d panes)", w.Index, w.Name, w.Panes))
			targets = append(targets, strconv.Itoa(w.Index))
		}
	}
	return newPicker(&proto.OpenPicker{Title: "choose window", Verb: "select-window", Items: items, Targets: targets})
}

// promptFeed drives a text-entry prompt from a chunk of local input, returning
// redraw bytes + the result. Clears the prompt when it closes.
func (c *compositor) promptFeed(data []byte) ([]byte, promptResult) {
	if c.prompt == nil {
		return nil, promptResult{}
	}
	res := c.prompt.feed(data)
	if res.done {
		c.prompt = nil
	}
	return c.redraw(), res
}

// pickerFeed drives a selection list from a chunk of local input, returning
// redraw bytes + the result. Clears the picker when it closes.
func (c *compositor) pickerFeed(data []byte) ([]byte, pickerResult) {
	if c.picker == nil {
		return nil, pickerResult{}
	}
	res := c.picker.feed(data)
	if res.done {
		c.picker = nil
	}
	return c.redraw(), res
}

// borderStyleAt resolves one border cell's style in CONTENT coordinates, so the
// interior dividers and the framed outer frame share one rule (the frame used to
// hardcode the inactive style, so a framed window never showed which pane was
// active). Precedence: pane-border-style, then the active pane's own ring cells,
// then a marked pane, then a pane:set_border override.
func (c *compositor) borderStyleAt(col, row int) (emu.Color, emu.Color, int16) {
	fg, bg, mode := c.cfg.InactiveBorderFG, c.cfg.InactiveBorderBG, c.cfg.InactiveBorderAttr
	if c.layout == nil {
		return fg, bg, mode
	}
	twoPanes := len(c.layout.Panes) == 2
	for i := range c.layout.Panes {
		p := c.layout.Panes[i]
		if !p.Active || !onPaneRing(p, col, row) {
			continue
		}
		// Two-pane indicator: only HALF the shared divider lights, so you can tell
		// which pane owns it. But that half-split applies ONLY to a cell shared
		// with the other pane (the divider between them) — a non-shared edge (the
		// outer frame in framed mode) is touched by one pane and must light fully.
		// Applying the split to the frame made it half-green/half-inactive, which
		// read as broken.
		if twoPanes && c.sharedBorderCell(col, row, i) && !activeBorderHalf(p, col, row) {
			continue // the neighbour's half of the shared divider stays inactive
		}
		fg, bg, mode = c.cfg.ActiveBorderFG, emu.DefaultBG, 0
	}
	for i := range c.layout.Panes {
		if c.layout.Panes[i].Marked && onPaneRing(c.layout.Panes[i], col, row) {
			fg, bg, mode = c.cfg.MarkedBorderFG, emu.DefaultBG, 0
		}
	}
	// pane:set_border override (e.g. command-exited flagging a pane).
	for id, override := range c.paneBorderColor {
		if pr, ok := c.rectFor(id); ok && onPaneRing(pr, col, row) {
			fg, bg, mode = override, emu.DefaultBG, 0
		}
	}
	return fg, bg, mode
}

// sharedBorderCell reports whether a border cell lies on another pane's ring
// too — i.e. it's a divider shared between the active pane (index exclude) and a
// neighbour, as opposed to a non-shared outer-frame edge. Gates the two-pane
// half-colour indicator so it only splits the genuine shared divider.
func (c *compositor) sharedBorderCell(col, row, exclude int) bool {
	for i := range c.layout.Panes {
		if i != exclude && onPaneRing(c.layout.Panes[i], col, row) {
			return true
		}
	}
	return false
}

// activeBorderHalf implements tmux's pane-border-indicators=colour split for a
// two-pane window: which HALF of the active pane's shared divider lights, so the
// two panes are distinguishable (a full-length divider is the same cells for
// both). The active pane's RIGHT edge (it is the left pane) lights the top half;
// its LEFT edge (it is the right pane) the bottom half; a horizontal divider
// splits its BOTTOM edge (top pane) into the left half and its TOP edge (bottom
// pane) into the right half — split at the pane's midpoint, matching tmux 3.6.
// Only meaningful for a shared divider cell (see sharedBorderCell).
func activeBorderHalf(pr proto.PaneRect, col, row int) bool {
	midRow, midCol := pr.Row+pr.Rows/2, pr.Col+pr.Cols/2
	switch {
	case col == pr.Col+pr.Cols: // right edge (pr is the left pane) -> top half
		return row <= midRow
	case col == pr.Col-1: // left edge (pr is the right pane) -> bottom half
		return row > midRow
	case row == pr.Row+pr.Rows: // bottom edge (pr is the top pane) -> left half
		return col <= midCol
	case row == pr.Row-1: // top edge (pr is the bottom pane) -> right half
		return col > midCol
	}
	return true
}

// onPaneRing reports whether cell (col,row) lies on pr's outline ring — the
// one-cell outset border around the pane, its four edges plus corners. Border
// color is picked per cell against this so a divider only lights along the
// pane's own edge, corners included (tmux-faithful).
func onPaneRing(pr proto.PaneRect, col, row int) bool {
	inCol := col >= pr.Col-1 && col <= pr.Col+pr.Cols
	inRow := row >= pr.Row-1 && row <= pr.Row+pr.Rows
	onColEdge := col == pr.Col-1 || col == pr.Col+pr.Cols
	onRowEdge := row == pr.Row-1 || row == pr.Row+pr.Rows
	return inCol && inRow && (onColEdge || onRowEdge)
}
