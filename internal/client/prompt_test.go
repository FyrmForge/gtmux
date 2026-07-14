package client

import (
	"reflect"
	"testing"

	"github.com/FyrmForge/gtmux/internal/config"
)

func TestFeedLock(t *testing.T) {
	// No password: any key unlocks.
	open := &compositor{cfg: config.ClientConfig{}}
	if !open.feedLock([]byte("x")) {
		t.Error("no-password lock should unlock on any key")
	}
	// Partial input (no Enter) doesn't unlock.
	if (&compositor{cfg: config.ClientConfig{LockPassword: "s3cret"}}).feedLock([]byte("s3cre")) {
		t.Error("partial input should not unlock")
	}
	// A wrong attempt clears the buffer; the correct one after it unlocks (proves
	// the mismatch reset, so buffered cruft doesn't block the retry).
	pw := &compositor{cfg: config.ClientConfig{LockPassword: "s3cret"}}
	if pw.feedLock([]byte("wrong\r")) {
		t.Error("wrong password should not unlock")
	}
	if !pw.feedLock([]byte("s3cret\r")) {
		t.Error("correct password + Enter should unlock")
	}
}

func TestTokenize(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"paste-buffer -b buf0", []string{"paste-buffer", "-b", "buf0"}},
		{"paste-buffer -b 'my buffer'", []string{"paste-buffer", "-b", "my buffer"}},
		// choose-client picker target: a quoted client-<epoch>@<session> stays one
		// arg even with a spaced session name (the @ and - are ordinary chars).
		{"detach-client -t 'client-1@my sess'", []string{"detach-client", "-t", "client-1@my sess"}},
		{`rename-window "two words"`, []string{"rename-window", "two words"}},
		{`a b\ c`, []string{"a", "b c"}}, // backslash-escaped space
		{"  spaced   out  ", []string{"spaced", "out"}},
		{"empty '' arg", []string{"empty", "", "arg"}}, // empty quotes = empty arg
	}
	for _, c := range cases {
		if got := tokenize(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("tokenize(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

// command-prompt template: %1 as its own field must take the whole typed text,
// spaces included — the reason for pre-splitting the template instead of
// substituting-then-splitting.
func TestPromptTemplateSubstitution(t *testing.T) {
	p := &prompt{kind: "command", tmpl: []string{"rename-window", "%1"}}
	p.feed([]byte("my long name"))
	res := p.feed([]byte{'\r'})
	want := []string{"rename-window", "my long name"}
	if !res.done || !reflect.DeepEqual(res.action, want) {
		t.Fatalf("commit = %v (done=%v), want %v", res.action, res.done, want)
	}
}

// command-prompt -p "a,b" asks twice: the overlay stays open after the first
// answer (label advances to the second), then %1/%2 take the two answers in
// order.
func TestPromptMultiStage(t *testing.T) {
	p := &prompt{kind: "command", labels: []string{"first", "second"}, tmpl: []string{"cmd", "%1", "%2"}}
	if got := p.label(); got != "first" {
		t.Fatalf("stage 0 label = %q, want first", got)
	}
	if res := p.feed(append([]byte("alpha"), '\r')); res.done {
		t.Fatal("prompt should stay open after the first of two answers")
	}
	if got := p.label(); got != "second" {
		t.Fatalf("stage 1 label = %q, want second", got)
	}
	res := p.feed(append([]byte("beta"), '\r'))
	want := []string{"cmd", "alpha", "beta"}
	if !res.done || !reflect.DeepEqual(res.action, want) {
		t.Fatalf("multi-stage commit = %v (done=%v), want %v", res.action, res.done, want)
	}
}

// display-menu: a picker with verb "run" dispatches the selected item's target
// as a command line (split into fields), not a {verb, target} pair.
func TestMenuPickerSelect(t *testing.T) {
	p := &picker{verb: "run", items: []string{"new", "split"}, targets: []string{"new-window", "split-window -h"}}
	p.feed([]byte{'j'}) // move to item 1
	res := p.feed([]byte{'\r'})
	want := []string{"split-window", "-h"}
	if !res.done || !reflect.DeepEqual(res.action, want) {
		t.Fatalf("menu select = %v (done=%v), want %v", res.action, res.done, want)
	}
}

// confirm-before runs the command only on y/Y; Enter and everything else cancel.
func TestPromptConfirm(t *testing.T) {
	mk := func() *prompt { return &prompt{kind: "confirm", cmd: []string{"kill-window"}} }

	if res := mk().feed([]byte{'y'}); !res.done || !reflect.DeepEqual(res.action, []string{"kill-window"}) {
		t.Fatalf("y: got done=%v action=%v, want done+kill-window", res.done, res.action)
	}
	for _, b := range []byte{'\r', 'n', 0x1b, 'q'} {
		if res := mk().feed([]byte{b}); !res.done || res.action != nil {
			t.Fatalf("key %q: got done=%v action=%v, want done+no-action", b, res.done, res.action)
		}
	}
}

// #6: emacs status-keys give the command prompt C-w (kill word) and C-u (kill
// line); with editKeys off they're inert.
func TestPromptEmacsEditing(t *testing.T) {
	p := &prompt{kind: "command", editKeys: true, buf: []byte("foo bar")}
	p.feed([]byte{0x17}) // C-w
	if string(p.buf) != "foo " {
		t.Fatalf("after C-w buf = %q, want %q", p.buf, "foo ")
	}
	p.feed([]byte{0x15}) // C-u
	if string(p.buf) != "" {
		t.Fatalf("after C-u buf = %q, want empty", p.buf)
	}
	// editKeys off: C-w does nothing (and the control byte isn't inserted).
	q := &prompt{kind: "command", editKeys: false, buf: []byte("foo bar")}
	q.feed([]byte{0x17})
	if string(q.buf) != "foo bar" {
		t.Fatalf("editKeys off: buf = %q, want unchanged", q.buf)
	}
}
