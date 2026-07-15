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

	// expander turns the client-owned status_left/status_right formats into
	// text against the server's Vars + ServerShell; stLeft/stRight cache the
	// last expansion so buildRow and the status-click hit-test share one result.
	expander        *statusExpander
	stLeft, stRight string
	stExtra         [4]string   // expanded ExtraStatusFormats (status lines 2..5)
	windowHits      []windowHit // per-window status click spans, set by renderBar
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

// contentOffset is the number of physical rows above the window content: the
// status-line count when the bar sits at the top, else 0. Window row R draws at
// physical row R+contentOffset.
func (c *compositor) contentOffset() int {
	if c.cfg.StatusPosition == "top" {
		return c.statusLines()
	}
	return 0
}

// statusRow is the physical row of the MAIN bar (status line 1: left + window
// list + right). Extra lines stack inward from it (see statusRowKind).
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
	inContent := func(rWin int) bool { return rWin >= 0 && rWin < c.layout.Rows }
	// A popup grabs the cursor: place it inside the box at the popup's cursor.
	if c.popup != nil {
		if sr, sc, ok := c.popupBounds(); ok {
			r, col := sr+c.popup.cursor.R, sc+c.popup.cursor.C
			return r + off, col, c.popup.cursorVisible && inContent(r) && col >= 0 && col < c.cols()
		}
	}
	if c.copy != nil {
		if pr, ok := c.rectFor(c.copy.paneID); ok {
			r := pr.Row + (c.copy.cy - c.copy.top)
			col := pr.Col + c.copy.cx
			return r + off, col, inContent(r) && col >= 0 && col < c.cols()
		}
	}
	for _, pr := range c.layout.Panes {
		if !pr.Active {
			continue
		}
		m := c.meta[pr.ID]
		r, col := pr.Row+m.cursor.R, pr.Col+m.cursor.C
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
func (c *compositor) apply(msg *proto.ServerMsg) []byte {
	dirty := map[int]bool{}
	var titleOut []byte // set-titles OSC, prepended to the emitted diff below
	var kittyOut []byte // extended-keys negotiation with the outer terminal

	if msg.Layout != nil {
		c.layout = msg.Layout
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
		if c.expander == nil {
			iv := c.cfg.StatusInterval
			if iv <= 0 {
				iv = 15
			}
			c.expander = newStatusExpander(time.Duration(iv) * time.Second)
		}
		c.stLeft = c.expander.expand(c.cfg.StatusLeft, msg.Status.Vars, msg.Status.ServerShell)
		c.stRight = c.expander.expand(c.cfg.StatusRight, msg.Status.Vars, msg.Status.ServerShell)
		for i := range c.cfg.ExtraStatusFormats {
			c.stExtra[i] = c.expander.expand(c.cfg.ExtraStatusFormats[i], msg.Status.Vars, msg.Status.ServerShell)
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
func (c *compositor) emit(dirty map[int]bool) []byte {
	var b strings.Builder
	for row := range dirty {
		if row < 0 || row >= c.totalRows() {
			continue
		}
		fmt.Fprintf(&b, "\x1b[%d;1H\x1b[2K", row+1)
		emu.WriteLine(&b, c.buildRow(row))
	}

	if row, col, visible := c.activeCursor(); visible {
		fmt.Fprintf(&b, "\x1b[%d;%dH\x1b[?25h", row+1, col+1)
	} else {
		b.WriteString("\x1b[?25l")
	}

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
		if extra >= 0 {
			return c.renderExtraStatus(extra)
		}
		// Main bar. Client-owned input modes draw their own status line, taking
		// precedence over the server's status.
		if c.copy != nil {
			return renderPromptLine(c.cols(), "copy-mode", c.copy.helpText(), c.cfg)
		}
		if c.prompt != nil {
			return renderPromptLine(c.cols(), c.prompt.label(), string(c.prompt.buf), c.cfg)
		}
		if c.status == nil {
			return make(emu.Line, c.cols())
		}
		// A transient server message (run-shell output, errors) borrows the
		// prompt layout; otherwise draw the normal bar from the expanded formats.
		if c.status.PromptLabel != "" {
			return renderPromptLine(c.cols(), c.status.PromptLabel, c.status.PromptText, c.cfg)
		}
		return c.renderBar()
	}

	// Below the status branch, work in window-row space: with the bar at the top
	// the window content is shifted down by contentOffset physical rows.
	row -= c.contentOffset()

	// Cells inside the window rectangle start as window background; cells
	// outside it (a larger client's slack) are dot-filled.
	line := make(emu.Line, c.cols())
	inWindowRow := c.layout != nil && row >= 0 && row < c.layout.Rows
	for i := range line {
		if c.layout != nil && !(inWindowRow && i < c.layout.Cols) {
			line[i] = c.fillGlyph()
		} else {
			line[i] = emu.EmptyGlyph()
		}
	}
	if c.layout == nil {
		return line
	}

	var activeRect, markedRect *proto.PaneRect
	for i := range c.layout.Panes {
		if c.layout.Panes[i].Active {
			activeRect = &c.layout.Panes[i]
		}
		if c.layout.Panes[i].Marked {
			markedRect = &c.layout.Panes[i]
		}
	}
	// Border color is decided per cell, not per divider: a cell lights active
	// (or marked) only where it lies on that pane's outline ring, so a long
	// divider shared by several stacked panes highlights just the active pane's
	// own edge instead of its full length.
	// Inactive dividers use pane-border-style (fg/bg/attr); the active/marked
	// pane's own ring cells override with their fg-only color.
	borderGlyph := func(char rune, col int) emu.Glyph {
		g := emu.Glyph{Char: char, FG: c.cfg.InactiveBorderFG, BG: c.cfg.InactiveBorderBG, Mode: c.cfg.InactiveBorderAttr}
		if activeRect != nil && onPaneRing(*activeRect, col, row) {
			g.FG, g.BG, g.Mode = c.cfg.ActiveBorderFG, emu.DefaultBG, 0
		}
		if markedRect != nil && onPaneRing(*markedRect, col, row) {
			g.FG, g.BG, g.Mode = c.cfg.MarkedBorderFG, emu.DefaultBG, 0
		}
		return g
	}
	for _, bd := range c.layout.Borders {
		if bd.Vertical {
			if row >= bd.Start && row < bd.End && bd.Fixed >= 0 && bd.Fixed < c.cols() {
				line[bd.Fixed] = borderGlyph('│', bd.Fixed)
			}
		} else if row == bd.Fixed {
			for col := bd.Start; col < bd.End && col < c.cols(); col++ {
				line[col] = borderGlyph('─', col)
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
			if col < 0 || col >= c.cols() {
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
		for col := pr.Col; col < pr.Col+pr.Cols && col < c.cols(); col++ {
			line[col] = emu.Glyph{Char: '─', FG: fg, BG: bg, Mode: mode}
		}
		col := pr.Col + 1
		for _, r := range pr.BorderLabel {
			if col >= pr.Col+pr.Cols || col >= c.cols() {
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

	if c.picker != nil {
		c.overlayRow(row, line)
	}
	if c.popup != nil {
		c.popupRow(row, line)
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

	return line
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

// drawCentered writes text centered on a window row.
func (c *compositor) drawCentered(text string, line emu.Line) {
	runes := []rune(text)
	start := (c.cols() - len(runes)) / 2
	if start < 0 {
		start = 0
	}
	for i, r := range runes {
		col := start + i
		if col >= 0 && col < c.cols() {
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
		if col < 0 || col >= c.cols() {
			continue
		}
		g := emu.EmptyGlyph()
		if x < len(src) {
			g = src[x]
		}
		switch {
		case bufY == cm.cy && x == cm.cx:
			g.FG, g.BG = c.cfg.CopyCursorFG, c.cfg.CopyCursorBG
		case cm.selecting && cm.inSelection(bufY, x):
			g.FG, g.BG = c.cfg.CopySelectionFG, c.cfg.CopySelectionBG
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
	if width > c.cols() {
		width = c.cols()
	}
	height := len(rows)
	startRow := (c.layout.Rows - height) / 2
	if startRow < 0 {
		startRow = 0
	}
	startCol := (c.cols() - width) / 2
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
		if col < 0 || col >= c.cols() {
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
	if maxL := c.cols() / 4; maxL > 8 && leftW > maxL {
		leftW = maxL
	}

	// The box fills most of the screen (tmux choose-tree -Z is near-full-screen);
	// the preview takes all the width and height the list doesn't need.
	const previewMax = 60
	if len(right) > previewMax {
		right = right[:previewMax]
	}
	width := c.cols() - 4
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
	startCol := (c.cols() - width) / 2
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
		if col := startCol + x; col >= 0 && col < c.cols() {
			line[col] = cells[x]
		}
	}
}

// resolveMouse turns a mouse event into a client-side action, or nil to forward
// it to the server. Only status-bar window clicks resolve locally (the client
// has the labels + widths); interior focus-clicks and border drags stay
// server-side, where the live pane mouse-mode and layout geometry are — the
// plan's sanctioned Stage-5 fallback for the events that need server state.
func (c *compositor) resolveMouse(me proto.MouseEvent) []string {
	if c.layout == nil || c.status == nil {
		return nil
	}
	row, col := me.Y-1, me.X-1
	// A plain left press on a window label in the status row selects it, using
	// the click spans renderBar recorded (so justify/format/position all match).
	if row == c.statusRow() && me.Press && me.Cb&0x60 == 0 && me.Cb&3 == 0 {
		for _, h := range c.windowHits {
			if col >= h.start && col < h.end {
				return []string{"select-window", strconv.Itoa(h.index)}
			}
		}
	}
	return nil
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
	// Status-bar window-label click (resolveMouse owns the labels + hit spans).
	if a := c.resolveMouse(me); a != nil {
		return mouseResult{actions: [][]string{a}}
	}
	if c.layout == nil || c.status == nil {
		return mouseResult{forward: true} // no layout yet: forward raw, as before
	}

	isMotion := me.Cb&0x20 != 0
	isWheel := me.Cb&0x40 != 0
	isLeft := me.Cb&3 == 0
	winRow, winCol := me.Y-1-c.contentOffset(), me.X-1

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
	// A pane whose app tracks the mouse gets the event forwarded verbatim.
	if target.WantsMouse {
		return mouseResult{forward: true}
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
	row, col := me.Y-1, me.X-1
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
func (c *compositor) reload(cfg config.ClientConfig) []byte {
	c.cfg = cfg
	// ponytail: no extended-keys renegotiation here — every path that changes the
	// option produces a Layout that reconciles; a source-file with no following
	// output self-heals on the next Layout event. Add back if that ever bites.
	return c.redraw()
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
		startCol = (c.cols() - p.cols) / 2
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
		if col >= 0 && col < c.cols() {
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
