//go:build linux

package server

import (
	"fmt"
	"os"
	"strings"
)

// procComm returns pid's short command name via /proc.
func procComm(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(string(b), "\n")
}

// procCwd returns pid's current working directory via /proc.
func procCwd(pid int) string {
	link, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
	if err != nil {
		return ""
	}
	return link
}
