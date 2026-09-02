//go:build linux

package server

import "testing"

// An npm-installed agent runs as `node /usr/bin/codex`, so /proc comm reports
// the interpreter and the pane's command never says "codex" — the sidebar's
// Clanker list and the agent classifier both matched on that name and saw
// nothing. currentCommand unwraps the interpreter to the script's basename.
func TestScriptNameUnwrapsInterpreterCmdline(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cmdline string
		want    string
	}{
		{"codex via node shebang", "node\x00/usr/bin/codex\x00", "codex"},
		{"flags before the script", "python3\x00-u\x00/opt/a/agent.py\x00", "agent.py"},
		{"module form", "python3\x00-m\x00mymod\x00", "mymod"},
		{"bare repl has no script", "node\x00", ""},
		{"flags only", "node\x00--version\x00", ""},
		{"empty", "", ""},
	} {
		if got := scriptName(tc.cmdline); got != tc.want {
			t.Errorf("%s: scriptName(%q) = %q, want %q", tc.name, tc.cmdline, got, tc.want)
		}
	}
	// Only interpreters get unwrapped; comm truncates at 15 chars, which is
	// exactly where "node-MainThread" lands, so the check is a prefix.
	for comm, want := range map[string]bool{
		"node-MainThread": true, "node": true, "python3.12": true, "bun": true,
		"opencode": false, "bash": false, "nodemon-ish": true,
	} {
		if got := isInterpreterComm(comm); got != want {
			t.Errorf("isInterpreterComm(%q) = %v, want %v", comm, got, want)
		}
	}
}
