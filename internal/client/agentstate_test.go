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
