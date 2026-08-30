package server

import "testing"

// light: bg white, fg black; dark: the reverse. ok always true.
func TestThemeDefaultColor(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no ~/.theme-mode -> dark
	if r, _, _, ok := themeDefaultColor(11); !ok || r != 0 {
		t.Errorf("dark bg: r=%d ok=%v", r, ok)
	}
	if r, _, _, _ := themeDefaultColor(10); r != 0xff {
		t.Errorf("dark fg: r=%d", r)
	}
}
