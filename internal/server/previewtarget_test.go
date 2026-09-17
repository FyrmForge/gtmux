package server

import "testing"

// A preview target names a session and optionally a %id pane; anything else
// (including tmux's bare pane-index form) falls back to the active pane.
func TestSplitPreviewTarget(t *testing.T) {
	for _, tc := range []struct {
		in   string
		name string
		id   int
	}{
		{"sess", "sess", 0},
		{"sess:%12", "sess", 12},
		{"sess:3", "sess", 0},
		{"sess:%x", "sess", 0},
		{"", "", 0},
	} {
		name, id := splitPreviewTarget(tc.in)
		if name != tc.name || id != tc.id {
			t.Errorf("splitPreviewTarget(%q) = (%q,%d), want (%q,%d)", tc.in, name, id, tc.name, tc.id)
		}
	}
}
