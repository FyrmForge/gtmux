package client

import (
	"fmt"

	"github.com/FyrmForge/gtmux/internal/proto"
)

// RenameSession asks the server to rename a session.
func RenameSession(oldName, newName string) error {
	if _, err := oneShot(&proto.ClientMsg{RenameSession: &proto.RenameSessionRequest{Old: oldName, New: newName}}); err != nil {
		return err
	}
	fmt.Printf("renamed session %s -> %s\n", oldName, newName)
	return nil
}
