package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadServerDefaults(t *testing.T) {
	cfg := LoadServer("/nonexistent/gtmux/server.lua")
	if cfg.SessionName != "%d" {
		t.Errorf("default SessionName = %q, want %q", cfg.SessionName, "%d")
	}
}

// session_name is the only server-side option; client-side set_option keys
// (prefix, status_*) are ignored, and session_name is still applied.
func TestLoadServerSessionNameAndIgnoresRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.lua")
	body := `gtmux.set_option("prefix", "C-a")
gtmux.set_option("status_left", "custom")
gtmux.set_option("session_name", "win%d")`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := LoadServer(path)
	if cfg.SessionName != "win%d" {
		t.Errorf("SessionName = %q, want %q", cfg.SessionName, "win%d")
	}
}

func TestParseKey(t *testing.T) {
	cases := []struct {
		in   string
		want byte
		ok   bool
	}{
		{"c", 'c', true},
		{"C-b", 0x02, true},
		{"C-B", 0x02, true},
		{"", 0, false},
		{"abc", 0, false},
	}
	for _, c := range cases {
		got, ok := parseKey(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("parseKey(%q) = %#x, %v; want %#x, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}
