package server

import "bytes"

// Passthrough handling for tmux's `allow-passthrough`. An app inside a pane can
// emit  ESC P tmux ; <payload> ESC \  (a DCS) where every ESC in <payload> is
// doubled; the multiplexer strips the envelope, un-doubles, and forwards the raw
// payload to the real client terminal.
//
// This can't ride emu's DCS callbacks: go-vte aborts a DCS on the first payload
// ESC, and passthrough payloads are ESC-laden by definition (see probe in
// history). So we scan the raw pane byte-stream in front of emu, strip the
// wrappers (whether or not passthrough is enabled — an un-stripped payload's
// inner OSC/CSI would otherwise execute on gtmux's own emu, the exact thing
// passthrough prevents), and hand the un-doubled payload to the session, which
// forwards it only when allow-passthrough is on.

var ptOpen = []byte("\x1bPtmux;") // ESC P t m u x ;

// ptMaxPayload caps how much unterminated passthrough payload we buffer. A
// passthrough this large is malformed (a real one is an escape sequence, KiB at
// most); without the cap, an app that emits the opener and never sends the
// terminator would grow s.buf without bound (and re-walk it O(n²) each chunk) —
// and the scanner runs even when allow-passthrough is off. On overflow we bail:
// discard the payload and resume normal parsing, matching tmux's cap+discard.
const ptMaxPayload = 1 << 20 // 1 MiB

// ptScanner extracts passthrough wrappers from a pane's output stream. One per
// pane; it holds a partial wrapper across PTY read boundaries (a wrapper can
// split anywhere between two reads).
type ptScanner struct {
	open bool   // inside a wrapper: buf accumulates the raw (still-doubled) payload
	buf  []byte // closed: a trailing partial-opener prefix held back; open: payload so far
}

// scan splits data into (clean, host): clean is everything outside passthrough
// wrappers (fed to emu); host is the concatenated un-doubled payloads. Returned
// slices are freshly allocated (no aliasing of data or internal state).
func (s *ptScanner) scan(data []byte) (clean, host []byte) {
	work := make([]byte, 0, len(s.buf)+len(data))
	work = append(work, s.buf...)
	work = append(work, data...)
	s.buf = nil
	i := 0
	for i < len(work) {
		if s.open {
			// Find the terminator by walking ESC pairs from the payload start:
			// ESC ESC = a doubled (literal) ESC, ESC \ = the real terminator. One
			// left-to-right walk both finds the end and validates the pairing.
			end, complete := ptPayloadEnd(work[i:])
			if !complete {
				if len(work)-i > ptMaxPayload {
					// Runaway/unterminated passthrough: give up, discard it, and
					// resume normal parsing so memory (and the re-walk) stay bounded.
					s.open = false
					return clean, host
				}
				s.buf = append([]byte(nil), work[i:]...) // hold whole partial payload
				return clean, host
			}
			host = append(host, ptUndouble(work[i:i+end])...)
			i += end + 2 // skip the ESC \ terminator
			s.open = false
			continue
		}
		// Closed: look for the opener in the remaining bytes.
		j := bytes.Index(work[i:], ptOpen)
		if j >= 0 {
			clean = append(clean, work[i:i+j]...)
			i += j + len(ptOpen)
			s.open = true
			continue
		}
		// No opener. Emit everything except a trailing suffix that is a proper
		// prefix of the opener (it might complete on the next read).
		rest := work[i:]
		k := openerPrefixSuffix(rest)
		clean = append(clean, rest[:len(rest)-k]...)
		if k > 0 {
			s.buf = append([]byte(nil), rest[len(rest)-k:]...)
		}
		return clean, host
	}
	return clean, host
}

// ptPayloadEnd walks p as a doubled-ESC payload. It returns the index of the
// terminating ESC (the ESC of a final ESC \) and complete=true, or complete=false
// if p ends mid-payload (needs more bytes).
func ptPayloadEnd(p []byte) (end int, complete bool) {
	i := 0
	for i < len(p) {
		if p[i] != 0x1b {
			i++
			continue
		}
		if i+1 >= len(p) {
			return 0, false // lone trailing ESC — partner not arrived
		}
		switch p[i+1] {
		case 0x1b:
			i += 2 // doubled literal ESC, stays in payload
		case '\\':
			return i, true // terminator
		default:
			// Malformed (single ESC not part of a pair). Treat as literal to stay
			// robust; skip it.
			i++
		}
	}
	return 0, false
}

// ptUndouble collapses each ESC ESC in a validated payload to a single ESC.
func ptUndouble(p []byte) []byte {
	out := make([]byte, 0, len(p))
	for i := 0; i < len(p); i++ {
		out = append(out, p[i])
		if p[i] == 0x1b && i+1 < len(p) && p[i+1] == 0x1b {
			i++ // skip the second ESC of the pair
		}
	}
	return out
}

// openerPrefixSuffix returns the length of the longest suffix of s that is a
// proper prefix of ptOpen (1..len(ptOpen)-1), or 0 — the partial opener to hold.
func openerPrefixSuffix(s []byte) int {
	max := len(ptOpen) - 1
	if len(s) < max {
		max = len(s)
	}
	for k := max; k > 0; k-- {
		if bytes.Equal(s[len(s)-k:], ptOpen[:k]) {
			return k
		}
	}
	return 0
}
