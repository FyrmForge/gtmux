package harness

import (
	"fmt"
	"time"
)

// prefixByte is the default gtmux prefix, C-b. The harness assumes the default
// binding; a test that rebinds the prefix would drive raw bytes instead.
const prefixByte = 0x02 // Ctrl-b

// write sends raw bytes to the client's tty (its stdin), then pauses for
// -slowmo if set, so headed runs are watchable.
func (c *Client) write(b []byte) {
	c.t.Helper()
	if err := c.be.write(b); err != nil {
		c.t.Fatalf("write to client: %v", err)
	}
	time.Sleep(*slowMo)
}

// Type sends literal text to the focused pane.
func (c *Client) Type(s string) { c.write([]byte(s)) }

// TypeLine types s followed by Enter (carriage return, as a real terminal
// sends for the Return key in raw mode).
func (c *Client) TypeLine(s string) { c.write(append([]byte(s), '\r')) }

// Key sends a single byte.
func (c *Client) Key(b byte) { c.write([]byte{b}) }

// Ctrl sends a control key, e.g. Ctrl('c') -> 0x03.
func (c *Client) Ctrl(b byte) { c.write([]byte{b & 0x1f}) }

// Prefix sends the prefix key (C-b) followed by keys, e.g. Prefix("c") to
// create a window, Prefix("[") to enter copy-mode.
func (c *Client) Prefix(keys string) {
	c.write(append([]byte{prefixByte}, []byte(keys)...))
}

// arrowFinal maps a direction to the CSI final byte for its arrow key.
var arrowFinal = map[string]byte{"up": 'A', "down": 'B', "left": 'D', "right": 'C'}

func (c *Client) arrow(dir string) byte {
	f, ok := arrowFinal[dir]
	if !ok {
		c.t.Fatalf("bad arrow direction %q", dir)
	}
	return f
}

// PrefixArrow sends prefix then a bare arrow — gtmux's pane navigation.
func (c *Client) PrefixArrow(dir string) {
	c.write([]byte{prefixByte, 0x1b, '[', c.arrow(dir)})
}

// PrefixCtrlArrow sends prefix then Ctrl+arrow (CSI 1;5X) — resize by 1.
func (c *Client) PrefixCtrlArrow(dir string) {
	c.write(append([]byte{prefixByte}, []byte(fmt.Sprintf("\x1b[1;5%c", c.arrow(dir)))...))
}

// PrefixAltArrow sends prefix then Alt+arrow (CSI 1;3X) — resize by 5.
func (c *Client) PrefixAltArrow(dir string) {
	c.write(append([]byte{prefixByte}, []byte(fmt.Sprintf("\x1b[1;3%c", c.arrow(dir)))...))
}

// Click sends an SGR mouse press+release (button 0) at 1-based col,row — used
// for focus and status-bar clicks.
func (c *Client) Click(col, row int) {
	c.write([]byte(fmt.Sprintf("\x1b[<0;%d;%dM\x1b[<0;%d;%dm", col, row, col, row)))
}

// WheelUp sends an SGR wheel-up event (Cb 64) at 1-based col,row.
func (c *Client) WheelUp(col, row int) {
	c.write([]byte(fmt.Sprintf("\x1b[<64;%d;%dM", col, row)))
}

// Drag sends a left-button press at (c1,r1), a motion (Cb 32 = motion+button0)
// to (c2,r2), and a release — the SGR sequence for a border drag or a
// drag-to-copy gesture. Coordinates are 1-based.
func (c *Client) Drag(c1, r1, c2, r2 int) {
	c.write([]byte(fmt.Sprintf("\x1b[<0;%d;%dM\x1b[<32;%d;%dM\x1b[<0;%d;%dm", c1, r1, c2, r2, c2, r2)))
}
