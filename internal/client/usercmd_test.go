package client

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/FyrmForge/gtmux/internal/config"
)

// TestUserCommands: gtmux.command entries and generated dock:* commands reach
// the palette, a built-in-shadowing name is dropped, and both kinds resolve.
func TestUserCommands(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.lua")
	os.WriteFile(p, []byte(`
gtmux.widget{ dock = "left", size = 10, name = "side", text = "x" }
gtmux.command{ name = "my:hide", desc = "hide side", run = function() gtmux.hide_dock("side") end }
gtmux.command{ name = "kill-pane", run = function() end }
`), 0o644)
	cfg, b := config.LoadClient(p)
	defer b.Close()
	c := newCompositor()
	c.cfg = cfg
	c.rebuildWidgets(b)

	pk := newPalettePicker(c.paletteCands, c.paletteExtra()...)
	have := map[string]bool{}
	for _, it := range pk.items {
		have[it] = true
	}
	for _, want := range []string{"my:hide", "dock:hide-side", "dock:show-side", "dock:toggle-side"} {
		if !have[want] {
			t.Errorf("palette missing %q", want)
		}
	}
	for _, uc := range c.userCmds {
		if uc.name == "kill-pane" {
			t.Error("built-in-shadowing command kept")
		}
	}

	ops, ok := b.RunCommand("my:hide")
	if !ok || !reflect.DeepEqual(ops, []config.BindOp{{ToggleDock: "side", DockMode: -1}}) {
		t.Fatalf("RunCommand ops = %v, %v", ops, ok)
	}
	if n, m, ok := parseDockCmd("dock:show-side"); !ok || n != "side" || m != 1 {
		t.Fatalf("parseDockCmd = %q %d %v", n, m, ok)
	}
	c.setDock("side", -1)
	c.setDock("side", -1) // hide is idempotent, unlike toggle
	if len(c.docks) != 0 {
		t.Fatal("dock visible after hide")
	}
}
