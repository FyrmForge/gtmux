package client

import "fmt"

// kittyNegotiate returns the bytes to send the outer terminal to move our pushed
// kitty-keyboard flags from old to want, and the new pushed state. We keep at
// most one entry on the terminal's stack: pop ours if we had one, push the new
// flags if wanted. want==0 (or !enabled) means "no kitty" → just pop.
//
// This is what makes extended-keys work: while a pane's app speaks the kitty
// protocol, the outer terminal is told to speak it too, so its CSI-u keystrokes
// flow straight through the client (which forwards unrecognized input verbatim)
// to the app. When the active pane is legacy, the outer terminal stays legacy.
func kittyNegotiate(old, want int, enabled bool) (out []byte, state int) {
	if !enabled {
		want = 0
	}
	if want == old {
		return nil, old
	}
	var b []byte
	if old > 0 {
		b = append(b, "\x1b[<1u"...) // pop our entry
	}
	if want > 0 {
		b = append(b, []byte(fmt.Sprintf("\x1b[>%du", want))...) // push new flags
	}
	return b, want
}

// negotiateKitty renegotiates against the active pane's KeyFlags in the current
// layout and returns any bytes to emit to the outer terminal. Updates state.
func (c *compositor) negotiateKitty() []byte {
	want := 0
	if c.layout != nil {
		for _, pr := range c.layout.Panes {
			if pr.Active {
				want = pr.KeyFlags
				break
			}
		}
	}
	out, state := kittyNegotiate(c.kittyFlags, want, c.cfg.ExtendedKeys)
	c.kittyFlags = state
	return out
}

// restoreKitty pops our kitty entry from the outer terminal on detach, if any.
func (c *compositor) restoreKitty() []byte {
	out, state := kittyNegotiate(c.kittyFlags, 0, true)
	c.kittyFlags = state
	return out
}
