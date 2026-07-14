package client

import "testing"

func TestMouseParserRecognizesSGR(t *testing.T) {
	var mp mouseParser
	var got []struct {
		cb, x, y int
		press    bool
	}
	onMouse := func(cb, x, y int, press bool) {
		got = append(got, struct {
			cb, x, y int
			press    bool
		}{cb, x, y, press})
	}

	seq := "\x1b[<0;20;10M\x1b[<0;20;10m"
	for _, b := range []byte(seq) {
		consumed, flushed := mp.feed(b, onMouse)
		if !consumed {
			t.Fatalf("byte %q should have been consumed by the mouse parser", b)
		}
		if flushed != nil {
			t.Fatalf("unexpected flush mid-sequence: %q", flushed)
		}
	}

	if len(got) != 2 {
		t.Fatalf("expected 2 events, got %d", len(got))
	}
	if got[0] != struct {
		cb, x, y int
		press    bool
	}{0, 20, 10, true} {
		t.Errorf("press event = %+v", got[0])
	}
	if got[1].press {
		t.Errorf("second event should be a release, got press=true")
	}
}

func TestMouseParserFlushesNonMouseEscape(t *testing.T) {
	var mp mouseParser
	onMouse := func(cb, x, y int, press bool) {
		t.Fatalf("onMouse should not be called for a plain arrow-key escape")
	}

	// A plain "ESC [ A" (up arrow) isn't a mouse sequence — the parser
	// should recognize the mismatch at '[A' and flush the buffered bytes
	// back out instead of silently eating them.
	seq := []byte{0x1b, '[', 'A'}
	var recovered []byte
	for _, b := range seq {
		consumed, flushed := mp.feed(b, onMouse)
		if !consumed {
			// The lone 'A' isn't part of any candidate sequence by itself;
			// callers forward it as ordinary input.
			recovered = append(recovered, b)
			continue
		}
		recovered = append(recovered, flushed...)
	}
	if string(recovered) != string(seq) {
		t.Errorf("recovered %q, want %q (no bytes should be lost)", recovered, seq)
	}
}

func TestMouseParserPlainBytesPassThrough(t *testing.T) {
	var mp mouseParser
	onMouse := func(cb, x, y int, press bool) {
		t.Fatalf("onMouse should not fire for plain typed input")
	}
	for _, b := range []byte("hello") {
		if consumed, _ := mp.feed(b, onMouse); consumed {
			t.Fatalf("plain byte %q should not be consumed by the mouse parser", b)
		}
	}
}
