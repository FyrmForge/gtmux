package harness

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/FyrmForge/gtmux/internal/emu"
	"github.com/FyrmForge/gtmux/internal/geom"
)

// tmuxBackend runs the gtmux client inside a real tmux pane, so a foreign
// terminal emulator sits between gtmux and the assertions. Input goes in as
// raw bytes via `send-keys -H` (bypassing tmux key/mouse interpretation); the
// grid comes back from `capture-pane -e` re-parsed through a fresh
// emu.Terminal.
//
// Two modes:
//   - -backend=tmux: headless, a private tmux server per test
//     (sock/conf set, name is a session).
//   - -tmux-session=<name>: headed, one window per client inside
//     that session on the user's running tmux server (sock/conf empty, name
//     is a window id) — watch tests render live.
type tmuxBackend struct {
	sock       string // private tmux server socket; empty = user's default server
	conf       string // tmux.conf disabling the status line (private server only)
	name       string // tmux target: session name or window id
	cols, rows int
}

func (b *tmuxBackend) tmux(args ...string) ([]byte, error) {
	var full []string
	if b.sock != "" {
		full = append(full, "-S", b.sock, "-f", b.conf)
	}
	full = append(full, args...)
	return exec.Command("tmux", full...).CombinedOutput()
}

// takeoverAttach runs the client in an existing, user-designated window
// (-tmux-window=session:window) on the user's running tmux server:
// rename it to "E2E ▷ <test>" so the takeover is visible, force it to
// cols×rows, respawn its pane as the gtmux client. Cleanup respawns the shell
// and restores the window's name and sizing — the window itself survives.
func (s *serverProc) takeoverAttach(t *testing.T, target, verb, sess string, cols, rows int) backend {
	t.Helper()
	b := &tmuxBackend{cols: cols, rows: rows}
	id, err := b.tmuxLine("display", "-p", "-t", target, "#{window_id}")
	if err != nil {
		t.Fatalf("resolve window %q: %v", target, err)
	}
	b.name = id
	oldName, err := b.tmuxLine("display", "-p", "-t", id, "#{window_name}")
	if err != nil {
		t.Fatalf("read window name: %v", err)
	}
	// Window-local option values (empty = inherited) that the takeover will
	// clobber, implicitly or otherwise — captured so cleanup restores them.
	oldOpts := map[string]string{}
	for _, opt := range []string{"automatic-rename", "window-size", "remain-on-exit"} {
		oldOpts[opt], _ = b.tmuxLine("show", "-wv", "-t", id, opt)
	}
	for _, args := range [][]string{
		{"rename-window", "-t", id, "E2E ▷ " + t.Name()},
		{"resize-window", "-t", id, "-x", fmt.Sprint(cols), "-y", fmt.Sprint(rows)},
		// The client exiting must never take the user's window (and possibly
		// whole session) down with it.
		{"set", "-w", "-t", id, "remain-on-exit", "on"},
		{"respawn-pane", "-k", "-t", id,
			"env", "GTMUX_SOCK=" + s.sock, "XDG_CONFIG_HOME=" + s.cfgDir, s.bin, verb, sess},
	} {
		if out, err := b.tmux(args...); err != nil {
			t.Fatalf("tmux %s: %v\n%s", args[0], err, out)
		}
	}
	t.Cleanup(func() {
		// Explicit shell: a bare respawn-pane would re-run the gtmux client.
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "sh"
		}
		b.tmux("respawn-pane", "-k", "-t", id, shell)
		b.tmux("rename-window", "-t", id, oldName)
		for opt, val := range oldOpts {
			if val == "" {
				b.tmux("set", "-uw", "-t", id, opt) // was inherited: unset
			} else {
				b.tmux("set", "-w", "-t", id, opt, val)
			}
		}
	})
	return b
}

// tmuxLine runs a tmux command and returns its single-line output.
func (b *tmuxBackend) tmuxLine(args ...string) (string, error) {
	out, err := b.tmux(args...)
	if err != nil {
		return "", fmt.Errorf("%v\n%s", err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// windowAttach creates one window for the client inside target, a session on
// the user's running tmux server, sized to cols×rows (tmux pads the rest of
// the viewer's screen). Cleanup kills only that window.
func (s *serverProc) windowAttach(t *testing.T, target, verb, sess string, cols, rows int) backend {
	t.Helper()
	b := &tmuxBackend{cols: cols, rows: rows}
	out, err := b.tmux("new-window", "-t", target, "-P", "-F", "#{window_id}",
		"env", "GTMUX_SOCK="+s.sock, "XDG_CONFIG_HOME="+s.cfgDir, s.bin, verb, sess)
	if err != nil {
		t.Fatalf("tmux new-window in %q: %v\n%s", target, err, out)
	}
	b.name = strings.TrimSpace(string(out))
	// resize-window forces manual sizing, so the window stays cols×rows
	// regardless of the viewer's terminal size.
	if out, err := b.tmux("resize-window", "-t", b.name, "-x", fmt.Sprint(cols), "-y", fmt.Sprint(rows)); err != nil {
		t.Fatalf("tmux resize-window: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		b.tmux("kill-window", "-t", b.name)
	})
	return b
}

func (b *tmuxBackend) write(p []byte) error {
	args := []string{"send-keys", "-t", b.name, "-H"}
	for _, x := range p {
		args = append(args, fmt.Sprintf("%02x", x))
	}
	out, err := b.tmux(args...)
	if err != nil {
		return fmt.Errorf("tmux send-keys: %v\n%s", err, out)
	}
	return nil
}

// resize changes the tmux window's size; tmux delivers SIGWINCH to the pane.
func (b *tmuxBackend) resize(cols, rows int) error {
	out, err := b.tmux("resize-window", "-t", b.name, "-x", fmt.Sprint(cols), "-y", fmt.Sprint(rows))
	if err != nil {
		return fmt.Errorf("tmux resize-window: %v\n%s", err, out)
	}
	b.cols, b.rows = cols, rows
	return nil
}

func (b *tmuxBackend) snapshot() ([][]emu.Glyph, error) {
	out, err := b.tmux("capture-pane", "-e", "-p", "-t", b.name)
	if err != nil {
		return nil, fmt.Errorf("tmux capture-pane: %v\n%s", err, out)
	}
	term := emu.New(emu.WithSize(geom.Vec2{R: b.rows, C: b.cols}))
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	term.Write([]byte(strings.Join(lines, "\r\n")))
	return copyGrid(term), nil
}
