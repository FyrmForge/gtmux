package client

import (
	"testing"

	"github.com/FyrmForge/gtmux/internal/config"
)

func TestDecodeExtKey(t *testing.T) {
	cases := []struct {
		seq  string
		tok  string
		form int
		ok   bool
	}{
		{"49;5u", "C-1", formKitty, true},  // kitty Ctrl+1
		{"50;5u", "C-2", formKitty, true},  // kitty Ctrl+2
		{"27;5;49~", "C-1", formMOK, true}, // modifyOtherKeys Ctrl+1
		{"27;5;51~", "C-3", formMOK, true}, // modifyOtherKeys Ctrl+3
		{"97;5u", "C-a", formKitty, true},  // kitty Ctrl+a
		{"97;3u", "M-a", formKitty, true},  // kitty Alt+a
		{"49;1u", "1", formKitty, true},    // kitty plain 1 (no mods)
		{"1;5D", "", formNone, false},      // C-Left: not an ext key
		{"5~", "", formNone, false},        // PgUp: not modifyOtherKeys
		{"A", "", formNone, false},         // Up arrow
		{"200~", "", formNone, false},      // paste marker, not 27;..~
	}
	for _, c := range cases {
		tok, _, _, form, ok := decodeExtKey(c.seq)
		if tok != c.tok || form != c.form || ok != c.ok {
			t.Errorf("decodeExtKey(%q) = (%q,%d,%v), want (%q,%d,%v)",
				c.seq, tok, form, ok, c.tok, c.form, c.ok)
		}
	}
}

// Unbound modifyOtherKeys keys must not silently kill representable keys in a
// legacy pane — Alt+b has to survive as ESC b, only truly unrepresentable combos
// (Ctrl+digit) may be dropped.
func TestMokLegacyBytes(t *testing.T) {
	cases := []struct {
		code, mods int
		want       string // "" means nil (drop)
	}{
		{'b', 3, "\x1bb"}, // Alt+b -> ESC b  (readline back-word)
		{'a', 5, "\x01"},  // Ctrl+a -> 0x01
		{'1', 5, ""},      // Ctrl+1 -> no legacy form, drop
		{'b', 1, "b"},     // plain
		{'b', 2, "B"},     // Shift+b -> B
	}
	for _, c := range cases {
		got := string(mokLegacyBytes(c.code, c.mods))
		if got != c.want {
			t.Errorf("mokLegacyBytes(%q,%d) = %q, want %q", rune(c.code), c.mods, got, c.want)
		}
	}
}

// The load-bearing invariant: a bound "C-1" must equal the token the decoder
// produces from a real Ctrl+1 keystroke — otherwise the bind never fires.
func TestBindTokenMatchesDecodedKey(t *testing.T) {
	for _, d := range []int{'1', '2', '3'} {
		bind, ok := config.ParseKey("C-" + string(rune(d)))
		if !ok {
			t.Fatalf("ParseKey(C-%c) not ok", d)
		}
		got := keyToken(d, 5) // ctrl
		if bind != got {
			t.Errorf("C-%c: bind token %q != decoded token %q", d, bind, got)
		}
	}
}
