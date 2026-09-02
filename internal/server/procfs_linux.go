//go:build linux

package server

import (
	"fmt"
	"os"
	"path"
	"strings"
)

// procComm returns pid's short command name via /proc.
func procComm(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(string(b), "\n")
}

// procCwd returns pid's current working directory via /proc.
func procCwd(pid int) string {
	link, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
	if err != nil {
		return ""
	}
	return link
}

// Interpreter comms hide the real program: an npm-installed agent runs as
// `node /usr/bin/codex`, whose comm is "node" (truncated "node-MainThread"),
// so nothing downstream can tell it apart from any other node process.
var interpreterComms = []string{"node", "bun", "deno", "python"}

// procCommand returns the pane-visible command name for pid — comm, except on
// an interpreter, where the script's basename is the name a user (and the
// agent classifier) actually means.
func procCommand(pid int) string {
	comm := procComm(pid)
	if !isInterpreterComm(comm) {
		return comm
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return comm
	}
	if name := scriptName(string(b)); name != "" {
		return name
	}
	return comm // bare REPL, or flags only
}

func isInterpreterComm(comm string) bool {
	for _, p := range interpreterComms {
		if strings.HasPrefix(comm, p) {
			return true
		}
	}
	return false
}

// scriptName picks the script argument out of a NUL-separated /proc cmdline
// ("node\0/usr/bin/codex\0") and returns its basename. Flags are skipped; ""
// when no script argument follows argv[0].
// ponytail: first non-flag arg wins, so `node --require x script.js` names x.
// Parse per-interpreter flag arity if that ever shows up in a real pane.
func scriptName(cmdline string) string {
	args := strings.Split(strings.TrimSuffix(cmdline, "\x00"), "\x00")
	for _, a := range args[1:] {
		if a == "" || strings.HasPrefix(a, "-") {
			continue
		}
		return path.Base(a)
	}
	return ""
}
