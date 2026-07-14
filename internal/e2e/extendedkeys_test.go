//go:build e2e

package e2e

import (
	"testing"
	"time"

	"github.com/FyrmForge/gtmux/internal/harness"
)

// An app enabling the kitty keyboard protocol writes CSI > flags u to its pty.
const kittyEnableSh = `printf '\033[>1u'`

// What the client must then send its outer terminal to match (real ESC bytes;
// the command echo shows the literal ASCII "\033[>1u", never a real ESC here).
var kittyEnableSeq = []byte("\x1b[>1u")

// TestExtendedKeysNegotiate: with extended-keys on, when a pane app enables the
// kitty keyboard protocol the client negotiates the same with its outer terminal
// (propagate PaneRect.KeyFlags → client emits CSI > 1 u to stdout).
func TestExtendedKeysNegotiate(t *testing.T) {
	c := harness.StartWithConfig(t, `gtmux.set_option("extended_keys", "on")`, "")
	c.WaitForStatus("default")

	c.TypeLine(kittyEnableSh)
	c.WaitForRaw(kittyEnableSeq)
}

// TestExtendedKeysRuntimeEnable: enabling extended-keys at runtime (after an app
// already turned kitty on) reconciles the outer terminal — the client must not
// stay legacy just because the app's enable happened before the option did.
func TestExtendedKeysRuntimeEnable(t *testing.T) {
	c := harness.Start(t) // extended-keys off by default
	c.WaitForStatus("default")

	// App enables kitty while the option is off → nothing negotiated yet.
	c.TypeLine(kittyEnableSh)
	time.Sleep(300 * time.Millisecond)
	if c.RawContains(kittyEnableSeq) {
		t.Fatalf("negotiated with extended-keys off")
	}

	// Toggle the option on at runtime; the client reconciles (reload hook and/or
	// the ensuing Layout). Without either, the outer terminal would stay legacy.
	c.Prefix(":")
	c.WaitForStatus("(:")
	c.TypeLine("set-option extended-keys on")
	c.WaitForRaw(kittyEnableSeq)
}

// TestExtendedKeysOff: with the option off (default), the client never negotiates
// with its outer terminal even when a pane app requests kitty.
func TestExtendedKeysOff(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("default")

	c.TypeLine(kittyEnableSh)
	time.Sleep(500 * time.Millisecond)
	if c.RawContains(kittyEnableSeq) {
		t.Fatalf("client negotiated kitty with extended-keys off")
	}
}
