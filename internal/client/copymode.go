package client

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/FyrmForge/gtmux/internal/emu"
	"github.com/FyrmForge/gtmux/internal/proto"
)

// copyMode is the client-side scrollback browser (tmux's copy-mode, prefix+[).
// Ported from what the server used to own: entering it snapshots the pane's
// history + current screen (delivered atomically by the server as
// proto.CopyModeEnter), and all movement/search/selection run locally over
// that frozen buffer. Now it's per-client — one client can scroll back while
// another follows the pane live.
type copyMode struct {
	paneID     int
	lines      []emu.Line
	rows       int  // visible pane height, for paging/scroll
	emacs      bool // emacs keytable (mode-keys emacs) instead of vi
	wordSep    string // word-separators: chars (besides whitespace) that bound w/b/e words
	top        int    // index into lines of the topmost visible row
	cy, cx     int    // cursor, as an index into lines/that line
	selecting  bool
	selY, selX int
	lineSel    bool
	rectSel    bool // rectangle (block) selection, C-v

	pending    byte // f/F/t/T awaiting its target char on the next keystroke
	count      int  // numeric prefix (1-9[0-9]*) multiplying the next motion

	searching  bool
	searchFwd  bool // direction of the active search: / forward, ? reverse
	searchBuf  []byte
	lastSearch string
	matches    [][2]int
	matchIdx   int
}

// newCopyMode builds copy-mode state from the server's frozen snapshot. emacs
// selects the emacs keytable (mode-keys); the keytable is fixed at entry, so a
// live `:set mode-keys` takes effect the next time copy-mode is entered.
func newCopyMode(m *proto.CopyModeEnter, rows int, emacs bool, wordSep string) *copyMode {
	cm := &copyMode{paneID: m.PaneID, lines: m.Lines, rows: rows, cy: m.CursorY, cx: m.CursorX, emacs: emacs, wordSep: wordSep}
	cm.clamp()
	// Drag-to-copy: begin a selection anchored at the entry cursor so the
	// continuing mouse drag extends it.
	if m.Select {
		cm.selecting = true
		cm.selY, cm.selX = cm.cy, cm.cx
	}
	cm.scroll()
	return cm
}

func (cm *copyMode) clamp() {
	if len(cm.lines) == 0 {
		cm.cy, cm.cx = 0, 0
		return
	}
	if cm.cy < 0 {
		cm.cy = 0
	}
	if cm.cy >= len(cm.lines) {
		cm.cy = len(cm.lines) - 1
	}
	maxX := len(cm.lines[cm.cy]) - 1
	if maxX < 0 {
		maxX = 0
	}
	if cm.cx < 0 {
		cm.cx = 0
	}
	if cm.cx > maxX {
		cm.cx = maxX
	}
}

func (cm *copyMode) scroll() {
	if cm.cy < cm.top {
		cm.top = cm.cy
	}
	if cm.cy >= cm.top+cm.rows {
		cm.top = cm.cy - cm.rows + 1
	}
	maxTop := len(cm.lines) - cm.rows
	if maxTop < 0 {
		maxTop = 0
	}
	if cm.top > maxTop {
		cm.top = maxTop
	}
	if cm.top < 0 {
		cm.top = 0
	}
}

// inSelection reports whether (y, x) falls within the selection between the
// anchor and the cursor, in either order.
func (cm *copyMode) inSelection(y, x int) bool {
	if cm.rectSel {
		ylo, yhi, xlo, xhi := cm.rectBounds()
		return y >= ylo && y <= yhi && x >= cm.snapLow(y, xlo) && x <= xhi
	}
	y0, x0, y1, x1 := cm.selY, cm.selX, cm.cy, cm.cx
	if y0 > y1 || (y0 == y1 && x0 > x1) {
		y0, x0, y1, x1 = y1, x1, y0, x0
	}
	if y < y0 || y > y1 {
		return false
	}
	if cm.lineSel {
		return true
	}
	if y0 == y1 {
		return x >= cm.snapLow(y0, x0) && x <= x1
	}
	if y == y0 {
		return x >= cm.snapLow(y0, x0)
	}
	if y == y1 {
		return x <= x1
	}
	return true
}

