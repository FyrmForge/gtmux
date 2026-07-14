package client

import (
	"encoding/gob"
	"fmt"
	"net"

	"github.com/FyrmForge/gtmux/internal/proto"
)

// KillServer shuts the whole daemon down (`gtmux kill-server`). The server
// exits immediately after acking, so a closed connection (no ack read back)
// is success, not failure — only a failure to connect is an error.
func KillServer() error {
	conn, err := net.Dial("unix", proto.SockPath())
	if err != nil {
		return fmt.Errorf("connect to gtmux server (is it running?): %w", err)
	}
	defer conn.Close()
	return gob.NewEncoder(conn).Encode(&proto.ClientMsg{KillServer: &proto.KillServerRequest{}})
}

// Command executes one command-mode command in a session from outside
// (`gtmux run <session> <command...>`).
func Command(session string, args []string) (string, error) {
	ack, err := oneShot(&proto.ClientMsg{Command: &proto.CommandRequest{Session: session, Args: args}})
	if err != nil {
		return "", err
	}
	return ack.Out, nil
}

// oneShot sends a single request that expects a single Ack reply, for
// requests that don't attach a terminal to a session (kill/rename/...).
func oneShot(msg *proto.ClientMsg) (*proto.Ack, error) {
	conn, err := net.Dial("unix", proto.SockPath())
	if err != nil {
		return nil, fmt.Errorf("connect to gtmux server (is it running?): %w", err)
	}
	defer conn.Close()

	if err := gob.NewEncoder(conn).Encode(msg); err != nil {
		return nil, err
	}

	var reply proto.ServerMsg
	if err := gob.NewDecoder(conn).Decode(&reply); err != nil {
		return nil, err
	}
	if reply.Ack == nil {
		return nil, fmt.Errorf("unexpected server reply")
	}
	if !reply.Ack.Ok {
		return nil, fmt.Errorf("%s", reply.Ack.Err)
	}
	return reply.Ack, nil
}
