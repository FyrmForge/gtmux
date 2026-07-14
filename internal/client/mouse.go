package client

import (
	"strconv"
	"strings"
)

// mouseParser incrementally recognizes "ESC [ < Cb ; Cx ; Cy M/m" sequences
// in the raw stdin byte stream — the job session.go used to do server-side,
// moved here since the client is what actually receives SGR mouse reports
// from its real terminal (it enabled that reporting itself on attach).
type mouseParser struct {
	stage    int // 0 idle, 1 saw ESC, 2 saw ESC[, 3 saw ESC[< (collecting params)
	raw      []byte
	paramBuf []byte
}

// feed processes one byte. onMouse is called if it completes a recognized
// mouse sequence. consumed reports whether b was absorbed by the state
// machine (either as part of a still-forming sequence, or flushed back out
// because it turned out not to fit one) — the caller should forward b as
// ordinary input only when consumed is false, plus whatever's in flushed.
func (m *mouseParser) feed(b byte, onMouse func(cb, x, y int, press bool)) (consumed bool, flushed []byte) {
	if m.stage == 0 && b != 0x1b {
		return false, nil
	}
	switch m.stage {
	case 0:
		m.stage = 1
		m.raw = []byte{b}
		return true, nil
	case 1:
		m.raw = append(m.raw, b)
		if b == '[' {
			m.stage = 2
			return true, nil
		}
	case 2:
		m.raw = append(m.raw, b)
		if b == '<' {
			m.stage = 3
			m.paramBuf = nil
			return true, nil
		}
	default: // 3: collecting "Cb;Cx;Cy" up to the M/m terminator
		m.raw = append(m.raw, b)
		if b == 'M' || b == 'm' {
			cb, x, y, ok := parseMouseParams(m.paramBuf)
			m.stage, m.raw, m.paramBuf = 0, nil, nil
			if ok {
				onMouse(cb, x, y, b == 'M')
			}
			return true, nil
		}
		if b == ';' || (b >= '0' && b <= '9') {
			m.paramBuf = append(m.paramBuf, b)
			return true, nil
		}
	}
	// Doesn't fit a mouse sequence after all: flush the buffered bytes
	// through as ordinary input instead of dropping them.
	flushed = m.raw
	m.stage, m.raw, m.paramBuf = 0, nil, nil
	return true, flushed
}

// flush returns whatever partial sequence is buffered and resets the parser.
// Called at the end of each stdin read: terminals write a whole mouse escape
// sequence in one go, so bytes still pending at a chunk boundary are almost
// certainly a bare ESC (or Alt-chord) that should be forwarded as input now
// rather than held until the next keypress.
func (m *mouseParser) flush() []byte {
	pending := m.raw
	m.stage, m.raw, m.paramBuf = 0, nil, nil
	return pending
}

func parseMouseParams(buf []byte) (cb, x, y int, ok bool) {
	parts := strings.Split(string(buf), ";")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	var e1, e2, e3 error
	cb, e1 = strconv.Atoi(parts[0])
	x, e2 = strconv.Atoi(parts[1])
	y, e3 = strconv.Atoi(parts[2])
	return cb, x, y, e1 == nil && e2 == nil && e3 == nil
}
