package client

import (
	"bytes"
	"strings"
	"testing"

	"github.com/FyrmForge/gtmux/internal/config"
	"github.com/FyrmForge/gtmux/internal/emu"
	"github.com/FyrmForge/gtmux/internal/proto"
)

func lineOf(s string) emu.Line {
	line := make(emu.Line, len(s))
	for i, r := range s {
		line[i] = emu.Glyph{Char: r}
	}
	return line
}

// TestCompositorPlacesTwoPanes verifies the client recombines two panes'
// content plus a vertical divider into one physical row — the thing that
// used to be the server's buildRow, now done here from Layout+PaneContent
// alone.
func TestCompositorPlacesTwoPanes(t *testing.T) {
	c := newCompositor()
	msg := &proto.ServerMsg{
		Layout: &proto.Layout{
			Cols: 7, Rows: 1,
			Panes: []proto.PaneRect{
				{ID: 1, Row: 0, Col: 0, Rows: 1, Cols: 3, Active: true},
				{ID: 2, Row: 0, Col: 4, Rows: 1, Cols: 3},
			},
			Borders: []proto.BorderSeg{{Vertical: true, Fixed: 3, Start: 0, End: 1}},
		},
		PaneContent: []proto.PaneContent{
			{PaneID: 1, Lines: map[int]emu.Line{0: lineOf("abc")}, CursorVisible: true},
			{PaneID: 2, Lines: map[int]emu.Line{0: lineOf("xyz")}},
		},
		Status: &proto.StatusInfo{},
	}
	c.apply(msg)

	row := c.buildRow(0)
	got := make([]rune, len(row))
	for i, g := range row {
		if g.Char == 0 {
			got[i] = ' '
		} else {
			got[i] = g.Char
		}
	}
	want := "abc│xyz"
	if string(got) != want {
		t.Errorf("buildRow(0) = %q, want %q", string(got), want)
	}
}

// TestCompositorActiveBorderPerCell proves the active-border highlight is
// per-cell: a full-height divider is shared by two stacked right panes, but
// only the active (top-right) pane's own edge lights — the rest stays grey.
func TestCompositorActiveBorderPerCell(t *testing.T) {
	c := newCompositor()
	c.apply(&proto.ServerMsg{
		Layout: &proto.Layout{
			Cols: 7, Rows: 4,
			Panes: []proto.PaneRect{
				{ID: 1, Row: 0, Col: 0, Rows: 4, Cols: 3},               // left, full height
				{ID: 2, Row: 0, Col: 4, Rows: 2, Cols: 3, Active: true}, // top-right (active)
				{ID: 3, Row: 2, Col: 4, Rows: 2, Cols: 3},               // bottom-right
			},
			Borders: []proto.BorderSeg{{Vertical: true, Fixed: 3, Start: 0, End: 4}},
		},
		Status: &proto.StatusInfo{},
	})

	active := c.cfg.ActiveBorderFG
	// Rows 0-1: divider is the active pane's left edge -> active color.
	for _, r := range []int{0, 1} {
		if fg := c.buildRow(r)[3].FG; fg != active {
			t.Errorf("row %d divider FG = %v, want active %v", r, fg, active)
		}
	}
	// Row 3: below the active pane (not its edge or corner) -> stays grey.
	if fg := c.buildRow(3)[3].FG; fg == active {
		t.Errorf("row 3 divider should not be active-colored, got %v", fg)
	}
}

func rowText(row emu.Line) string {
	got := make([]rune, len(row))
	for i, g := range row {
		if g.Char == 0 {
			got[i] = ' '
		} else {
			got[i] = g.Char
		}
	}
	return string(got)
}

