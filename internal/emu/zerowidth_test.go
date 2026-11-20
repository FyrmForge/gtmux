package emu

import (
	"strings"
	"testing"
	"time"
)

// A cell holding a zero-width rune (a stray U+FE0F variation selector) must
// not stall the width-stepping loops: WriteLine and Line.String advanced by
// Width()-1 = -1 and never moved on (the uww picker hang on "☎️").
func TestZeroWidthCellDoesNotHang(t *testing.T) {
	line := Line{{Char: 'a'}, {Char: 0xFE0F}, {Char: 'b'}}
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		WriteLine(&b, line)
		done <- b.String() + "|" + line.String()
	}()
	select {
	case out := <-done:
		if !strings.Contains(out, "b") {
			t.Fatalf("lost the rune after the selector: %q", out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WriteLine/String hung on a zero-width cell")
	}
}
