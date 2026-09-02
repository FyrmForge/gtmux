package client

import (
	"testing"

	"github.com/FyrmForge/gtmux/internal/config"
	"github.com/FyrmForge/gtmux/internal/proto"
)

// snap builds a one-session/one-window/one-pane snapshot for the state machine.
func snap(cmd, title string, bell, focused bool) *proto.StateSnapshot {
	return &proto.StateSnapshot{Sessions: []proto.SnapSession{{
		Name: "s", Attached: focused,
		Windows: []proto.SnapWindow{{
			Index: 0, Active: focused, Bell: bell,
			Panes: []proto.PaneInfo{{ID: 7, Command: cmd, Title: title, Active: focused}},
		}},
	}}}
}

func TestDetectAgentStates(t *testing.T) {
	c := newCompositor()
	c.agentDefs = []config.AgentDef{{Match: "claude", Busy: "✳"}}

	c.detectAgentStates(snap("claude", "✳ thinking", false, false)) // seed tick
	if got := c.drainAgentChanges(); len(got) != 0 {
		t.Fatalf("seed tick fired changes: %+v", got)
	}
	if c.agentState[7] != "busy" {
		t.Fatalf("state after seed = %q, want busy", c.agentState[7])
	}

	c.detectAgentStates(snap("claude", "done summary", true, false)) // bell edge
	ch := c.drainAgentChanges()
	if len(ch) != 1 || ch[0].state != "done" || ch[0].pane != 7 {
		t.Fatalf("bell edge changes = %+v, want one done for pane 7", ch)
	}

	c.detectAgentStates(snap("claude", "done summary", true, true)) // focus acks
	ch = c.drainAgentChanges()
	if len(ch) != 1 || ch[0].state != "idle" {
		t.Fatalf("focus ack changes = %+v, want one idle", ch)
	}

	c.detectAgentStates(snap("zsh", "", false, false)) // agent exited
	if _, ok := c.agentState[7]; ok {
		t.Fatal("state survived the agent exiting")
	}
}

// opencode never marks the title while working and never rings the bell — its
// only live signal is an "esc interrupt" hint on the pane's bottom rows, which
// the server ships as PaneInfo.ScreenTail. Without busy_screen the pane sat at
// idle forever, so the dock showed a permanent "awaiting you" flag.
func TestDetectAgentStatesBusyFromScreenTail(t *testing.T) {
	c := newCompositor()
	c.agentDefs = []config.AgentDef{{Match: "opencode", BusyScreen: "esc interrupt"}}
	tail := func(s string) *proto.StateSnapshot {
		sn := snap("opencode", "OC | count to ten", false, false)
		sn.Sessions[0].Windows[0].Panes[0].ScreenTail = s
		return sn
	}

	c.detectAgentStates(tail("  /tmp   9.1K (1%)\n")) // seed: not working
	c.drainAgentChanges()
	if c.agentState[7] != "idle" {
		t.Fatalf("state with no marker = %q, want idle", c.agentState[7])
	}

	c.detectAgentStates(tail(" ⬝⬝■■■■  esc interrupt      tab agents\n"))
	ch := c.drainAgentChanges()
	if len(ch) != 1 || ch[0].state != "busy" || ch[0].pane != 7 {
		t.Fatalf("screen marker changes = %+v, want one busy for pane 7", ch)
	}

	// Marker gone and no bell: opencode goes back to idle, not done.
	c.detectAgentStates(tail("  /tmp   9.1K (1%)\n"))
	ch = c.drainAgentChanges()
	if len(ch) != 1 || ch[0].state != "idle" {
		t.Fatalf("marker cleared changes = %+v, want one idle", ch)
	}
}
