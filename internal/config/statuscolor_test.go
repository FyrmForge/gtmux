package config

import (
	"os"
	"path/filepath"
	"testing"
)

// status_fg / status_bg were parsed but silently DROPPED — applyOption only
// recognized the combined status_style, and it returns false for an unknown
// name which every caller ignores. So a config setting them kept the default
// colors with no error. Guards the direct fg/bg pair.
func TestStatusFGBG(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "c.lua")
	os.WriteFile(p, []byte(`gtmux.options.status_bg = "red"
gtmux.options.status_fg = "blue"`), 0o644)
	cfg, b := LoadClient(p)
	defer b.Close()
	wantBG, _ := ColorByName("red")
	wantFG, _ := ColorByName("blue")
	t.Logf("StatusBG=%v want=%v | StatusFG=%v want=%v", cfg.StatusBG, wantBG, cfg.StatusFG, wantFG)
	if cfg.StatusBG != wantBG {
		t.Error("status_bg NOT applied")
	}
	if cfg.StatusFG != wantFG {
		t.Error("status_fg NOT applied")
	}
}

// Unknown option names used to be dropped in total silence — applyOption
// returns false for them and every caller ignored it, which is how status_fg /
// status_bg went unnoticed. They're now collected and logged once, covering
// both a typo and a server option put in client.lua. Valid options in the same
// file must still apply.
func TestUnknownOptionsWarnButDontBreakValidOnes(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "c.lua")
	if err := os.WriteFile(p, []byte(`gtmux.options.stauts_bg = "green"
gtmux.set_option("main_pane_width", "100")
gtmux.options.status_bg = "red"`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, b := LoadClient(p)
	defer b.Close()
	want, _ := ColorByName("red")
	if cfg.StatusBG != want {
		t.Errorf("StatusBG=%v, want %v — a valid option must still apply alongside unknown ones", cfg.StatusBG, want)
	}
}
