package client

import (
	"fmt"

	"github.com/FyrmForge/gtmux/internal/proto"
)

// KillSession asks the server to tear down a named session without
// attaching to it.
func KillSession(name string) error {
	if _, err := oneShot(&proto.ClientMsg{KillSession: &proto.KillSessionRequest{Name: name}}); err != nil {
		return err
	}
	fmt.Printf("killed session %s\n", name)
	return nil
}

// HasSession reports whether a session exists — nil if it does, an error
// otherwise (so the CLI can exit non-zero, tmux has-session's contract). Prints
// nothing: it's a scripting test.
func HasSession(name string) error {
	_, err := oneShot(&proto.ClientMsg{HasSession: &proto.HasSessionRequest{Name: name}})
	return err
}
