package server

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/FyrmForge/gtmux/internal/emu"
)

// themeDefaultColor answers OSC 10 (fg) / 11 (bg) queries from pane apps with
// a colour that matches ~/.theme-mode (written by theme-apply): light gives
// a white bg / black fg, anything else the reverse. Apps only use the answer
// to pick light vs dark, so the exact palette isn't needed. Read per query so
// a theme switch is seen without a server restart.
func themeDefaultColor(num int) (r, g, b int, ok bool) {
	light := false
	if home, err := os.UserHomeDir(); err == nil {
		if raw, err := os.ReadFile(filepath.Join(home, ".theme-mode")); err == nil {
			light = strings.TrimSpace(string(raw)) == "light"
		}
	}
	bg := num == 11
	if light == bg {
		return 0xff, 0xff, 0xff, true
	}
	return 0, 0, 0, true
}

func init() { emu.DefaultColorFallback = themeDefaultColor }
