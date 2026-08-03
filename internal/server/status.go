package server

import (
	"os/exec"
	"strings"
	"time"
)

// gitBranch shells out to git in dir; empty result (not an error) covers the
// common case of a pane whose cwd isn't a repo. It feeds the status bar's
// git_branch var, which the client expands into its format.
// ponytail: polled once a second from the status tick, not cached beyond
// that — fine at this scale, revisit if per-session git spawning shows up
// in profiling.
func gitBranch(dir string) string {
	if dir == "" {
		// Unknown pane cwd (e.g. the lookup failed): report nothing rather
		// than letting git -C "" answer for the daemon's own directory.
		return ""
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// serverShell runs the #server(cmd) commands a client asked for (via Attach)
// and returns their first-line output keyed by command, caching each for
// interval. The client owns the status formats, so it's the only side that
// knows which commands exist — it sends the list, the server runs them here
// each tick and streams the results back in StatusInfo.ServerShell.
type serverShell struct {
	interval time.Duration
	cache    map[string]shellCacheEntry
}

type shellCacheEntry struct {
	out string
	at  time.Time
}

func newServerShell(interval time.Duration) *serverShell {
	return &serverShell{interval: interval, cache: map[string]shellCacheEntry{}}
}

// run returns cached-or-fresh output for each command. nil in → nil out (the
// common case: no #server() in any attached client's formats).
func (s *serverShell) run(cmds []string) map[string]string {
	if len(cmds) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, cmd := range cmds {
		if r, ok := s.cache[cmd]; ok && time.Since(r.at) < s.interval {
			out[cmd] = r.out
			continue
		}
		raw, _ := exec.Command("sh", "-c", cmd).Output()
		line := strings.TrimSpace(string(raw))
		if nl := strings.IndexByte(line, '\n'); nl >= 0 {
			line = line[:nl]
		}
		s.cache[cmd] = shellCacheEntry{out: line, at: time.Now()}
		out[cmd] = line
	}
	return out
}
