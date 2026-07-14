//go:build e2e

package e2e

import (
	"testing"

	"github.com/FyrmForge/gtmux/internal/harness"
)

// TestClientResizeRedraw covers the SIGWINCH path: grow then shrink the
// client's terminal; the status bar must follow the new bottom row, existing
// content must survive the repaint, and the client must stay interactive.
func TestClientResizeRedraw(t *testing.T) {
	c := harness.Start(t)
	promptReady(c)
	c.TypeLine("echo marker1")
	c.WaitForText("marker1")

	c.Resize(120, 40)
	c.WaitForStatus("default") // status bar re-rendered on the new row 39
	c.WaitForText("marker1")   // content repainted, not lost

	c.Resize(60, 20)
	c.WaitForStatus("default")
	c.TypeLine("echo marker2") // still interactive after shrink
	c.WaitForText("marker2")
}

// TestClientResizeRefitsSplit checks the layout re-fits on client resize: a
// 50/50 vertical split's divider sits near the middle both before and after
// the terminal grows.
func TestClientResizeRefitsSplit(t *testing.T) {
	c := harness.Start(t) // 80 wide -> divider around col 40
	promptReady(c)
	c.Prefix("%")
	c.WaitFor(func(s *harness.Screen) bool { return s.Col(3, '│') >= 0 })

	c.Resize(120, 30)
	// Proportional re-fit puts the divider near col 60; anything still at the
	// old col 40 (or missing) means the layout didn't follow the resize.
	c.WaitFor(func(s *harness.Screen) bool { return s.Col(3, '│') > 50 })
}

// TestPeerResizeRenegotiates covers live window-size renegotiation: the small
// client dot-fills while the acting peer is 190x9; when the peer grows, the
// window re-sizes and the dots must clear.
func TestPeerResizeRenegotiates(t *testing.T) {
	c := harness.Start(t) // 80x24
	c.WaitForStatus("default")
	big := c.NewPeer(190, 9)
	big.WaitForStatus("default")
	c.WaitFor(func(s *harness.Screen) bool { return s.Cell(10, 0).Char == '·' })

	big.Resize(190, 30) // window no longer shorter than the small client
	c.WaitFor(func(s *harness.Screen) bool { return s.Cell(10, 0).Char != '·' })
}

// TestTinyTerminal shrinks the client to near-nothing, keeps using it, and
// grows it back — no crash, no garbage, still interactive.
func TestTinyTerminal(t *testing.T) {
	c := harness.Start(t)
	promptReady(c)
	c.Resize(20, 6)
	c.WaitFor(func(s *harness.Screen) bool { return s.Status().String() != "" })
	c.Prefix("%") // split in a window with barely any room
	c.Prefix("%")
	c.WaitFor(func(s *harness.Screen) bool { return s.Status().String() != "" })

	c.Resize(80, 24)
	c.WaitForStatus("default")
	c.TypeLine("echo marker3")
	c.WaitForText("marker3")
}

// TestOddSizeSplit runs the split math at odd dimensions (79x23): the divider
// must land mid-screen and both halves must render.
func TestOddSizeSplit(t *testing.T) {
	c := harness.Start(t)
	promptReady(c)
	c.Resize(79, 23)
	c.WaitForStatus("default")
	c.Prefix("%")
	c.WaitFor(func(s *harness.Screen) bool {
		d := s.Col(3, '│')
		return d > 30 && d < 48
	})
	c.TypeLine("echo marker4") // active (right) pane usable at odd width
	c.WaitForText("marker4")
}
