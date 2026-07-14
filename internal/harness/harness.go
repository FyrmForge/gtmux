// Package harness is a full-stack e2e driver for gtmux: it spawns a real
// gtmux server and one or more real `gtmux attach` clients as subprocesses,
// each client on its own pty, and pipes the client's rendered output into an
// emu.Terminal so tests can assert on the on-screen grid (text and color) and
// synchronize with WaitFor instead of sleeps.
//
// Isolation: every Start() gets its own Unix socket under t.TempDir(), passed
// to both processes via GTMUX_SOCK, so tests never touch the real daemon and
// run in parallel. Everything is torn down via t.Cleanup.
package harness

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/FyrmForge/gtmux/internal/emu"
	"github.com/FyrmForge/gtmux/internal/geom"
)

// DefaultTimeout bounds every WaitFor. Generous so CI scheduling jitter
// doesn't flake; a real hang still fails in a couple seconds.
var DefaultTimeout = 2 * time.Second

// Test-binary flags, e.g.:
//
//	go test -tags=e2e ./internal/e2e/ -run TestCopyMode \
//	    -tmux-window ff-gtmux:2 -slowmo 200ms -start-wait 2s
var (
	flagBackend = flag.String("backend", "pty",
		"e2e backend: pty (default) or tmux (private tmux server per test, headless)")
	flagTmuxSession = flag.String("tmux-session", "",
		"headed: create client windows in this session on the running tmux server")
	flagTmuxWindow = flag.String("tmux-window", "",
		"headed: take over this session:window on the running tmux server (renamed 'E2E ▷ <test>' while in use, fully restored after)")
	slowMo = flag.Duration("slowmo", 0,
		"pause after every input action, for watching headed runs")
	startWait = flag.Duration("start-wait", 0,
		"one-time pause before the first test acts — time to find the window")
)

// startWaitOnce: -start-wait applies once per `go test` run, not per test.
var startWaitOnce sync.Once

// build the gtmux binary once per `go test` process, into a temp dir, so every
// test exercises the current source rather than a possibly-stale install.
var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

func gtmuxBin(t *testing.T) string {
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "gtmux-e2e-bin")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "gtmux")
		out, err := exec.Command("go", "build", "-o", binPath, "github.com/FyrmForge/gtmux/cmd/gtmux").CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("build gtmux: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binPath
}

// backend abstracts one attached client: raw keystroke bytes in, rendered
// grid out. ptyBackend (default) observes through a local pty + emu;
// tmuxBackend (-backend=tmux &/or -tmux-* flags) observes through a real tmux pane.
type backend interface {
	write(p []byte) error
	snapshot() ([][]emu.Glyph, error) // rows × cols rendered grid
	resize(cols, rows int) error      // change the client's terminal size
}

