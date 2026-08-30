package emu

import (
	"bytes"
	"testing"
)

// OSC 11 "?" with no override used to be dropped; with a fallback installed it
// must answer, so auto-theming apps inside a pane can tell light from dark.
func TestOSC11QueryFallback(t *testing.T) {
	old := DefaultColorFallback
	defer func() { DefaultColorFallback = old }()
	DefaultColorFallback = func(num int) (int, int, int, bool) { return 0xff, 0xff, 0xff, true }
	var out bytes.Buffer
	s := New(WithWriter(&out))
	s.Write([]byte("\033]11;?\007"))
	if got := out.String(); got != "\033]11;rgb:ffff/ffff/ffff\007" {
		t.Errorf("reply = %q", got)
	}
}
