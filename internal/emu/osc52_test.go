package emu

import "testing"

// An app's OSC 52 set-clipboard lands in Dirty.Clipboards decoded;
// TakeClipboards drains once.
func TestOSC52Clipboard(t *testing.T) {
	term := New()
	term.Write([]byte("\x1b]52;c;R1RNVVg=\x07"))     // BEL-terminated, "GTMUX"
	term.Write([]byte("\x1b]52;c;dHdv\x1b\\"))       // ST-terminated, "two"
	term.Write([]byte("\x1b]52;p;aWdub3JlZA==\x07")) // primary selection: ignored
	term.Write([]byte("\x1b]52;c;!!!\x07"))          // bad base64: ignored
	term.Write([]byte("\x1b]52;c;?\x07"))            // query: ignored

	got := term.Changes().TakeClipboards()
	if len(got) != 2 || got[0] != "GTMUX" || got[1] != "two" {
		t.Fatalf("TakeClipboards = %q, want [GTMUX two]", got)
	}
	if left := term.Changes().TakeClipboards(); len(left) != 0 {
		t.Fatalf("second drain not empty: %q", left)
	}
}