// serverProc is the shared daemon a set of clients attach to.
type serverProc struct {
	bin    string
	sock   string
	cmd    *exec.Cmd
	cfgDir string // XDG_CONFIG_HOME for this test's server + clients (isolated)

	// tmux backend only: this test's private tmux server, started lazily on
	// the first attach and killed via t.Cleanup.
	tmuxSock string
	tmuxConf string
	clientN  int
	tookOver bool // -tmux-window: designated window already claimed
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// sessionOf extracts the session part of a "session:window" tmux target.
func sessionOf(target string) string {
	if i := strings.IndexByte(target, ':'); i >= 0 {
		return target[:i]
	}
	return target
}

// Client is one attached gtmux client, driven and observed via its backend.
type Client struct {
	t          *testing.T
	srv        *serverProc
	sess       string
	be         backend
	cols, rows int
}

// copyGrid snapshots a terminal's cells into a plain grid.
func copyGrid(term emu.Terminal) [][]emu.Glyph {
	sz := term.Size()
	cells := make([][]emu.Glyph, sz.R)
	for y := 0; y < sz.R; y++ {
		cells[y] = make([]emu.Glyph, sz.C)
		for x := 0; x < sz.C; x++ {
			cells[y][x] = term.Cell(x, y)
		}
	}
	return cells
}

// Start builds gtmux, launches a server on an isolated socket, and attaches
// one 80x24 client to session "default". Returns that client.
func Start(t *testing.T) *Client {
	return StartWithConfig(t, "", "")
}

// StartWithConfig is Start plus optional client.lua / server.lua contents
// written into this test's isolated XDG_CONFIG_HOME before the server and
// client start — so tests can exercise config-driven binds (command-prompt,
// display-menu, display-popup, …). Empty strings write nothing (defaults).
func StartWithConfig(t *testing.T, clientLua, serverLua string) *Client {
	t.Helper()
	bin := gtmuxBin(t)

	dir := t.TempDir()
	// Keep the socket filename short: unix sun_path caps at ~108 bytes.
	sock := filepath.Join(dir, "s")
	// Isolate config from the developer's real ~/.config/gtmux so tests are
	// hermetic, and let a test inject its own client/server config.
	cfgDir := filepath.Join(dir, "config")
	for name, body := range map[string]string{"client.lua": clientLua, "server.lua": serverLua} {
		if body == "" {
			continue
		}
		p := filepath.Join(cfgDir, "gtmux", name)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}

	srvCmd := exec.Command(bin, "server")
	srvCmd.Env = append(os.Environ(), "GTMUX_SOCK="+sock, "XDG_CONFIG_HOME="+cfgDir)
	// Server log to a file in the temp dir for post-mortem; discard otherwise.
	if lf, err := os.Create(filepath.Join(dir, "server.log")); err == nil {
		srvCmd.Stdout, srvCmd.Stderr = lf, lf
	}
	if err := srvCmd.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	srv := &serverProc{bin: bin, sock: sock, cmd: srvCmd, cfgDir: cfgDir}
	t.Cleanup(func() {
		srvCmd.Process.Kill()
		srvCmd.Wait()
	})

	if err := waitListening(sock, DefaultTimeout); err != nil {
		t.Fatalf("server never came up: %v", err)
	}
	c := srv.attach(t, "default", 80, 24, true)
	startWaitOnce.Do(func() { time.Sleep(*startWait) })
	return c
}

// NewPeer attaches a second client of the given size to the same session and
// server — for multi-client tests (window-size negotiation, dot-fill).
func (c *Client) NewPeer(cols, rows int) *Client {
	c.t.Helper()
	return c.srv.attach(c.t, c.sess, cols, rows, false) // existing session
}

// NewPeerReadOnly attaches a read-only (attach -r) peer to the same session:
// it renders but its input never reaches a pane. Pty backend only.
func (c *Client) NewPeerReadOnly(cols, rows int) *Client {
	c.t.Helper()
	be := c.srv.ptyAttach(c.t, "attach", c.sess, cols, rows, "-r")
	return &Client{t: c.t, srv: c.srv, sess: c.sess, be: be, cols: cols, rows: rows}
}

// AttachSession attaches an 80x24 client to a different session name (created
// on demand) — for choose-session / multi-session tests.
func (c *Client) AttachSession(name string) *Client {
	c.t.Helper()
	return c.srv.attach(c.t, name, 80, 24, true) // creates the session on demand
}

// AttachGroup creates a new session joined to target's group (new-session -t),
// so it displays target's current windows. gtmux-only: the snapshot semantics
// differ from tmux's live-synced session groups, so there's no A/B parity.
func (c *Client) AttachGroup(name, target string) *Client {
	c.t.Helper()
	be := c.srv.ptyAttach(c.t, "new", name, 80, 24, "-t", target)
	return &Client{t: c.t, srv: c.srv, sess: name, be: be, cols: 80, rows: 24}
}

// Resize changes the client's terminal size mid-test (SIGWINCH via the pty;
// resize-window on tmux) — for live-resize scenarios.
func (c *Client) Resize(cols, rows int) {
	c.t.Helper()
	if err := c.be.resize(cols, rows); err != nil {
		c.t.Fatalf("resize client: %v", err)
	}
	c.cols, c.rows = cols, rows
}

// Run invokes a gtmux CLI subcommand against this test's daemon (e.g.
// Run("run", "default", "new-window") or Run("list")) and returns its output.
func (c *Client) Run(args ...string) string {
	c.t.Helper()
	cmd := exec.Command(c.srv.bin, args...)
	cmd.Env = append(os.Environ(), "GTMUX_SOCK="+c.srv.sock, "XDG_CONFIG_HOME="+c.srv.cfgDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		c.t.Logf("Run %v: %v", args, err)
	}
	return string(out)
}

// RunErr invokes a gtmux CLI subcommand and returns its exit error (nil on exit
// 0) — for commands whose contract is the exit code, like has-session.
func (c *Client) RunErr(args ...string) error {
	c.t.Helper()
	cmd := exec.Command(c.srv.bin, args...)
	cmd.Env = append(os.Environ(), "GTMUX_SOCK="+c.srv.sock, "XDG_CONFIG_HOME="+c.srv.cfgDir)
	return cmd.Run()
}

// attach spawns one client (as a pty subprocess, or inside a tmux pane when
// -backend=tmux or a -tmux-* flag) and wraps it in a Client.
func (s *serverProc) attach(t *testing.T, sess string, cols, rows int, create bool) *Client {
	t.Helper()
	verb := "attach"
	if create {
		verb = "new"
	}
	var be backend
	if *flagTmuxWindow != "" && !s.tookOver {
		// Only the first client fits in the designated window; peers fall
		// through to temporary windows in its session (next branch).
		s.tookOver = true
		be = s.takeoverAttach(t, *flagTmuxWindow, verb, sess, cols, rows)
	} else if target := firstNonEmpty(*flagTmuxSession, sessionOf(*flagTmuxWindow)); target != "" {
		be = s.windowAttach(t, target, verb, sess, cols, rows)
	} else if *flagBackend == "tmux" {
		be = s.tmuxAttach(t, verb, sess, cols, rows)
	} else {
		be = s.ptyAttach(t, verb, sess, cols, rows)
	}
	return &Client{t: t, srv: s, sess: sess, be: be, cols: cols, rows: rows}
}

// ptyAttach spawns the client subprocess on a local pty and starts feeding
// its output into a fresh emu.Terminal.
func (s *serverProc) ptyAttach(t *testing.T, verb, sess string, cols, rows int, extra ...string) backend {
	t.Helper()
	cmd := exec.Command(s.bin, append([]string{verb, sess}, extra...)...)
	cmd.Env = append(os.Environ(), "GTMUX_SOCK="+s.sock, "XDG_CONFIG_HOME="+s.cfgDir)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
	if err != nil {
		t.Fatalf("start client: %v", err)
	}
	b := &ptyBackend{ptmx: ptmx, term: emu.New(emu.WithSize(geom.Vec2{R: rows, C: cols}))}
	go b.readLoop()
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		ptmx.Close()
	})
	return b
}