// TestCompositorMultiLineStatus verifies tmux `status` N reserves N rows: the
// main bar sits at the screen edge and the extra lines render their own
// expanded formats, stacking inward.
func TestCompositorMultiLineStatus(t *testing.T) {
	c := newCompositor()
	c.cfg.StatusLines = 2
	c.cfg.ExtraStatusFormats[0] = "LINE2"
	c.apply(&proto.ServerMsg{
		Layout: &proto.Layout{
			Cols: 10, Rows: 3,
			Panes: []proto.PaneRect{{ID: 1, Row: 0, Col: 0, Rows: 3, Cols: 10, Active: true}},
		},
		Status: &proto.StatusInfo{},
	})
	// totalRows = 3 content + 2 status = 5. Bottom bar: row 4 = main, row 3 = line 2.
	if got := c.totalRows(); got != 5 {
		t.Fatalf("totalRows = %d, want 5", got)
	}
	if is, ex := c.statusRowKind(4); !is || ex != -1 {
		t.Errorf("row 4 = (%v,%d), want main bar (true,-1)", is, ex)
	}
	if is, ex := c.statusRowKind(3); !is || ex != 0 {
		t.Errorf("row 3 = (%v,%d), want extra line (true,0)", is, ex)
	}
	if is, _ := c.statusRowKind(2); is {
		t.Errorf("row 2 should be a window row, not status")
	}
	if got := rowText(c.buildRow(3)); !strings.HasPrefix(got, "LINE2") {
		t.Errorf("extra status row = %q, want it to start with LINE2", got)
	}
}

// TestCompositorPaneBorderStyle verifies pane-border-style colors the INACTIVE
// dividers (fg/bg/attr) while the active pane's edge keeps active_border_fg.
func TestCompositorPaneBorderStyle(t *testing.T) {
	c := newCompositor()
	c.cfg.InactiveBorderFG = emu.Red
	c.cfg.InactiveBorderBG = emu.Blue
	c.apply(&proto.ServerMsg{
		Layout: &proto.Layout{
			Cols: 7, Rows: 4,
			Panes: []proto.PaneRect{
				{ID: 1, Row: 0, Col: 0, Rows: 4, Cols: 3},
				{ID: 2, Row: 0, Col: 4, Rows: 2, Cols: 3, Active: true},
				{ID: 3, Row: 2, Col: 4, Rows: 2, Cols: 3},
			},
			Borders: []proto.BorderSeg{{Vertical: true, Fixed: 3, Start: 0, End: 4}},
		},
		Status: &proto.StatusInfo{},
	})
	// Row 3: not the active pane's edge -> inactive style (red on blue).
	if g := c.buildRow(3)[3]; g.FG != emu.Red || g.BG != emu.Blue {
		t.Errorf("inactive divider = FG %v/BG %v, want red/blue", g.FG, g.BG)
	}
	// Row 0: active pane's left edge -> keeps active_border_fg, not the style.
	if g := c.buildRow(0)[3]; g.FG != c.cfg.ActiveBorderFG {
		t.Errorf("active divider FG = %v, want active %v", g.FG, c.cfg.ActiveBorderFG)
	}
}

// TestCompositorCopyModeCursorHighlight verifies the client owns copy-mode: a
// CopyModeEnter snapshot makes the compositor render the frozen buffer and
// paint the cursor cell itself.
func TestCompositorCopyModeCursorHighlight(t *testing.T) {
	c := newCompositor()
	c.apply(&proto.ServerMsg{
		Layout: &proto.Layout{Cols: 5, Rows: 1, Panes: []proto.PaneRect{
			{ID: 1, Row: 0, Col: 0, Rows: 1, Cols: 5, Active: true},
		}},
		Status: &proto.StatusInfo{},
	})
	// Enter copy-mode over one line, cursor at column 2 ('l').
	c.apply(&proto.ServerMsg{CopyModeEnter: &proto.CopyModeEnter{
		PaneID: 1, Lines: []emu.Line{lineOf("hello")}, CursorY: 0, CursorX: 2,
	}})

	row := c.buildRow(0)
	for i, g := range row {
		highlighted := g.FG == emu.Black && g.BG == emu.Yellow
		if i == 2 && !highlighted {
			t.Errorf("cell %d should be the highlighted copy-mode cursor, got FG=%v BG=%v", i, g.FG, g.BG)
		}
		if i != 2 && highlighted {
			t.Errorf("cell %d should not be highlighted", i)
		}
	}
}

