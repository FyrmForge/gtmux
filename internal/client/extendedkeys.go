package client

import "fmt"

// kittyNegotiate returns the bytes to send the outer terminal to set our pushed
// kitty-keyboard flags to want, and the new pushed state. We keep at most one
// entry on the terminal's stack: always pop, then push the new flags if wanted.
// want==0 (or !enabled) means "no kitty" → just pop.
//
// Deliberately NOT a diff against old: the terminal's real stack can drift from
// what we remember (a client killed without its detach pop, a terminal that
// reset its own stack), and a diff would then skip the very re-push that fixes
// it — leaving a kitty pane receiving legacy keys (Esc/Ctrl/Enter dead, letters
// fine). Popping an empty stack is a no-op per the kitty spec, so re-asserting
// on every layout is safe and idempotent; the cost is a few bytes per focus
// change. ponytail: a stack entry pushed by something outside gtmux (client
// nested in another multiplexer) would get popped too — not a supported setup.
//
// This is what makes extended-keys work: while a pane's app speaks the kitty
// protocol, the outer terminal is told to speak it too, so its CSI-u keystrokes
// flow straight through the client (which forwards unrecognized input verbatim)
// to the app. When the active pane is legacy, the outer terminal stays legacy.
func kittyNegotiate(old, want int, enabled bool) (out []byte, state int) {
	if !enabled {
		want = 0
	}
	b := []byte("\x1b[<1u") // pop our entry (no-op if the stack is empty)
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
	// modifyOtherKeys: when extended-keys is on but the active pane isn't in
	// kitty mode, put the outer terminal in modifyOtherKeys=1 so gtmux still
	// receives modified keys (Ctrl+1, …) as CSI 27;mods;code~ for its own binds.
	// Mode 1 leaves Escape/Tab/Enter/plain-Ctrl untouched, so legacy panes are
	// unaffected. Mutually exclusive with kitty, which supersedes it.
	return append(out, c.negotiateMOK(c.cfg.ExtendedKeys && state == 0)...)
}

// negotiateMOK sets the outer terminal's xterm modifyOtherKeys=1 mode to `want`.
// Always emitted (the sequence is idempotent), for the same drift reason as
// kittyNegotiate.
func (c *compositor) negotiateMOK(want bool) []byte {
	c.mokActive = want
	if want {
		return []byte("\x1b[>4;1m")
	}
	return []byte("\x1b[>4;0m")
}

// restoreKitty pops our kitty entry from the outer terminal on detach, if any,
// and clears modifyOtherKeys.
func (c *compositor) restoreKitty() []byte {
	out, state := kittyNegotiate(c.kittyFlags, 0, true)
	c.kittyFlags = state
	return append(out, c.negotiateMOK(false)...)
}
