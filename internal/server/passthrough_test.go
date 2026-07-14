package server

import (
	"bytes"
	"testing"
)

// feed runs a sequence of chunks through one scanner and concatenates the clean
// and host outputs — models a wrapper split across PTY reads.
func feed(chunks ...string) (clean, host []byte) {
	var s ptScanner
	for _, c := range chunks {
		cl, ho := s.scan([]byte(c))
		clean = append(clean, cl...)
		host = append(host, ho...)
	}
	return clean, host
}

func TestPassthroughWholeWrapper(t *testing.T) {
	// ESC P tmux ; <ESC[0m with ESC doubled> ESC \, surrounded by normal output.
	clean, host := feed("before\x1bPtmux;\x1b\x1b[0m\x1b\\after")
	if string(clean) != "beforeafter" {
		t.Fatalf("clean = %q, want %q", clean, "beforeafter")
	}
	if string(host) != "\x1b[0m" {
		t.Fatalf("host = %q, want %q", host, "\x1b[0m")
	}
}

// TestPassthroughLiteralST is the trap: an original ESC-backslash in the payload
// becomes ESC ESC backslash on the wire, which a naive Index(ESC \) would treat
// as an early terminator. The single ESC-pair walk must not.
func TestPassthroughLiteralST(t *testing.T) {
	// payload = ESC '\' (literal); doubled on the wire = ESC ESC '\'.
	clean, host := feed("\x1bPtmux;\x1b\x1b\\\x1b\\X")
	if string(clean) != "X" {
		t.Fatalf("clean = %q, want %q", clean, "X")
	}
	if string(host) != "\x1b\\" {
		t.Fatalf("host = %q, want %q (early-terminated?)", host, "\x1b\\")
	}
}

// TestPassthroughSplitPayload splits the wrapper mid-payload across two reads.
func TestPassthroughSplitPayload(t *testing.T) {
	clean, host := feed("\x1bPtmux;AB", "CD\x1b\\rest")
	if string(clean) != "rest" {
		t.Fatalf("clean = %q, want %q", clean, "rest")
	}
	if string(host) != "ABCD" {
		t.Fatalf("host = %q, want %q", host, "ABCD")
	}
}

// TestPassthroughSplitOpener splits inside the ESC P t m u x ; opener.
func TestPassthroughSplitOpener(t *testing.T) {
	clean, host := feed("x\x1bPtm", "ux;PAY\x1b\\y")
	if string(clean) != "xy" {
		t.Fatalf("clean = %q, want %q", clean, "xy")
	}
	if string(host) != "PAY" {
		t.Fatalf("host = %q, want %q", host, "PAY")
	}
}

// TestPassthroughNonTmuxDCS: a non-tmux DCS (e.g. a DECRQSS reply) must flow to
// emu untouched, with nothing forwarded to the host.
func TestPassthroughNonTmuxDCS(t *testing.T) {
	dcs := "a\x1bP0$r1 q\x1b\\b"
	clean, host := feed(dcs)
	if !bytes.Equal(clean, []byte(dcs)) {
		t.Fatalf("clean = %q, want unchanged %q", clean, dcs)
	}
	if len(host) != 0 {
		t.Fatalf("host = %q, want empty", host)
	}
}

// TestPassthroughRunawayBails: an unterminated payload past the cap must not
// grow the buffer without bound — the scanner bails and recovers so a later,
// well-formed wrapper still works.
func TestPassthroughRunawayBails(t *testing.T) {
	var s ptScanner
	// Opener + a huge run of non-terminator bytes, no ESC \.
	s.scan(append([]byte("\x1bPtmux;"), bytes.Repeat([]byte("A"), ptMaxPayload+16)...))
	if s.open {
		t.Fatalf("scanner still open after overflow; should have bailed")
	}
	if len(s.buf) != 0 {
		t.Fatalf("buffer retained %d bytes after bail, want 0", len(s.buf))
	}
	// State recovered: a normal wrapper parses cleanly afterwards.
	_, host := s.scan([]byte("\x1bPtmux;\x1b\x1b[0m\x1b\\"))
	if string(host) != "\x1b[0m" {
		t.Fatalf("post-bail host = %q, want %q", host, "\x1b[0m")
	}
}

// TestPassthroughPlainPassesThrough: no wrapper at all -> all clean, no host.
func TestPassthroughPlain(t *testing.T) {
	clean, host := feed("hello \x1b[31mworld\x1b[0m")
	if string(clean) != "hello \x1b[31mworld\x1b[0m" {
		t.Fatalf("clean = %q", clean)
	}
	if len(host) != 0 {
		t.Fatalf("host = %q, want empty", host)
	}
}