// TestCompositorMarkedBorder verifies a marked pane's divider is drawn in the
// marked-border color rather than the default divider color.
func TestCompositorMarkedBorder(t *testing.T) {
	c := newCompositor()
	msg := &proto.ServerMsg{
		Layout: &proto.Layout{
			Cols: 7, Rows: 1,
			Panes: []proto.PaneRect{
				{ID: 1, Row: 0, Col: 0, Rows: 1, Cols: 3, Active: true},
				{ID: 2, Row: 0, Col: 4, Rows: 1, Cols: 3, Marked: true},
			},
			Borders: []proto.BorderSeg{{Vertical: true, Fixed: 3, Start: 0, End: 1}},
		},
		PaneContent: []proto.PaneContent{
			{PaneID: 1, Lines: map[int]emu.Line{0: lineOf("abc")}},
			{PaneID: 2, Lines: map[int]emu.Line{0: lineOf("xyz")}},
		},
		Status: &proto.StatusInfo{},
	}
	c.apply(msg)

	if got := c.buildRow(0)[3].FG; got != c.cfg.MarkedBorderFG {
		t.Errorf("divider next to marked pane: FG=%v, want MarkedBorderFG=%v", got, c.cfg.MarkedBorderFG)
	}
}

// TestCompositorDotFillsLargerClient verifies a client whose terminal is
// bigger than the window it displays dot-fills the slack columns/rows and
// pins the status bar to its own physical bottom row.
func TestCompositorDotFillsLargerClient(t *testing.T) {
	c := newCompositor()
	c.setPhysical(6, 3) // terminal bigger than the 3x1 window
	msg := &proto.ServerMsg{
		Layout: &proto.Layout{Cols: 3, Rows: 1, Panes: []proto.PaneRect{
			{ID: 1, Row: 0, Col: 0, Rows: 1, Cols: 3, Active: true},
		}},
		PaneContent: []proto.PaneContent{
			{PaneID: 1, Lines: map[int]emu.Line{0: lineOf("abc")}},
		},
		Status: &proto.StatusInfo{},
	}
	c.apply(msg)

	row0 := c.buildRow(0)
	if len(row0) != 6 {
		t.Fatalf("row0 width = %d, want 6", len(row0))
	}
	for i, want := range []rune{'a', 'b', 'c', '·', '·', '·'} {
		if row0[i].Char != want {
			t.Errorf("row0[%d] = %q, want %q", i, row0[i].Char, want)
		}
	}
	for i, g := range c.buildRow(1) { // below the 1-row window: all dots
		if g.Char != '·' {
			t.Errorf("row1[%d] = %q, want dot", i, g.Char)
		}
	}
	if c.statusRow() != 2 { // physical bottom row
		t.Errorf("statusRow = %d, want 2", c.statusRow())
	}
}

// TestCompositorClipsSmallerClient verifies a client whose terminal is smaller
// than the window clips content to its own width with no overflow.
func TestCompositorClipsSmallerClient(t *testing.T) {
	c := newCompositor()
	c.setPhysical(3, 2) // terminal smaller than the 5x3 window
	msg := &proto.ServerMsg{
		Layout: &proto.Layout{Cols: 5, Rows: 3, Panes: []proto.PaneRect{
			{ID: 1, Row: 0, Col: 0, Rows: 3, Cols: 5, Active: true},
		}},
		PaneContent: []proto.PaneContent{
			{PaneID: 1, Lines: map[int]emu.Line{0: lineOf("hello")}},
		},
		Status: &proto.StatusInfo{},
	}
	c.apply(msg)

	row0 := c.buildRow(0)
	if len(row0) != 3 {
		t.Fatalf("row0 width = %d, want 3 (clipped)", len(row0))
	}
	for i, want := range []rune{'h', 'e', 'l'} {
		if row0[i].Char != want {
			t.Errorf("row0[%d] = %q, want %q", i, row0[i].Char, want)
		}
	}
}

