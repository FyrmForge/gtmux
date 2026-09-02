//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/gtmux/internal/harness"
)

// Agent busy-detection from the pane's screen, full stack. opencode never puts
// a working marker in its title and never rings the bell — its only live signal
// is an "esc interrupt" hint on the bottom rows, so the server ships every
// pane's screen tail in the snapshot and gtmux.agents{busy_screen=...} matches
// on it. `sleep` stands in for the agent here: the same pane command spans both
// phases, so the state has to come from the screen, not from the command.
//
// Agent state only reaches a client that has a Lua widget (that's what turns
// server snapshots on), hence the dock — which is also how the real sidebar
// gets it.
func TestAgentBusyFromScreenTail(t *testing.T) {
	c := harness.StartWithConfig(t, `
gtmux.agents{ { match = "sleep", busy_screen = "esc interrupt" } }
gtmux.widget{ dock = "top", size = 1, interval = 1, draw = function(c)
  for _, p in ipairs(gtmux.find_panes({})) do
    if p.command == "sleep" then
      c:text(0, 0, "AGENT=" .. (p.state == "" and "unknown" or p.state))
    end
  end
end }
`, "")
	c.WaitFor(func(s *harness.Screen) bool { return s.Row(1).String() != "" }) // shell prompt

	// One command line, two phases: the marker on screen, then gone. `clear`
	// wipes the echoed command line, so the marker text in it can't leak into
	// the second phase.
	c.TypeLine(`clear; printf 'x\n  esc interrupt  tab agents\n'; sleep 4; clear; printf 'done\n'; sleep 60`)
	c.WaitForText("AGENT=busy")
	// The marker clears when the first sleep ends, past the default timeout.
	c.WaitForUntil(10*time.Second, func(s *harness.Screen) bool {
		return strings.Contains(s.String(), "AGENT=idle") // no bell: idle, not done
	})
}
