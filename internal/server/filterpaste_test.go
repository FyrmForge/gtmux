package server

import (
	"bytes"
	"testing"

	"github.com/FyrmForge/gtmux/internal/emu"
	"github.com/FyrmForge/gtmux/internal/geom"
)

// filterPaste is the server-side bracketed-paste gate. The client forwards the
// 200~/201~ markers unconditionally; the server strips them ONLY when the pane's
// app hasn't enabled 2004 — otherwise a pasted password into `sudo` (no 2004)
// would arrive as "200~pw201~" and fail auth, and a plain shell would print the
// markers. An app that HAS enabled 2004 must receive them intact.
func TestFilterPaste(t *testing.T) {
	newPane := func() *pane {
		return &pane{term: emu.New(emu.WithSize(geom.Vec2{R: 5, C: 20}))}
	}
	pasted := []byte("\x1b[200~echo hi\rmore\x1b[201~")
	stripped := []byte("echo hi\rmore")

	// App has NOT enabled 2004 → markers stripped.
	p := newPane()
	if got := p.filterPaste(pasted); !bytes.Equal(got, stripped) {
		t.Errorf("no-2004: got %q, want %q (markers should be stripped)", got, stripped)
	}

	// App enables 2004 → markers pass through untouched.
	p2 := newPane()
	p2.term.Write([]byte("\x1b[?2004h"))
	if got := p2.filterPaste(pasted); !bytes.Equal(got, pasted) {
		t.Errorf("2004-on: got %q, want the markers left intact", got)
	}

	// Plain keystrokes (no ESC) are never rewritten, regardless of 2004 state.
	keys := []byte("ls -la")
	if got := newPane().filterPaste(keys); !bytes.Equal(got, keys) {
		t.Errorf("plain keys altered: got %q", got)
	}
}