// TestResolveMouseStatusClick verifies a click on a window label in the status
// row resolves to a select-window action, using the same label widths the
// client renders. status_left expands to "[h][s]" (6 cols), then " 1:a" (win
// 1), " 2:b*" (win 2).
func TestResolveMouseStatusClick(t *testing.T) {
	c := newCompositor()
	c.cfg.StatusLeft = "[#{host}][#{session}]" // expands to "[h][s]" below
	c.cfg.StatusRight = ""
	c.apply(&proto.ServerMsg{
		Layout: &proto.Layout{Cols: 20, Rows: 1, Panes: []proto.PaneRect{{ID: 1, Rows: 1, Cols: 20, Active: true}}},
		Status: &proto.StatusInfo{Vars: map[string]string{"host": "h", "session": "s"}, Windows: []proto.WindowInfo{
			{Index: 1, Name: "a"}, {Index: 2, Name: "b", Active: true},
		}},
	})
	sr := c.statusRow()
	// col 8 falls in "1:a" (cols 7..9); col 12 in "2:b*" (cols 11..14).
	cases := []struct {
		col  int
		want string
	}{{8, "1"}, {12, "2"}}
	for _, tc := range cases {
		got := c.resolveMouse(proto.MouseEvent{Cb: 0, X: tc.col + 1, Y: sr + 1, Press: true})
		if len(got) != 2 || got[0] != "select-window" || got[1] != tc.want {
			t.Errorf("click col %d = %v, want [select-window %s]", tc.col, got, tc.want)
		}
	}
	// A click off any label forwards (nil).
	if got := c.resolveMouse(proto.MouseEvent{Cb: 0, X: 1, Y: sr + 1, Press: true}); got != nil {
		t.Errorf("click on Left prefix = %v, want nil (forward)", got)
	}
}

// TestCopyMouseDragSelect verifies a mouse press-drag-release in copy-mode
// anchors, extends, and yanks the selection mapped through the pane rect.
func TestCopyMouseDragSelect(t *testing.T) {
	c := newCompositor()
	c.apply(&proto.ServerMsg{
		Layout: &proto.Layout{Cols: 5, Rows: 3, Panes: []proto.PaneRect{{ID: 1, Rows: 3, Cols: 5, Active: true}}},
		Status: &proto.StatusInfo{},
	})
	c.apply(&proto.ServerMsg{CopyModeEnter: &proto.CopyModeEnter{
		PaneID: 1, Lines: []emu.Line{lineOf("hello"), lineOf("world"), lineOf("abcde")},
	}})

	// Press at buffer (1,1), drag to (2,3), release — SGR: press cb=0,
	// motion cb=0x20, release press=false.
	c.copyMouse(proto.MouseEvent{Cb: 0, X: 2, Y: 2, Press: true})
	c.copyMouse(proto.MouseEvent{Cb: 0x20, X: 4, Y: 3, Press: true})
	_, res := c.copyMouse(proto.MouseEvent{Cb: 0, X: 4, Y: 3, Press: false})

	if !res.exit {
		t.Fatal("release should exit copy-mode with a yank")
	}
	if res.yank != "orld\nabcd" {
		t.Errorf("yank = %q, want %q", res.yank, "orld\nabcd")
	}
}

func TestCompositorStatusRow(t *testing.T) {
	c := newCompositor()
	msg := &proto.ServerMsg{
		Layout:      &proto.Layout{Cols: 10, Rows: 2},
		PaneContent: nil,
		Status:      &proto.StatusInfo{},
	}
	c.apply(msg)
	if c.statusRow() != 2 {
		t.Fatalf("statusRow() = %d, want 2", c.statusRow())
	}
	row := c.buildRow(2)
	if len(row) != 10 {
		t.Errorf("status row length = %d, want 10", len(row))
	}
}

