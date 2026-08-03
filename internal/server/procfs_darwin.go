//go:build darwin

package server

import (
	"os/exec"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// procComm returns pid's short command name via sysctl kern.proc.pid — the
// pure-Go equivalent of tmux osdep-darwin's fallback path (its preferred
// proc_pidinfo needs libproc/cgo). Like tmux, the name truncates at the
// kernel's 16-char p_comm limit.
func procComm(pid int) string {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return ""
	}
	comm := kp.Proc.P_comm
	b := make([]byte, 0, len(comm))
	for _, c := range comm {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}

// procCwd returns pid's current working directory. tmux uses
// proc_pidinfo(PROC_PIDVNODEPATHINFO), but that's libproc/cgo territory;
// lsof reads the same kernel state. Only called at split/new-window time,
// never on a hot path.
// ponytail: ~50-100ms subprocess per split; swap for a cgo libproc call if
// that ever registers.
func procCwd(pid int) string {
	out, err := exec.Command("lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n") {
			return line[1:]
		}
	}
	return ""
}
