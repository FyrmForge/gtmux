package harness

import (
	"strings"
	"time"

	"github.com/FyrmForge/gtmux/internal/emu"
)

// Text is one row (or the whole screen) as a string, with a Has convenience
// for substring assertions.
type Text string

func (t Text) Has(sub string) bool { return strings.Contains(string(t), sub) }
func (t Text) String() string      { return string(t) }

// Screen is an immutable snapshot of a client's rendered grid, safe to inspect
// without holding the client lock.
type Screen struct {
	rows, cols int
	cells      [][]emu.Glyph
}

// Screen snapshots the client's current rendered grid.
func (c *Client) Screen() *Screen {
	c.t.Helper()
	cells, err := c.be.snapshot()
	if err != nil {
		c.t.Fatalf("screen snapshot: %v", err)
	}
	return &Screen{rows: c.rows, cols: c.cols, cells: cells}
}

// Cell returns the glyph at (row, col), or an empty glyph if out of range.
func (s *Screen) Cell(row, col int) emu.Glyph {
	if row < 0 || row >= s.rows || col < 0 || col >= s.cols {
		return emu.EmptyGlyph()
	}
	return s.cells[row][col]
}

// Row returns one row's text (attributes stripped).
func (s *Screen) Row(row int) Text {
	if row < 0 || row >= s.rows {
		return ""
	}
	var b strings.Builder
	for _, g := range s.cells[row] {
		if g.Char == 0 {
			b.WriteByte(' ')
		} else {
			b.WriteRune(g.Char)
		}
	}
	return Text(strings.TrimRight(b.String(), " "))
}

// Status returns the bottom row — the gtmux status bar.
func (s *Screen) Status() Text { return s.Row(s.rows - 1) }

// Col returns the first column in row holding rune r, or -1 — e.g. locating a
// pane divider ('│') to check a resize moved it.
func (s *Screen) Col(row int, r rune) int {
	if row < 0 || row >= s.rows {
		return -1
	}
	for x, g := range s.cells[row] {
		if g.Char == r {
			return x
		}
	}
	return -1
}

// ActiveWindow returns the index of the highlighted window in the status bar,
// found by its active-window background color, or -1. Robust to the shell name
// (which varies by environment), unlike matching label text.
func (s *Screen) ActiveWindow() int {
	row := s.rows - 1
	if row < 0 {
		return -1
	}
	for x := 0; x < s.cols; x++ {
		g := s.cells[row][x]
		if g.BG == emu.Green && g.Char >= '1' && g.Char <= '9' {
			return int(g.Char - '0')
		}
	}
	return -1
}

// String renders the whole grid as text, one row per line — used in assertions
// and in WaitFor's failure dump.
func (s *Screen) String() string {
	rows := make([]string, s.rows)
	for y := 0; y < s.rows; y++ {
		rows[y] = string(s.Row(y))
	}
	return strings.Join(rows, "\n")
}

// WaitFor polls the screen until pred holds, then returns; on timeout it fails
// the test and dumps the last screen so the failure is debuggable.
func (c *Client) WaitFor(pred func(*Screen) bool) {
	c.t.Helper()
	c.WaitForUntil(DefaultTimeout, pred)
}

// WaitForUntil is WaitFor with an explicit timeout — for behavior that's
// deliberately slow, e.g. the 3s status-message auto-clear.
func (c *Client) WaitForUntil(timeout time.Duration, pred func(*Screen) bool) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred(c.Screen()) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.t.Fatalf("WaitFor timed out after %v; screen:\n%s", timeout, c.Screen().String())
}

// WaitForText waits until sub appears anywhere on screen.
func (c *Client) WaitForText(sub string) {
	c.t.Helper()
	c.WaitFor(func(s *Screen) bool { return strings.Contains(s.String(), sub) })
}

// WaitForStatus waits until sub appears in the status bar.
func (c *Client) WaitForStatus(sub string) {
	c.t.Helper()
	c.WaitFor(func(s *Screen) bool { return s.Status().Has(sub) })
}

// RawContains reports whether the given bytes have appeared anywhere in the
// client's cumulative raw output — for bytes that bypass the grid (passthrough).
// pty backend only.
func (c *Client) RawContains(sub []byte) bool {
	pb, ok := c.be.(*ptyBackend)
	if !ok {
		c.t.Fatal("RawContains: pty backend only")
	}
	return pb.rawContains(sub)
}

// WaitForRaw polls the raw output until sub appears (passthrough is async), or
// fails on timeout.
func (c *Client) WaitForRaw(sub []byte) {
	c.t.Helper()
	deadline := time.Now().Add(DefaultTimeout)
	for time.Now().Before(deadline) {
		if c.RawContains(sub) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.t.Fatalf("WaitForRaw timed out waiting for %q", sub)
}
