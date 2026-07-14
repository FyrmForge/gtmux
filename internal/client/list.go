package client

import (
	"encoding/gob"
	"fmt"
	"net"

	"github.com/FyrmForge/gtmux/internal/proto"
)

// List asks the server for its live sessions and prints them, one per line.
func List() error {
	conn, err := net.Dial("unix", proto.SockPath())
	if err != nil {
		return fmt.Errorf("connect to gtmux server (is it running?): %w", err)
	}
	defer conn.Close()

	enc := gob.NewEncoder(conn)
	dec := gob.NewDecoder(conn)

	if err := enc.Encode(&proto.ClientMsg{List: &proto.ListRequest{}}); err != nil {
		return err
	}

	var msg proto.ServerMsg
	if err := dec.Decode(&msg); err != nil {
		return err
	}
	if msg.SessionList == nil {
		return fmt.Errorf("unexpected server reply")
	}

	if len(msg.SessionList.Sessions) == 0 {
		fmt.Println("no sessions")
		return nil
	}
	for _, s := range msg.SessionList.Sessions {
		fmt.Printf("%s: %d windows\n", s.Name, s.Windows)
	}
	return nil
}
