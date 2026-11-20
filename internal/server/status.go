package server

import (
	"os/exec"
	"strings"
	"time"

	"github.com/FyrmForge/gtmux/internal/bgcache"
)

// lsof (pane cwd) and git (branch) are subprocesses polled from the session
// tick, so they go through bgcache: the tick reads the last value, the
// subprocess runs off the session goroutine. Values are at most ttl stale.
var (
	cwdCache    = bgcache.New(3 * time.Second)
	branchCache = bgcache.New(3 * time.Second)
)

// gitBranch reports the branch of the repo containing dir; empty (not an
// error) covers the common case of a pane whose cwd isn't a repo. It feeds
// the status bar's git_branch var, which the client expands into its format.
func gitBranch(dir string) string {
	if dir == "" {
		// Unknown pane cwd (e.g. the lookup failed): report nothing rather
		// than letting git -C "" answer for the daemon's own directory.
		return ""
	}
	return branchCache.Get(dir, func() string {
		out, err := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	})
}

// serverShell runs the #server(cmd) commands a client asked for (via Attach)
// and returns their first-line output keyed by command, caching each for
// interval. The client owns the status formats, so it's the only side that
// knows which commands exist — it sends the list, the server runs them here
// each tick and streams the results back in StatusInfo.ServerShell.
type serverShell struct {
	cache *bgcache.Cache
}

func newServerShell(interval time.Duration) *serverShell {
	return &serverShell{cache: bgcache.New(interval)}
}

func (s *serverShell) setInterval(interval time.Duration) { s.cache.SetTTL(interval) }

// run returns the last known output for each command (refreshing stale ones
// in the background). nil in → nil out (the common case: no #server() in any
// attached client's formats).
func (s *serverShell) run(cmds []string) map[string]string {
	if len(cmds) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, cmd := range cmds {
		out[cmd] = s.cache.Get(cmd, func() string {
			raw, _ := exec.Command("sh", "-c", cmd).Output()
			line := strings.TrimSpace(string(raw))
			if nl := strings.IndexByte(line, '\n'); nl >= 0 {
				line = line[:nl]
			}
			return line
		})
	}
	return out
}
