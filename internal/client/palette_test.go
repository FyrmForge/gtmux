package client

import (
	"reflect"
	"testing"
)

func testPalette() *picker {
	return newPalettePicker(func(kind string) ([]string, []string) {
		switch kind {
		case "session":
			return []string{"gtmux", "notes"}, []string{"gtmux", "notes"}
		case "option":
			return paletteOptions, paletteOptions
		}
		return nil, nil
	})
}

// TestPaletteStages walks the command palette through both stages: fuzzy pick a
// command, Enter on one that takes an argument opens stage two (candidates),
// fuzzy pick the candidate, Enter runs command + candidate.
func TestPaletteStages(t *testing.T) {
	p := testPalette()
	p.feed([]byte("swcl t"))
	if v := p.view(); len(v) == 0 || p.items[v[0]] != "switch-client -t" {
		t.Fatalf("stage one top match = %v", v)
	}
	if r := p.feed([]byte("\r")); r.done {
		t.Fatal("Enter on a command that takes an argument must open stage two, not run")
	}
	if p.filter != "switch-client -t " || p.palette.cmd == nil {
		t.Fatalf("filter = %q, stage two = %v", p.filter, p.palette.cmd != nil)
	}
	p.feed([]byte("nts"))
	r := p.feed([]byte("\r"))
	if !reflect.DeepEqual(r.action, []string{"switch-client", "-t", "notes"}) {
		t.Fatalf("action = %v", r.action)
	}
}

// TestPaletteVerbatim: text that matches nothing runs as typed (the palette is
// still a command line), and free-text arguments run with what was typed.
func TestPaletteVerbatim(t *testing.T) {
	p := testPalette()
	r := p.feed([]byte("rename-window my win\r"))
	if !reflect.DeepEqual(r.action, []string{"rename-window", "my", "win"}) {
		t.Fatalf("free-text arg: action = %v", r.action)
	}
	p = testPalette()
	r = p.feed([]byte("zzqqx 1\r"))
	if !reflect.DeepEqual(r.action, []string{"zzqqx", "1"}) {
		t.Fatalf("no match: action = %v", r.action)
	}
}

// TestPaletteTabAndOption: Tab completes the selection into the input; picking
// an option completes it (it needs a value) instead of running set-option bare.
func TestPaletteTabAndOption(t *testing.T) {
	p := testPalette()
	p.feed([]byte("set-option -g\t"))
	if p.filter != "set-option -g " {
		t.Fatalf("Tab: filter = %q", p.filter)
	}
	p.feed([]byte("monact"))
	if r := p.feed([]byte("\r")); r.done {
		t.Fatal("picking an option must not run set-option without a value")
	}
	if p.filter != "set-option -g monitor-activity " {
		t.Fatalf("option completion: filter = %q", p.filter)
	}
	r := p.feed([]byte("on\r"))
	if !reflect.DeepEqual(r.action, []string{"set-option", "-g", "monitor-activity", "on"}) {
		t.Fatalf("action = %v", r.action)
	}
}
