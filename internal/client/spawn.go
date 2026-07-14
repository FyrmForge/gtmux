package client

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/FyrmForge/gtmux/internal/proto"
)

// NewDetached is `gtmux new -d`: create a session without attaching (tmux's
// new-session -d), auto-starting the daemon first. The session runs on its own
// goroutine, ready for `gtmux run <name> ...` to build it and a later attach.
func NewDetached(name, groupTarget string) error {
	if err := ensureServer(); err != nil {
		return err
	}
	cwd, _ := os.Getwd()
	_, err := oneShot(&proto.ClientMsg{NewSession: &proto.NewSessionRequest{Name: name, Cwd: cwd, GroupTarget: groupTarget}})
	return err
}

// ensureServer makes sure a gtmux daemon is listening on the socket before
// attach proceeds, auto-spawning one (detached, logging to a file) if not —
// mirroring tmux's own "start the server if it isn't already running"
// behavior, without disturbing `gtmux server` run explicitly in a foreground
// pane.
func ensureServer() error {
	sockPath := proto.SockPath()
	if probeSocket(sockPath) {
		return nil
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate gtmux binary to auto-start server: %w", err)
	}

	dir := filepath.Dir(sockPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	logFile, err := os.OpenFile(filepath.Join(dir, "server.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("open server log: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(exe, "server")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("auto-start gtmux server: %w", err)
	}
	cmd.Process.Release()

	return waitForSocket(sockPath, 2*time.Second)
}

func probeSocket(sockPath string) bool {
	conn, err := net.DialTimeout("unix", sockPath, 200*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func waitForSocket(sockPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if probeSocket(sockPath) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for auto-started gtmux server on %s", sockPath)
}
