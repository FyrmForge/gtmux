//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/gtmux/internal/harness"
)

// select-pane -t %N into ANOTHER window (what a sidebar agent-row click sends)
// must be a real window switch: it used to set the current window without
// activating its view, so the pane painted once and then froze in the client
// while the daemon's grid kept updating.
func TestSelectPaneCrossWindowKeepsRendering(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("default")
	c.TypeLine("echo win-one''-marker")
	c.WaitForText("win-one-marker")
	lp := c.Run("run", "default", "list-panes")
	i := strings.Index(lp, "%")
	if i < 0 {
		t.Fatalf("no pane id in list-panes: %q", lp)
	}
	j := i + 1
	for j < len(lp) && lp[j] >= '0' && lp[j] <= '9' {
		j++
	}
	id := lp[i:j]
	c.Run("run", "default", "new-window")
	c.TypeLine("echo win-two''-marker")
	c.WaitForText("win-two-marker")
	c.Run("run", "default", "select-pane", "-t", id)
	c.WaitForText("win-one-marker")
	c.TypeLine("echo still''-live")
	deadline := time.Now().Add(4 * time.Second)
	for !strings.Contains(c.Screen().String(), "still-live") {
		if time.Now().After(deadline) {
			t.Fatalf("pane froze after cross-window select-pane\nclient screen:\n%s", c.Screen().String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}