// selectedText renders the current selection as plain text. Indices are cell
// (rune) positions; lineRunes keeps them aligned so a multi-byte char never
// splits mid-selection.
// rectBounds normalizes the rectangle selection to (ylo,yhi,xlo,xhi). Unlike
// the linear selection, columns come straight from anchor/cursor, not from a
// lexical (y,x) ordering.
func (cm *copyMode) rectBounds() (ylo, yhi, xlo, xhi int) {
	ylo, yhi = cm.selY, cm.cy
	if ylo > yhi {
		ylo, yhi = yhi, ylo
	}
	xlo, xhi = cm.selX, cm.cx
	if xlo > xhi {
		xlo, xhi = xhi, xlo
	}
	return
}

func (cm *copyMode) selectedText() string {
	if cm.rectSel {
		ylo, yhi, xlo, xhi := cm.rectBounds()
		var b strings.Builder
		for y := ylo; y <= yhi && y < len(cm.lines); y++ {
			runes := lineRunes(cm.lines[y])
			start, end := xlo, xhi+1
			if start > len(runes) {
				start = len(runes)
			}
			if end > len(runes) {
				end = len(runes)
			}
			if start < end {
				b.WriteString(cellText(cm.lines[y], runes, start, end))
			}
			if y != yhi {
				b.WriteRune('\n')
			}
		}
		return b.String()
	}
	y0, x0, y1, x1 := cm.selY, cm.selX, cm.cy, cm.cx
	if y0 > y1 || (y0 == y1 && x0 > x1) {
		y0, x0, y1, x1 = y1, x1, y0, x0
	}
	var b strings.Builder
	for y := y0; y <= y1 && y < len(cm.lines); y++ {
		runes := lineRunes(cm.lines[y])
		start, end := 0, len(runes)
		if !cm.lineSel {
			if y == y0 {
				start = x0
			}
			if y == y1 {
				end = x1 + 1
			}
		}
		if start > len(runes) {
			start = len(runes)
		}
		if end > len(runes) {
			end = len(runes)
		}
		if start < end {
			b.WriteString(cellText(cm.lines[y], runes, start, end))
		}
		if y != y1 {
			b.WriteRune('\n')
		}
	}
	return b.String()
}

// helpText is the copy-mode status line: position + key legend.
func (cm *copyMode) helpText() string {
	if cm.searching {
		return fmt.Sprintf("search: %s", string(cm.searchBuf))
	}
	return fmt.Sprintf("line %d/%d — hjkl move, v/C-v select, y yank, /? search, n/N next/prev, q quit", cm.cy+1, len(cm.lines))
}

// lineRunes renders a line as runes (one per cell), trailing blanks trimmed.
func lineRunes(line emu.Line) []rune {
	runes := make([]rune, len(line))
	for i, g := range line {
		if g.Char == 0 {
			runes[i] = ' '
		} else {
			runes[i] = g.Char
		}
	}
	end := len(runes)
	for end > 0 && runes[end-1] == ' ' {
		end--
	}
	return runes[:end]
}

// isWidePlaceholder reports whether cell i is the second half of a double-width
// rune. lineRunes emits a ' ' there to keep rune indices equal to cell indices —
// which every cursor position, selection bound and word motion relies on — but
// that space isn't real text, so anything LEAVING copy-mode (yanked text, search
// subjects) has to drop it or "🔨ab" comes out as "🔨 ab".
func isWidePlaceholder(line emu.Line, i int) bool {
	return i > 0 && i-1 < len(line) && line[i-1].Width() > 1
}

