package client

import (
	"testing"
	"time"
)

func TestExpandStatus(t *testing.T) {
	e := newStatusExpander(time.Minute)
	vars := map[string]string{"host": "box", "session": "dev", "git_branch": "main", "clock": "12:00"}

	cases := []struct{ format, want string }{
		{"[#{host}][#{session}]", "[box][dev]"},
		{"#{?git_branch,[git:#{git_branch}] ,}#{clock}", "[git:main] 12:00"},
		{"#{?missing,a,b}", "b"},
		{"##{host}", "#{host}"},
		{"#{unknown}", ""},
		{"#{?git_branch,#{host},#{session}}", "box"},
		{"plain text", "plain text"},
		{"#{unclosed", "#{unclosed"},
		{"#(echo hi)", ""}, // bare #() has no side declared — ignored
	}
	for _, c := range cases {
		if got := e.expand(c.format, vars, nil); got != c.want {
			t.Errorf("expand(%q) = %q, want %q", c.format, got, c.want)
		}
	}

	// Empty git_branch flips the conditional to its else branch.
	vars["git_branch"] = ""
	if got := e.expand("#{?git_branch,[git:#{git_branch}] ,}#{clock}", vars, nil); got != "12:00" {
		t.Errorf("empty-branch conditional = %q, want %q", got, "12:00")
	}
}

func TestExpandClientShellCached(t *testing.T) {
	e := newStatusExpander(time.Minute)
	// First expansion never blocks: empty now, the command runs in the background.
	if got := e.expand("#client(echo hi)", nil, nil); got != "" {
		t.Fatalf("first #client shell = %q, want empty (non-blocking)", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for e.expand("#client(echo hi)", nil, nil) != "hi" {
		if time.Now().After(deadline) {
			t.Fatalf("#client shell never landed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestExpandServerShell(t *testing.T) {
	e := newStatusExpander(time.Minute)
	serverShell := map[string]string{"uptime -p": "up 3 days"}
	if got := e.expand("load: #server(uptime -p)", nil, serverShell); got != "load: up 3 days" {
		t.Errorf("#server shell = %q, want %q", got, "load: up 3 days")
	}
	// A #server() with no streamed result expands to empty (server hasn't run it).
	if got := e.expand("#server(missing)", nil, serverShell); got != "" {
		t.Errorf("#server unknown = %q, want empty", got)
	}
}

func TestExtractServerCmds(t *testing.T) {
	got := extractServerCmds("a #server(one) b", "#client(x) #server(two) #server(one)")
	want := []string{"one", "two"} // deduped, in first-seen order
	if len(got) != len(want) {
		t.Fatalf("extractServerCmds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("cmd %d = %q, want %q", i, got[i], want[i])
		}
	}
	// #client() commands are NOT extracted (they run locally).
	if cmds := extractServerCmds("#client(only)"); len(cmds) != 0 {
		t.Errorf("client-only formats yielded %v, want none", cmds)
	}
}
