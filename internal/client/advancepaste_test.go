package client

import "testing"

// advancePaste must recognize the bracketed-paste markers across a byte stream
// (byte at a time, so it works split across reads), and only report done on the
// full "\x1b[200~" — including the ESC-restart case for "\x1b\x1b[200~".
func TestAdvancePaste(t *testing.T) {
	pat := []byte("\x1b[200~")

	feed := func(s string) bool {
		m, done := 0, false
		for i := 0; i < len(s); i++ {
			m, done = advancePaste(m, pat, s[i])
		}
		return done
	}

	if !feed("\x1b[200~") {
		t.Error("exact marker should complete")
	}
	if !feed("hello\x1b[200~") {
		t.Error("marker after other bytes should complete")
	}
	if !feed("\x1b\x1b[200~") {
		t.Error("a stray ESC before the marker must restart, not break the match")
	}
	if feed("\x1b[201~") {
		t.Error("the CLOSE marker must not complete the OPEN pattern")
	}
	if feed("\x1b[20") {
		t.Error("partial marker must not complete")
	}
	// Completion resets progress to 0, so a second marker is independently found.
	m, done := 0, false
	for _, b := range []byte("\x1b[200~x\x1b[200~") {
		m, done = advancePaste(m, pat, b)
	}
	if !done {
		t.Error("second marker should complete after progress reset")
	}
}
