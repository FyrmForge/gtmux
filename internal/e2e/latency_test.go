//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/gtmux/internal/harness"
)

// TestActivePaneEchoUnderSiblingFlood guards interactive latency: background
// output in one pane must not leave the focused pane's PTY output sitting behind
// hundreds of flood chunks in the window actor queue.
func TestActivePaneEchoUnderSiblingFlood(t *testing.T) {
	c := harness.Start(t)
	promptReady(c)
	c.Run("run", "default", "split-window") // new bottom pane is active
	c.TypeLine("yes BACKGROUND-FLOOD")
	c.Run("run", "default", "select-pane", "-t", ":.0")

	started := time.Now()
	c.TypeLine("echo ACTIVE-PANE-READY")
	c.WaitForText("ACTIVE-PANE-READY")
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("active pane echo took %v under sibling flood", elapsed)
	}
}

// TestNeovimInsertLatency exercises the cursor-heavy partial updates that make
// stale-row bugs visible: each inserted byte must reach the rendered screen
// promptly, not merely move Neovim's cursor and appear on a later refresh.
func TestNeovimInsertLatency(t *testing.T) {
	testNeovimInsertLatency(t, "")
}

// TestNeovimInsertLatencyWithAnimatedDock covers the real client's other
// continuous output producer. Dock animation must not make pane frames queue
// behind repaints or split cursor/content updates.
func TestNeovimInsertLatencyWithAnimatedDock(t *testing.T) {
	clientLua := `
local frame = 0
local spin = { "a", "b", "c", "d" }
gtmux.widget{ dock = "left", size = 25, interval = 1, draw = function(c)
  frame = frame + 1
  c:box(0, 0, c.w, c.h, "fg=cyan,rounded")
  c:text(2, 0, spin[(frame % #spin) + 1], "fg=blue")
  for _, s in ipairs(gtmux.sessions()) do c:text(1, 2, s.name) end
  for _, p in ipairs(gtmux.find_panes({})) do c:text(1, 4, p.command) end
end }
`
	testNeovimInsertLatency(t, clientLua)
}

func testNeovimInsertLatency(t *testing.T, clientLua string) {
	t.Helper()
	c := harness.StartWithConfig(t, clientLua, "")
	promptReady(c)
	c.TypeLine("NVIM_LOG_FILE=/dev/null nvim --clean -u NONE")
	c.WaitForText("[No Name]")
	c.Key('i')

	want := ""
	for i := 0; i < 40; i++ {
		want += string(rune('a' + i%26))
		started := time.Now()
		c.Type(string(rune('a' + i%26)))
		c.WaitForText(want)
		if elapsed := time.Since(started); elapsed > 150*time.Millisecond {
			t.Fatalf("Neovim insert %d took %v", i, elapsed)
		}
	}

	// Keep strings imported in the same way the assertion is phrased if the
	// marker wraps on a narrower backend.
	if !strings.Contains(c.Screen().String(), want) {
		t.Fatalf("final Neovim line missing %q", want)
	}
}