// tmuxAttach runs the client inside a detached tmux session (one per client)
// on this test's private tmux server, started on first use.
func (s *serverProc) tmuxAttach(t *testing.T, verb, sess string, cols, rows int) backend {
	t.Helper()
	if s.tmuxSock == "" {
		dir := filepath.Dir(s.sock)
		s.tmuxSock = filepath.Join(dir, "t")
		// No tmux status line, so the pane is exactly the requested size.
		// (window-size manual is set per-session below: putting it in the
		// config file crashes the tmux 3.6a server on startup.)
		s.tmuxConf = filepath.Join(dir, "tmux.conf")
		if err := os.WriteFile(s.tmuxConf, []byte("set -g status off\n"), 0644); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			exec.Command("tmux", "-S", s.tmuxSock, "kill-server").Run()
		})
		t.Logf("tmux backend; watch live with: tmux -S %s attach", s.tmuxSock)
	}
	s.clientN++
	b := &tmuxBackend{
		sock: s.tmuxSock, conf: s.tmuxConf,
		name: fmt.Sprintf("c%d", s.clientN),
		cols: cols, rows: rows,
	}
	out, err := b.tmux("new-session", "-d",
		"-x", fmt.Sprint(cols), "-y", fmt.Sprint(rows), "-s", b.name,
		"env", "GTMUX_SOCK="+s.sock, "XDG_CONFIG_HOME="+s.cfgDir, s.bin, verb, sess)
	if err != nil {
		t.Fatalf("tmux new-session: %v\n%s", err, out)
	}
	// Keep the window at the requested size even if a headed viewer attaches.
	if out, err := b.tmux("set", "-w", "-t", b.name, "window-size", "manual"); err != nil {
		t.Fatalf("tmux set window-size: %v\n%s", err, out)
	}
	return b
}

// waitListening blocks until the daemon accepts connections on sock.
func waitListening(sock string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if conn, err := net.DialTimeout("unix", sock, 100*time.Millisecond); err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("no listener on %s after %v", sock, timeout)
}
