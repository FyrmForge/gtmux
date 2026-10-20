//go:build e2e

package e2e

import (
	"testing"

	"github.com/FyrmForge/gtmux/internal/harness"
)

// The status bar's centre shows the input mode: NORMAL at rest, PREFIX the
// moment the prefix key is pressed (no redraw from the server), COPY in copy
// mode with its position, VISUAL once a selection starts, and back to NORMAL.
// Copy mode never takes the bar over: the window list stays.
func TestModeIndicator(t *testing.T) {
	c := harness.Start(t)
	promptReady(c)
	c.WaitForStatus("NORMAL")

	c.Ctrl('b')
	c.WaitForStatus("PREFIX")
	c.Key('[') // enter copy mode
	c.WaitForStatus("COPY")
	c.WaitForStatus("line ")
	c.WaitForStatus("default") // session name: the bar is still there
	c.Key('v')
	c.WaitForStatus("VISUAL")
	c.Key('q')
	c.WaitForStatus("NORMAL")
}

// A rename prompt, by keybind or typed into the palette, reads the same in the
// top-centre box — "rename-window: <text>" — and the bar shows PROMPT.
func TestPromptBox(t *testing.T) {
	c := harness.Start(t)
	promptReady(c)

	c.Prefix(",")
	c.WaitForText("rename-window: ")
	c.WaitForStatus("PROMPT")
	c.Key(0x1b) // Esc cancels
	c.WaitForStatus("NORMAL")

	c.Prefix(":")
	c.WaitForStatus("COMMAND")
	c.Type("rename-window ")
	c.WaitForText("rename-window: ")
	c.Type("my win\r")
	c.WaitForStatus("my win")
}