// snapLow moves a selection's LOW column edge off a wide rune's placeholder onto
// the rune itself. The cursor block renders there too (buildCopyRow snaps the
// same way), so a selection anchored on what looks like the rune must actually
// take it — otherwise the emoji you saw highlighted is missing from the yank.
// Applied wherever a low edge is consumed, so the highlight and the copied text
// always agree.
func (cm *copyMode) snapLow(y, x int) int {
	if y >= 0 && y < len(cm.lines) && isWidePlaceholder(cm.lines[y], x) {
		return x - 1
	}
	return x
}

// cellText renders the cell range [start,end) of a line as text, dropping the
// wide-rune placeholders. runes must be that line's lineRunes output. start is
// snapped like the highlight's low edge (see snapLow) so a range beginning on a
// placeholder still yields the rune it belongs to.
func cellText(line emu.Line, runes []rune, start, end int) string {
	if isWidePlaceholder(line, start) {
		start--
	}
	var b strings.Builder
	for i := start; i < end && i < len(runes); i++ {
		if isWidePlaceholder(line, i) {
			continue
		}
		b.WriteRune(runes[i])
	}
	return b.String()
}

// lineSearchText is a line's text with wide-rune placeholders dropped, plus the
// cell index each text rune came from — so a match found in the text maps back to
// a cursor position in cell space.
func lineSearchText(line emu.Line) ([]rune, []int) {
	runes := lineRunes(line)
	text := make([]rune, 0, len(runes))
	cells := make([]int, 0, len(runes))
	for i, r := range runes {
		if isWidePlaceholder(line, i) {
			continue
		}
		text = append(text, r)
		cells = append(cells, i)
	}
	return text, cells
}

// Word motions (w/b/e): a word is a maximal run of non-boundary chars. A
// boundary is whitespace or any char in word-separators (tmux's default is all
// ASCII punctuation, so foo.bar is three words); separator runs are skipped,
// not landed on — matching tmux's next-word.
func (cm *copyMode) isBoundary(r rune) bool {
	return r == ' ' || r == '\t' || strings.ContainsRune(cm.wordSep, r)
}

// takeCount returns the pending numeric prefix (default 1) and clears it.
func (cm *copyMode) takeCount() int {
	n := cm.count
	cm.count = 0
	if n < 1 {
		return 1
	}
	return n
}

// charMotion moves within the current line to target per the vi find operator:
// f=onto next, t=just-before next, F=onto prev, T=just-after prev. No-op if the
// target isn't on the line. ponytail: target matched by rune, ASCII in practice.
func (cm *copyMode) charMotion(op byte, target rune) {
	// Search the raw padded line, matching the cursor's coordinate space (clamp
	// and `$` index cm.lines[cm.cy] directly) — lineRunes trims trailing blanks,
	// which would put cx past the trimmed slice and panic.
	line := cm.lines[cm.cy]
	switch op {
	case 'f', 't':
		for x := cm.cx + 1; x < len(line); x++ {
			if line[x].Char == target {
				cm.cx = x
				if op == 't' {
					cm.cx--
				}
				return
			}
		}
	case 'F', 'T':
		for x := cm.cx - 1; x >= 0 && x < len(line); x-- {
			if line[x].Char == target {
				cm.cx = x
				if op == 'T' {
					cm.cx++
				}
				return
			}
		}
	}
}

func (cm *copyMode) wordForward() {
	runes := lineRunes(cm.lines[cm.cy])
	x := cm.cx
	for x < len(runes) && !cm.isBoundary(runes[x]) {
		x++
	}
	for {
		for x < len(runes) && cm.isBoundary(runes[x]) {
			x++
		}
		if x < len(runes) {
			cm.cx = x
			return
		}
		if cm.cy >= len(cm.lines)-1 {
			return
		}
		cm.cy++
		runes = lineRunes(cm.lines[cm.cy])
		x = 0
	}
}

func (cm *copyMode) wordBack() {
	runes := lineRunes(cm.lines[cm.cy])
	x := cm.cx - 1
	for {
		for x >= 0 && x < len(runes) && cm.isBoundary(runes[x]) {
			x--
		}
		if x >= len(runes) {
			x = len(runes) - 1
			continue
		}
		if x >= 0 {
			for x > 0 && !cm.isBoundary(runes[x-1]) {
				x--
			}
			cm.cx = x
			return
		}
		if cm.cy == 0 {
			return
		}
		cm.cy--
		runes = lineRunes(cm.lines[cm.cy])
		x = len(runes) - 1
	}
}

