package server

import (
	"fmt"

	"github.com/FyrmForge/gtmux/internal/emu"
)

// encodeSGRMouse builds an SGR mouse report: ESC [ < Cb ; Cx ; Cy M (press)
// or ... m (release).
func encodeSGRMouse(cb, cx, cy int, press bool) []byte {
	term := byte('M')
	if !press {
		term = 'm'
	}
	return []byte(fmt.Sprintf("\x1b[<%d;%d;%d%c", cb, cx, cy, term))
}

// encodeX10Mouse builds the legacy 6-byte X10 mouse report (ESC [ M Cb Cx
// Cy, one byte per field as value+32) for apps that never asked for SGR.
// That single-byte encoding caps coordinates around 223; like tmux, we
// clamp rather than refuse to send. Release events lose the original
// button number in this format (there's no way to represent "button 2
// released" — only "some button was released"), so we send the classic
// "no button" marker for those, matching what a real X10-only terminal did.
func encodeX10Mouse(cb, cx, cy int, press bool) []byte {
	if !press {
		cb = 3
	}
	clamp := func(v int) byte {
		if v < 1 {
			v = 1
		}
		if v > 223-32 {
			v = 223 - 32
		}
		return byte(v + 32)
	}
	return []byte{0x1b, '[', 'M', byte(cb + 32), clamp(cx), clamp(cy)}
}

// translateMouseForPane reports the bytes to forward a mouse event into p,
// in whatever protocol p's own application requested — nil if it hasn't
// requested mouse tracking at all, or if this is a motion event and it only
// asked for click tracking (not motion/any-event tracking).
func translateMouseForPane(p *pane, cb, cx, cy int, press bool) []byte {
	mode := p.term.Mode()
	if mode&emu.ModeMouseMask == 0 {
		return nil
	}
	if cb&0x20 != 0 && mode&(emu.ModeMouseMotion|emu.ModeMouseMany) == 0 {
		return nil
	}
	if mode&emu.ModeMouseSgr != 0 {
		return encodeSGRMouse(cb, cx, cy, press)
	}
	return encodeX10Mouse(cb, cx, cy, press)
}
