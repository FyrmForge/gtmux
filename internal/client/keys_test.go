package client

import (
	"testing"

	"github.com/FyrmForge/gtmux/internal/config"
)

// guardPanic must run the restore (terminal cleanup) before re-raising, so a
// goroutine crash never leaves the pane in raw mode / mouse-reporting.
func TestGuardPanicRestoresThenReraises(t *testing.T) {
	restored := false
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("guardPanic swallowed the panic instead of re-raising")
		}
		if !restored {
			t.Fatal("guardPanic re-raised without running restore first")
		}
		if r != "boom" {
			t.Fatalf("panic value = %v, want boom (original preserved)", r)
		}
	}()
	func() {
		defer guardPanic(func() { restored = true })
		panic("boom")
	}()
}

// The reader (byteKey / csiKeyName / ss3KeyName) and the config parser
// (config.ParseKey → parseKeyName) must agree on the canonical token for a key,
// or a bind written in config never matches the keystroke. This pins that
// contract: for each key, the token a raw keystroke decodes to equals the token
// the same config name parses to.
func TestReaderParserAgree(t *testing.T) {
	// byte keystroke → the config name a user would bind it under.
	byteCases := []struct {
		b    byte
		name string
	}{
		{0x02, "C-b"},
		{0x01, "C-a"},
		{0x1c, "C-\\"}, // C-\ — non-letter control key (byte 0x1c)
		{'c', "c"},
		{'%', "%"},
		{0x09, "Tab"},   // 0x09 folds to C-i; "Tab" alias must match
		{0x0d, "Enter"}, // 0x0d folds to C-m
		{' ', "Space"},
		{0x7f, "BSpace"},
	}
	for _, c := range byteCases {
		reader := byteKey(c.b)
		parsed, ok := config.ParseKey(c.name)
		if !ok || reader != parsed {
			t.Errorf("byte %#x: reader=%q, config.ParseKey(%q)=%q,%v — must match", c.b, reader, c.name, parsed, ok)
		}
	}

	// CSI/SS3 escape sequences → the config name.
	seqCases := []struct {
		token string // what the reader map yields
		name  string // what a user binds
	}{
		{csiKeyName["A"], "Up"},
		{csiKeyName["5~"], "PgUp"},
		{csiKeyName["15~"], "F5"},
		{csiKeyName["24~"], "F12"},
		{ss3KeyName["P"], "F1"},
		{ss3KeyName["D"], "Left"},
	}
	for _, c := range seqCases {
		parsed, ok := config.ParseKey(c.name)
		if !ok || c.token != parsed {
			t.Errorf("seq token %q vs config.ParseKey(%q)=%q,%v — must match", c.token, c.name, parsed, ok)
		}
	}

	// Meta is formed the same way on both sides.
	if got, ok := config.ParseKey("M-h"); !ok || got != "M-"+string(byte('h')) {
		t.Errorf("Meta token mismatch: config=%q, reader forms %q", got, "M-h")
	}
}