func (cm *copyMode) wordEnd() {
	runes := lineRunes(cm.lines[cm.cy])
	x := cm.cx + 1
	for {
		for x < len(runes) && cm.isBoundary(runes[x]) {
			x++
		}
		if x < len(runes) {
			for x+1 < len(runes) && !cm.isBoundary(runes[x+1]) {
				x++
			}
			cm.cx = x
			return
		}
		if cm.cy >= len(cm.lines)-1 {
			return
		}
		cm.cy++
		runes = lineRunes(cm.lines[cm.cy])
		x = 0
	}
}

// runeIndex is strings.Index for rune slices, so match positions stay
// column-aligned instead of byte-offset.
func runeIndex(haystack, needle []rune) int {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return -1
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// runSearch finds every occurrence of query and jumps to the first at or after
// the cursor.
func (cm *copyMode) runSearch(query string) {
	cm.matches = nil
	cm.lastSearch = query
	if query == "" {
		return
	}
	q := []rune(query)
	for y, line := range cm.lines {
		// Search the line's TEXT (placeholders dropped), then map each hit back
		// to its cell index — matches are cursor positions, which are cell-based.
		text, cells := lineSearchText(line)
		start := 0
		for start <= len(text) {
			idx := runeIndex(text[start:], q)
			if idx < 0 {
				break
			}
			cm.matches = append(cm.matches, [2]int{y, cells[start+idx]})
			start += idx + 1
		}
	}
	if len(cm.matches) == 0 {
		return
	}
	if cm.searchFwd {
		for i, m := range cm.matches {
			if m[0] > cm.cy || (m[0] == cm.cy && m[1] > cm.cx) {
				cm.matchIdx = i
				cm.cy, cm.cx = m[0], m[1]
				return
			}
		}
		cm.matchIdx = 0 // wrap to top
	} else {
		for i := len(cm.matches) - 1; i >= 0; i-- {
			m := cm.matches[i]
			if m[0] < cm.cy || (m[0] == cm.cy && m[1] < cm.cx) {
				cm.matchIdx = i
				cm.cy, cm.cx = m[0], m[1]
				return
			}
		}
		cm.matchIdx = len(cm.matches) - 1 // wrap to bottom
	}
	cm.cy, cm.cx = cm.matches[cm.matchIdx][0], cm.matches[cm.matchIdx][1]
}

func (cm *copyMode) jumpMatch(forward bool) {
	if len(cm.matches) == 0 {
		return
	}
	if forward {
		cm.matchIdx = (cm.matchIdx + 1) % len(cm.matches)
	} else {
		cm.matchIdx = (cm.matchIdx - 1 + len(cm.matches)) % len(cm.matches)
	}
	m := cm.matches[cm.matchIdx]
	cm.cy, cm.cx = m[0], m[1]
}

// encodeOSC52 wraps text as an OSC 52 set-clipboard escape, written straight to
// the client's own terminal on yank.
func encodeOSC52(text string) []byte {
	return []byte(fmt.Sprintf("\x1b]52;c;%s\x07", base64.StdEncoding.EncodeToString([]byte(text))))
}

// copyResult is what handleByte reports back to the input loop.
type copyResult struct {
	exit bool
	yank string // non-empty means: set as paste buffer + OSC52 to the terminal
}

// handleByte drives one keystroke of copy-mode over the frozen buffer,
// returning whether copy-mode should exit (and any yanked text). Mirrors the
// server's old handleCopyModeInput.
// emacsCtrl maps emacs copy-mode control keys onto the vi action byte the main
// switch already handles. Translate-first-then-fall-through: unmapped bytes
// still reach the vi switch (harmless — no vi key does the opposite of an emacs
// one), and translating first resolves the collisions (C-v→page-down before it
// can hit vi's rectangle case). 'R' is emacs's rectangle-toggle.
var emacsCtrl = map[byte]byte{
	0x02: 'h',  // C-b  left
	0x06: 'l',  // C-f  right
	0x10: 'k',  // C-p  up
	0x0e: 'j',  // C-n  down
	0x01: '0',  // C-a  line start
	0x05: '$',  // C-e  line end
	0x16: 0x06, // C-v  page down
	0x13: '/',  // C-s  search forward
	0x12: '?',  // C-r  search backward
	0x00: 'v',  // C-Space  begin selection
	0x07: 'q',  // C-g  cancel/exit
	'R':  0x16, // rectangle-toggle
}

// emacsMeta maps emacs Meta (ESC-prefixed) keys onto vi action bytes; consulted
// by feed() when it sees ESC+letter in emacs mode.
var emacsMeta = map[byte]byte{
	'f': 'w',  // M-f  word forward
	'b': 'b',  // M-b  word back
	'w': 'y',  // M-w  copy selection
	'<': 'g',  // M-<  top
	'>': 'G',  // M->  bottom
	'v': 0x02, // M-v  page up
}

func (cm *copyMode) handleByte(b byte) copyResult {
	if cm.emacs && !cm.searching {
		if v, ok := emacsCtrl[b]; ok {
			b = v
		}
	}
	return cm.dispatch(b)
}

// dispatch runs one vi action byte over the buffer. It does no emacs
// translation, so the Meta path (feed) can hand it a vi action byte that
// happens to collide with an emacs control key (M-v→page-up 0x02, which
// emacsCtrl would otherwise re-map to 'h').
func (cm *copyMode) dispatch(b byte) copyResult {
	if cm.searching {
		switch b {
		case '\r', '\n':
			cm.searching = false
			cm.runSearch(string(cm.searchBuf))
			cm.clamp()
			cm.scroll()
		case 0x1b:
			cm.searching = false
		case 0x7f, 0x08:
			if len(cm.searchBuf) > 0 {
				cm.searchBuf = cm.searchBuf[:len(cm.searchBuf)-1]
			}
		default:
			if b >= 0x20 && b < 0x7f {
				cm.searchBuf = append(cm.searchBuf, b)
			}
		}
		return copyResult{}
	}
	// Pending f/F/t/T: this keystroke is the search target (count applies here).
	if cm.pending != 0 {
		op := cm.pending
		cm.pending = 0
		for n := cm.takeCount(); n > 0; n-- {
			cm.charMotion(op, rune(b))
		}
		cm.clamp()
		cm.scroll()
		return copyResult{}
	}
	// Numeric prefix: 1-9 always builds a count; 0 extends only a count already
	// building (a lone 0 stays line-start).
	if (b >= '1' && b <= '9') || (b == '0' && cm.count > 0) {
		cm.count = cm.count*10 + int(b-'0')
		return copyResult{}
	}
	// Find operators defer to their target keystroke; leave count intact for it.
	switch b {
	case 'f', 'F', 't', 'T':
		cm.pending = b
		return copyResult{}
	}
	hadCount := cm.count > 0
	n := cm.takeCount()
	switch b {
	case 'q', 0x1b:
		return copyResult{exit: true}
	case 'h':
		cm.cx -= n
	case 'l':
		cm.cx += n
	case 'j':
		cm.cy += n
	case 'k':
		cm.cy -= n
	case '0':
		cm.cx = 0
	case '$':
		// End of line is the last NON-BLANK cell, not the grid width: a terminal
		// line is space-padded to the pane width, so len(line)-1 parked the cursor
		// far right in the padding and a visual selection swept up a tail of
		// spaces. lineRunes already trims that padding (it's what the yank uses).
		cm.cx = len(lineRunes(cm.lines[cm.cy])) - 1
		if cm.cx < 0 {
			cm.cx = 0
		}
	case 'g':
		cm.cy = 0
	case 'G':
		if hadCount { // count G = go to that (1-based) line, like vi
			cm.cy = n - 1
		} else {
			cm.cy = len(cm.lines) - 1
		}
	case 0x04: // Ctrl-d
		cm.cy += cm.rows / 2
	case 0x15: // Ctrl-u
		cm.cy -= cm.rows / 2
	case 0x06: // Ctrl-f, full page down
		cm.cy += cm.rows
	case 0x02: // Ctrl-b, full page up
		cm.cy -= cm.rows
	case 'w':
		for i := 0; i < n; i++ {
			cm.wordForward()
		}
	case 'b':
		for i := 0; i < n; i++ {
			cm.wordBack()
		}
	case 'e':
		for i := 0; i < n; i++ {
			cm.wordEnd()
		}
	case 'v', 'V':
		sameKind := cm.lineSel == (b == 'V')
		if cm.selecting && sameKind {
			cm.selecting = false
		} else {
			if !cm.selecting {
				cm.selY, cm.selX = cm.cy, cm.cx
			}
			cm.selecting = true
			cm.lineSel = b == 'V'
			cm.rectSel = false
		}
	case 'y':
		if cm.selecting {
			return copyResult{exit: true, yank: cm.selectedText()}
		}
		return copyResult{exit: true}
	case '/':
		cm.searching = true
		cm.searchFwd = true
		cm.searchBuf = nil
	case '?':
		cm.searching = true
		cm.searchFwd = false
		cm.searchBuf = nil
	case 'n':
		cm.jumpMatch(cm.searchFwd)
	case 'N':
		cm.jumpMatch(!cm.searchFwd)
	case 0x16: // Ctrl-v: rectangle-toggle
		if !cm.selecting {
			cm.selY, cm.selX = cm.cy, cm.cx
			cm.selecting = true
		}
		cm.rectSel = !cm.rectSel
		cm.lineSel = false
	}
	cm.clamp()
	cm.scroll()
	return copyResult{}
}

// feed processes a chunk of stdin bytes in copy-mode, recognizing inline CSI
// (arrows/PgUp/PgDn) the way the server's input loop used to. Mouse sequences
// are already stripped upstream by the mouse parser, so an ESC[ here is a
// cursor/page key. Stops at the first keystroke that exits copy-mode.
func (cm *copyMode) feed(data []byte) copyResult {
	for i := 0; i < len(data); i++ {
		b := data[i]
		if b == 0x1b && !cm.searching && i+1 < len(data) && data[i+1] == '[' {
			j := i + 2
			for j < len(data) && (data[j] < 0x40 || data[j] > 0x7e) {
				j++
			}
			if j < len(data) {
				res := cm.handleCSI(string(data[i+2 : j+1]))
				i = j
				if res.exit {
					return res
				}
				continue
			}
		}
		// Emacs Meta key: ESC + letter (not ESC[, which is a CSI above). Only in
		// emacs mode; in vi a lone ESC still means exit.
		if cm.emacs && b == 0x1b && !cm.searching && i+1 < len(data) {
			if v, ok := emacsMeta[data[i+1]]; ok {
				i++
				if res := cm.dispatch(v); res.exit {
					return res
				}
				continue
			}
		}
		if res := cm.handleByte(b); res.exit {
			return res
		}
	}
	return copyResult{}
}

// handleCSI translates an arrow/PgUp/PgDn escape typed in copy-mode into its vi
// equivalent. Unknown sequences are dropped.
func (cm *copyMode) handleCSI(seq string) copyResult {
	if cm.searching {
		return copyResult{}
	}
	switch seq {
	case "A":
		return cm.handleByte('k')
	case "B":
		return cm.handleByte('j')
	case "C":
		return cm.handleByte('l')
	case "D":
		return cm.handleByte('h')
	case "5~": // PgUp
		return cm.handleByte(0x02)
	case "6~": // PgDn
		return cm.handleByte(0x06)
	}
	return copyResult{}
}
