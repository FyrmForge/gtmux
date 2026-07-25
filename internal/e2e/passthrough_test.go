//go:build e2e

package e2e

import (
	"testing"
	"time"

	"github.com/FyrmForge/gtmux/internal/harness"
)

// The wire form an app emits: ESC P tmux ; <payload, ESCs doubled> ESC \.
// Payload = an OSC 52 (ESC ] 52 ; c ; <b64> ESC \), each ESC doubled.
const passSh = `printf '\033Ptmux;\033\033]52;c;R1RNVVg=\033\033\134\033\134'`

// After un-doubling, the client's terminal should receive exactly this (real ESC
// bytes). The command echo contains the literal ASCII "]52;c;R1RNVVg=", but never
// a real ESC byte in front of it — so this match is unambiguous.
var passForwarded = []byte("\x1b]52;c;R1RNVVg=")

// TestAllowPassthrough: with allow-passthrough on, an app's wrapped DCS payload
// is un-doubled and forwarded raw to the client terminal.
func TestAllowPassthrough(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("default")
	c.Run("run", "default", "set-option", "-g", "allow-passthrough", "on")

	c.TypeLine(passSh)
	c.WaitForRaw(passForwarded)
	// The wire form has doubled ESCs; if un-doubling were skipped, the forwarded
	// bytes would contain ESC ESC ] 52. A real ESC ESC only comes from a
	// non-un-doubled payload (the command echo is literal ASCII backslashes).
	if c.RawContains([]byte("\x1b\x1b]52;c;R1RNVVg=")) {
		t.Fatalf("payload forwarded without un-doubling ESCs")
	}
}

// wrap builds the wire form (ESC P tmux ; <doubled OSC 52 with b64 marker> ESC \)
// as a printf command, and the un-doubled forwarded bytes to look for.
func wrap(marker string) (sh string, forwarded []byte) {
	sh = `printf '\033Ptmux;\033\033]52;c;` + marker + `\033\033\134\033\134'`
	return sh, []byte("\x1b]52;c;" + marker)
}

// TestAllowPassthroughBackgroundDropped: passthrough is forwarded only for the
// pane the client actually sees. A pane in a background window emits it → dropped;
// the active window's pane emits it → forwarded (control).
func TestAllowPassthroughBackgroundDropped(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("default")
	c.Run("run", "default", "set-option", "-g", "allow-passthrough", "on")

	// Window 0's pane emits BKG only after a 2s delay. The delay's sole job is to
	// let new-window (below) background the pane first — once backgrounded, the
	// emit fires in the background regardless of later timing, so the assertion is
	// deterministic as long as new-window (tens of ms) beats the 2s timer.
	bgSh, bgSeq := wrap("QktH")   // "BKG"
	visSh, visSeq := wrap("VklT") // "VIS"
	c.TypeLine("sleep 2; " + bgSh)

	// New window becomes active; window 0 (and its pending emit) is now background.
	c.Run("run", "default", "new-window")
	c.TypeLine(visSh) // active pane emits immediately → visible → forwards (control)
	c.WaitForRaw(visSeq)

	// Let the backgrounded emit fire; it must not be forwarded.
	time.Sleep(2500 * time.Millisecond)
	if c.RawContains(bgSeq) {
		t.Fatalf("background-window passthrough was forwarded")
	}
}

// TestAllowPassthroughReadOnly: a read-only (attach -r) client observes only; an
// app's passthrough payload must reach the writable client but not the read-only
// one.
func TestAllowPassthroughReadOnly(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("default")
	c.Run("run", "default", "set-option", "-g", "allow-passthrough", "on")
	ro := c.NewPeerReadOnly(80, 24)
	ro.WaitForStatus("default")

	c.TypeLine(passSh)
	c.WaitForRaw(passForwarded) // writable client gets it
	// Both clients would be encoded in the same handleRender; give the read-only
	// conn time to deliver, then confirm it never did.
	time.Sleep(300 * time.Millisecond)
	if ro.RawContains(passForwarded) {
		t.Fatalf("passthrough forwarded to read-only client")
	}
}

// TestAllowPassthroughOff: with the option off (default), the wrapper is stripped
// and the payload dropped — nothing reaches the client terminal.
func TestAllowPassthroughOff(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("default")

	c.TypeLine(passSh)
	// Give the sequence time to round-trip; it must never be forwarded.
	time.Sleep(500 * time.Millisecond)
	if c.RawContains(passForwarded) {
		t.Fatalf("payload forwarded with allow-passthrough off")
	}
}

// TestOSC52SetClipboard: a bare OSC 52 from an app in a pane (no passthrough
// wrapper, no allow-passthrough needed) is decoded by the pane's emulator,
// re-emitted as OSC 52 to the client terminal, and lands in the paste buffer
// so prefix+] pastes it — the "yank in remote nvim → local clipboard" path.
func TestOSC52SetClipboard(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("default")

	c.TypeLine(`printf '\033]52;c;R1RNVVg=\033\134'`) // sets clipboard to "GTMUX"
	// The client re-encodes (BEL-terminated), so match the common prefix. The
	// command echo has the literal ASCII "]52;c;…" but never a real ESC before it.
	c.WaitForRaw([]byte("\x1b]52;c;R1RNVVg="))

	c.TypeLine("cat")
	c.Prefix("]") // paste buffer was set from the same OSC 52
	c.WaitForText("GTMUX")
}