// TestCompositorStatusJustify verifies right-justify positions the window list
// against the right edge and records matching click spans in windowHits.
func TestCompositorStatusJustify(t *testing.T) {
	c := newCompositor()
	c.cfg = config.DefaultClientConfig()
	c.cfg.StatusJustify = "right"
	c.cfg.WindowStatusFormat = "#{window_index}"
	c.cfg.WindowStatusCurrentFormat = "#{window_index}"
	c.phyCols, c.phyRows = 10, 2 // 10-wide bar, no left/right formats
	c.cfg.StatusLeft, c.cfg.StatusRight = "", ""
	msg := &proto.ServerMsg{
		Layout: &proto.Layout{Cols: 10, Rows: 1},
		Status: &proto.StatusInfo{Windows: []proto.WindowInfo{{Index: 1}, {Index: 2, Active: true}}},
	}
	c.stLeft, c.stRight = "", ""
	c.apply(msg)

	// list is "1 2" (separator " "), 3 wide, right-justified in 10 cols → cols 7..9.
	line := c.renderBar()
	got := make([]rune, len(line))
	for i, g := range line {
		got[i] = g.Char
	}
	if string(got) != "       1 2" {
		t.Errorf("right-justified bar = %q, want %q", string(got), "       1 2")
	}
	// windowHits must point at the actual label columns: '1' at 7, '2' at 9.
	if len(c.windowHits) != 2 || c.windowHits[0].start != 7 || c.windowHits[1].start != 9 {
		t.Errorf("windowHits = %+v, want entries starting at cols 7 and 9", c.windowHits)
	}
}

// TestCompositorSetTitles verifies set-titles: apply emits an OSC 0/2 title
// (expanded from set-titles-string) on the first status, pushes the title
// stack once, dedupes an unchanged title, and re-emits when it changes.
func TestCompositorSetTitles(t *testing.T) {
	c := newCompositor()
	c.cfg = config.ClientConfig{SetTitles: true, SetTitlesString: "#{session}:#{window_name}"}
	vars := map[string]string{"session": "dev", "window_name": "zsh"}

	out := c.apply(&proto.ServerMsg{Status: &proto.StatusInfo{Vars: vars}})
	if !bytes.Contains(out, []byte("\x1b[22;2t")) {
		t.Error("first set-titles apply must push the title stack (\\e[22;2t)")
	}
	if !bytes.Contains(out, []byte("\x1b]0;dev:zsh\a")) {
		t.Errorf("missing expanded title OSC; got %q", out)
	}

	// Unchanged title → no re-emit.
	out = c.apply(&proto.ServerMsg{Status: &proto.StatusInfo{Vars: vars}})
	if bytes.Contains(out, []byte("\x1b]0;")) {
		t.Errorf("unchanged title should not re-emit; got %q", out)
	}

	// Changed name → re-emit (no second stack push).
	out = c.apply(&proto.ServerMsg{Status: &proto.StatusInfo{Vars: map[string]string{"session": "dev", "window_name": "vim"}}})
	if !bytes.Contains(out, []byte("\x1b]0;dev:vim\a")) {
		t.Errorf("changed title should re-emit; got %q", out)
	}
	if bytes.Contains(out, []byte("\x1b[22;2t")) {
		t.Error("title stack must be pushed only once")
	}
	if r := c.restoreTitle(); !bytes.Equal(r, []byte("\x1b[23;2t")) {
		t.Errorf("restoreTitle() = %q, want pop \\e[23;2t", r)
	}
}

// status-left-length caps the status-left segment before the window list.
func TestCompositorStatusLeftLength(t *testing.T) {
	c := newCompositor()
	c.cfg = config.DefaultClientConfig()
	c.cfg.StatusLeft = "ABCDEFGH" // literal (no format vars): expands to itself
	c.cfg.StatusRight = ""
	c.cfg.StatusLeftLength = 4
	c.phyCols, c.phyRows = 40, 2
	msg := &proto.ServerMsg{
		Layout: &proto.Layout{Cols: 40, Rows: 1},
		Status: &proto.StatusInfo{},
	}
	c.apply(msg)
	line := c.renderBar()
	got := make([]rune, 5)
	for i := 0; i < 5; i++ {
		got[i] = line[i].Char
	}
	if string(got[:4]) != "ABCD" {
		t.Fatalf("status-left missing/wrong; first 4 = %q", string(got[:4]))
	}
	if got[4] == 'E' {
		t.Fatalf("status-left-length 4 did not truncate: cell 5 = %q", got[4])
	}
}

