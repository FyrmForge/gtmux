package client

import (
	"encoding/gob"
	"fmt"
	"net"

	"github.com/FyrmForge/gtmux/internal/proto"
)

// List asks the server for its live sessions and prints them, one per line.
func List() error {
	sessions, err := listSessions()
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		fmt.Println("no sessions")
		return nil
	}
	for _, s := range sessions {
		fmt.Printf("%s: %d windows\n", s.Name, s.Windows)
	}
	return nil
}

// listSessions asks the server for its live sessions.
func listSessions() ([]proto.SessionInfo, error) {
	conn, err := net.Dial("unix", proto.SockPath())
	if err != nil {
		return nil, fmt.Errorf("connect to gtmux server (is it running?): %w", err)
	}
	defer conn.Close()
	if err := gob.NewEncoder(conn).Encode(&proto.ClientMsg{List: &proto.ListRequest{}}); err != nil {
		return nil, err
	}
	var msg proto.ServerMsg
	if err := gob.NewDecoder(conn).Decode(&msg); err != nil {
		return nil, err
	}
	if msg.SessionList == nil {
		return nil, fmt.Errorf("unexpected server reply")
	}
	return msg.SessionList.Sessions, nil
}
