package server

import (
	"testing"

	"github.com/FyrmForge/gtmux/internal/emu"
	"github.com/FyrmForge/gtmux/internal/geom"
)

// An app's requested modes (mouse, SGR, bracketed paste, DECCKM, kitty flags)
// must survive the dump→replay roundtrip into a fresh emu — losing them left
// nvim without mouse events after every in-place upgrade.
func TestModeSeqsRoundtrip(t *testing.T) {
	old := emu.New(emu.WithSize(geom.Vec2{R: 24, C: 80}))
	old.Write([]byte("\x1b[?1002h\x1b[?1006h\x1b[?2004h\x1b[?1h\x1b[=1;1u"))

	fresh := emu.New(emu.WithSize(geom.Vec2{R: 24, C: 80}))
	fresh.Write([]byte(modeSeqs(old)))

	want := emu.ModeMouseMotion | emu.ModeMouseSgr | emu.ModeBracketedPaste | emu.ModeAppCursor
	if got := fresh.Mode() & want; got != want {
		t.Errorf("modes not restored: got %b, want %b", got, want)
	}
	if fresh.KeyState() != old.KeyState() || fresh.KeyState() == 0 {
		t.Errorf("kitty flags not restored: got %v, want %v", fresh.KeyState(), old.KeyState())
	}
}

// A pane with no special modes replays nothing.
func TestModeSeqsEmpty(t *testing.T) {
	if s := modeSeqs(emu.New(emu.WithSize(geom.Vec2{R: 24, C: 80}))); s != "" {
		t.Errorf("expected empty, got %q", s)
	}
}