// #7: the picker shows the highlighted item's static preview below the list,
// and it follows the selection.
func TestPickerPreview(t *testing.T) {
	// mkPrev turns text into one styled preview line, coloring it red so we can
	// assert the preview keeps the pane's own colors (not the status style).
	mkPrev := func(s string) []emu.Line {
		l := make(emu.Line, len(s))
		for i, r := range s {
			l[i] = emu.Glyph{Char: r, FG: emu.Red}
		}
		return []emu.Line{l}
	}
	c := newCompositor()
	c.apply(&proto.ServerMsg{Layout: &proto.Layout{Cols: 40, Rows: 20}, Status: &proto.StatusInfo{}})
	c.apply(&proto.ServerMsg{OpenPicker: &proto.OpenPicker{
		Title: "choose session", Verb: "switch-session",
		Items:    []string{"alpha", "beta"},
		Targets:  []string{"alpha", "beta"},
		Previews: [][]emu.Line{mkPrev("PREVIEW_ALPHA"), mkPrev("PREVIEW_BETA")},
	}})
	screen := func() string {
		var b strings.Builder
		for r := 0; r < 20; r++ {
			b.WriteString(rowText(c.buildRow(r)))
			b.WriteByte('\n')
		}
		return b.String()
	}
	if s := screen(); !strings.Contains(s, "PREVIEW_ALPHA") || strings.Contains(s, "PREVIEW_BETA") {
		t.Fatalf("sel 0 should show ALPHA preview only:\n%s", s)
	}
	// The preview's own color (red) must survive into the composited grid.
	redSeen := false
	for r := 0; r < 20 && !redSeen; r++ {
		for _, g := range c.buildRow(r) {
			if g.Char == 'P' && g.FG == emu.Red {
				redSeen = true
				break
			}
		}
	}
	if !redSeen {
		t.Fatal("preview lost its color (no red glyph in the composited box)")
	}
	c.pickerFeed([]byte{'j'}) // move to beta
	if s := screen(); !strings.Contains(s, "PREVIEW_BETA") || strings.Contains(s, "PREVIEW_ALPHA") {
		t.Fatalf("sel 1 should show BETA preview only:\n%s", s)
	}
}

// #4: copy-drag-finish gates the release-yanks-and-exits behavior (tmux's
// MouseDragEnd1Pane). Off = selection persists (no exit/yank on release).
func TestCopyDragFinishOption(t *testing.T) {
	mkLines := func() []emu.Line {
		lines := make([]emu.Line, 10)
		for y := range lines {
			l := make(emu.Line, 20)
			for x := range l {
				l[x] = emu.Glyph{Char: 'x'}
			}
			lines[y] = l
		}
		return lines
	}
	drag := func(finish bool) copyResult {
		c := newCompositor()
		c.cfg.CopyDragFinish = finish
		c.apply(&proto.ServerMsg{
			Layout: &proto.Layout{Cols: 20, Rows: 11, Panes: []proto.PaneRect{{ID: 1, Row: 0, Col: 0, Rows: 10, Cols: 20, Active: true}}},
			Status: &proto.StatusInfo{},
		})
		c.apply(&proto.ServerMsg{CopyModeEnter: &proto.CopyModeEnter{PaneID: 1, Lines: mkLines()}})
		c.copyMouse(proto.MouseEvent{Cb: 0, X: 3, Y: 3, Press: true})  // anchor
		c.copyMouse(proto.MouseEvent{Cb: 0x20, X: 8, Y: 3, Press: true}) // drag
		_, res := c.copyMouse(proto.MouseEvent{Cb: 0, X: 8, Y: 3})       // release
		return res
	}
	if res := drag(true); !res.exit || res.yank == "" {
		t.Errorf("finish=true: want exit+yank, got %+v", res)
	}
	if res := drag(false); res.exit || res.yank != "" {
		t.Errorf("finish=false: want no exit/yank, got %+v", res)
	}
}
