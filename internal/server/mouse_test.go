package server

import (
	"testing"

	"github.com/FyrmForge/gtmux/internal/emu"
	"github.com/FyrmForge/gtmux/internal/geom"
)

func newTermWithModes(decset string) emu.Terminal {
	term := emu.New(emu.WithSize(geom.Vec2{R: 24, C: 80}))
	term.Write([]byte(decset))
	return term
}

func TestTranslateMouseForPane(t *testing.T) {
	t.Run("no mouse mode requested", func(t *testing.T) {
		p := &pane{term: newTermWithModes("")}
		if got := translateMouseForPane(p, 0, 5, 5, true); got != nil {
			t.Errorf("expected nil, got %q", got)
		}
	})

	t.Run("click tracking + SGR", func(t *testing.T) {
		p := &pane{term: newTermWithModes("\x1b[?1000h\x1b[?1006h")}
		got := translateMouseForPane(p, 0, 5, 7, true)
		want := "\x1b[<0;5;7M"
		if string(got) != want {
			t.Errorf("press: got %q, want %q", got, want)
		}
		got = translateMouseForPane(p, 0, 5, 7, false)
		want = "\x1b[<0;5;7m"
		if string(got) != want {
			t.Errorf("release: got %q, want %q", got, want)
		}
		// This pane only asked for click tracking, not motion.
		if got := translateMouseForPane(p, 0x20, 5, 7, true); got != nil {
			t.Errorf("expected motion event to be dropped, got %q", got)
		}
	})

	t.Run("any-event tracking forwards motion", func(t *testing.T) {
		p := &pane{term: newTermWithModes("\x1b[?1003h\x1b[?1006h")}
		got := translateMouseForPane(p, 0x20, 5, 7, true)
		want := "\x1b[<32;5;7M"
		if string(got) != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("legacy X10 when SGR not requested", func(t *testing.T) {
		p := &pane{term: newTermWithModes("\x1b[?1000h")}
		got := translateMouseForPane(p, 0, 5, 7, true)
		want := []byte{0x1b, '[', 'M', byte(0 + 32), byte(5 + 32), byte(7 + 32)}
		if string(got) != string(want) {
			t.Errorf("press: got %v, want %v", got, want)
		}
		// X10 release loses the button number: always encoded as 3.
		got = translateMouseForPane(p, 0, 5, 7, false)
		want = []byte{0x1b, '[', 'M', byte(3 + 32), byte(5 + 32), byte(7 + 32)}
		if string(got) != string(want) {
			t.Errorf("release: got %v, want %v", got, want)
		}
	})

	t.Run("X10 clamps coordinates", func(t *testing.T) {
		p := &pane{term: newTermWithModes("\x1b[?1000h")}
		got := translateMouseForPane(p, 0, 9999, 1, true)
		if got[4] != byte(223) {
			t.Errorf("expected clamped column byte 223, got %d", got[4])
		}
	})
}
