package emu

import (
	"bytes"
	"testing"

	"github.com/FyrmForge/gtmux/internal/geom"
)

// Only a PRIMARY device-attributes query (ESC[c / ESC[0c) gets the ?6c reply.
// Secondary (ESC[>c), tertiary (ESC[=c) and private (ESC[?c) carry a marker and
// must NOT be answered with ?6c — a real terminal replies in a distinct format
// or not at all. Over-answering leaked stray bytes: tmux's startup probe sends
// primary + secondary, and the bogus second ?6c echoed into the pane.
func TestDeviceAttributesPrimaryOnly(t *testing.T) {
	cases := []struct {
		seq  string
		want string
	}{
		{"\033[c", "\033[?6c"},  // primary
		{"\033[0c", "\033[?6c"}, // primary, explicit 0
		{"\033[>c", ""},         // secondary
		{"\033[=c", ""},         // tertiary
		{"\033[?c", ""},         // private
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		term := New(WithSize(geom.Vec2{R: 5, C: 20}), WithWriter(&buf))
		_, _ = term.Write([]byte(tc.seq))
		if buf.String() != tc.want {
			t.Errorf("DA %q -> reply %q, want %q", tc.seq, buf.String(), tc.want)
		}
	}
}
