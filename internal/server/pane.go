package server

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/FyrmForge/gtmux/internal/emu"
	"github.com/FyrmForge/gtmux/internal/geom"
	"github.com/FyrmForge/gtmux/internal/proto"
)

// historyLimit caps per-pane scrollback (tmux's history-limit). Seeded once in
// Run from the server config before any session spawns, then read-only — so
// it's a plain package var, no lock (config-time only, no runtime set-option).
var historyLimit = 2000

// defaultShell/defaultCommand are tmux's default-shell/default-command. Seeded
// once in Run from the server config (config-time, like historyLimit), then
// read-only, so plain package vars need no lock.
var defaultShell, defaultCommand string

// stripOptFlags drops leading set-option/show-options flags (-g/-w/-s/-a/-q/-u
// and -t <target>), returning the remaining name/value args. ponytail: the
// flags' scoping semantics are dropped, not honored — gtmux has one option
// scope per session.
func stripOptFlags(a []string) []string {
	for len(a) > 0 {
		switch {
		case a[0] == "-t" && len(a) > 1:
			a = a[2:]
		case a[0] != "--" && strings.HasPrefix(a[0], "-"):
			a = a[1:]
		default:
			return a
		}
	}
	return a
}

// popupDim parses a display-popup -w/-h value: "N" cells or "N%" of total.
// Unparseable → 0 (the caller clamps to a minimum).
func popupDim(s string, total int) int {
	if strings.HasSuffix(s, "%") {
		if n, err := strconv.Atoi(strings.TrimSuffix(s, "%")); err == nil {
			return total * n / 100
		}
		return 0
	}
	n, _ := strconv.Atoi(s)
	return n
}

// popupPos parses a display-popup -x/-y value: "N" cells, "N%" of total, or
// "C"/unparseable → -1 (center that axis).
func popupPos(s string, total int) int {
	if s == "C" || s == "" {
		return -1
	}
	if strings.HasSuffix(s, "%") {
		if n, err := strconv.Atoi(strings.TrimSuffix(s, "%")); err == nil {
			return total * n / 100
		}
		return -1
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return -1
	}
	return n
}

// nextPaneID hands out unique pane IDs, global for the whole daemon process
// (mirrors tmux's %N pane IDs, which are also daemon-global, not per-session).
var nextPaneID atomic.Int64

// pane is one shell running in its own PTY, with its own emu screen buffer,
// occupying rect within its window's composite screen.
type pane struct {
	id     int
	pty    *os.File
	cmd    *exec.Cmd
	term   emu.Terminal
	win    *window
	rect   rect
	marked bool // tagged by prefix+m as the join-pane source
	dead   bool // remain-on-exit: process exited but the pane is kept in the layout
	gen    int  // bumped on respawn; tags PTY-reader events so a stale reader's
	// output/exit (from the pre-respawn process) is dropped, not applied.
	// origin is the actor this pane's reader posts to (its birth window). It never
	// changes; break/join set origin.relay[p] so the origin forwards to the pane's
	// new window actor — keeping one ordered path (reader→origin→current) without
	// retargeting the unpausable reader. See WINDOW_ACTORS.md P3.0.
	origin *windowActor
	pipeW  io.WriteCloser // pipe-pane target (this pane's output is tee'd here), or nil
	ptScan ptScanner      // strips tmux allow-passthrough DCS wrappers from output
	// pane-border-status: the window-row reserved for this pane's border label
	// (-1 = none), and the last expanded pane-border-format text for it.
	borderRow   int
	borderLabel string
}

// parseSpawn pulls tmux's -c <dir> / -h / -v / -n <name> out of a
// new-window/split-window arg list; the leftover tokens joined are the command
// to run (empty = a plain shell). -d (don't-switch) is accepted and ignored —
// not modeled. name applies only to new-window (split-window ignores it).
// ponytail: space-joins the trailing tokens, so original shell quoting inside
// the command is lost; fine for the common `new-window npm run dev` case.
func parseSpawn(args []string) (dir, command, name string, horiz bool) {
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-c":
			if i+1 < len(args) {
				dir = args[i+1]
				i++
			}
		case "-n":
			if i+1 < len(args) {
				name = args[i+1]
				i++
			}
		case "-h":
			horiz = true
		case "-v", "-d":
			// -v is the default orientation; -d not modeled.
		default:
			rest = append(rest, args[i])
		}
	}
	return dir, strings.Join(rest, " "), name, horiz
}

// parseBind splits a bind-key/unbind-key arg list into (root, key, command):
// -n selects the no-prefix table, -T <table> is consumed (custom tables aren't
// runtime-settable), the first leftover token is the key, the rest the command.
func parseBind(args []string) (root bool, key string, cmd []string) {
	rest := args
	for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
		if rest[0] == "-n" {
			root = true
		} else if rest[0] == "-T" && len(rest) > 1 {
			rest = rest[1:]
		}
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return root, "", nil
	}
	return root, rest[0], rest[1:]
}

// resolveShell picks the shell for a new pane process: configured default-shell,
// else $SHELL, else /bin/sh (tmux's default-shell resolution).
func resolveShell() string {
	if defaultShell != "" {
		return defaultShell
	}
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	return "/bin/sh"
}

// paneEnv builds a new pane process's base environment. Truecolor is forced
// regardless of the server's inherited TERM/COLORTERM (gtmux's renderer always
// emits 24-bit color); GTMUX/GTMUX_PANE mirror tmux's TMUX/TMUX_PANE so apps in
// the pane can detect gtmux and which pane they're in.
func paneEnv(sessionName string, id int) []string {
	return append(os.Environ(),
		"TERM=xterm-256color", "COLORTERM=truecolor",
		fmt.Sprintf("GTMUX=%s,%d,%s", proto.SockPath(), os.Getpid(), sessionName),
		fmt.Sprintf("GTMUX_PANE=%%%d", id),
	)
}

// spawnPane starts a new shell in dir (the empty string leaves it to the
// shell's own default, generally the server process's cwd). command, if set,
// runs via `shell -c command` (tmux's new-window/split-window [command]) — the
// pane exits when it finishes, same as tmux. win == nil is a display-popup
// backing pane: no window wiring, held at session level (sessionName is passed
// explicitly since there's no window to read it from).
func spawnPane(win *window, sessionName string, r rect, dir, command string) (*pane, error) {
	shell := resolveShell()
	if command == "" {
		command = defaultCommand // empty leaves a login shell (below)
	}
	id := int(nextPaneID.Add(1))
	cmd := exec.Command(shell)
	if command != "" {
		cmd = exec.Command(shell, "-c", command)
	}
	cmd.Dir = dir
	cmd.Env = paneEnv(sessionName, id)
	if win != nil {
		// Environment: global (set-environment -g) first, then session env on top
		// so a session-scoped var overrides a global one — tmux's precedence.
		for k, v := range win.globalEnv {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		for k, v := range win.env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(r.Rows), Cols: uint16(r.Cols)})
	if err != nil {
		return nil, err
	}
	return &pane{
		id:  id,
		pty: f,
		cmd: cmd,
		// WithWriter(f): DSR/CPR-style query responses (e.g. vim asking for
		// cursor position) must be written back into the pty master so the
		// child process sees them as input, same as a real terminal would.
		// WithHistoryLimit: without one, scrollback grows unbounded for the
		// life of the pane; historyLimit is the configured history-limit.
		term:      emu.New(emu.WithSize(geom.Vec2{R: r.Rows, C: r.Cols}), emu.WithWriter(f), emu.WithHistoryLimit(historyLimit)),
		win:       win,
		rect:      r,
		borderRow: -1,
	}, nil
}

// respawn kills the pane's current process and starts a fresh one (the shell,
// or command) in the same slot — same id/rect/window, a new PTY + emu grid.
// Bumps gen so the old reader goroutine's trailing events are ignored; the
// caller must start a new watcher (watchPane) afterward.
func (p *pane) respawn(sessionName, command string) error {
	shell := resolveShell()
	cmd := exec.Command(shell)
	if command != "" {
		cmd = exec.Command(shell, "-c", command)
	}
	cmd.Dir = p.cwd()
	cmd.Env = paneEnv(sessionName, p.id)
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(p.rect.Rows), Cols: uint16(p.rect.Cols)})
	if err != nil {
		return err
	}
	if p.pipeW != nil {
		p.pipeW.Close()
		p.pipeW = nil
	}
	p.pty.Close() // SIGHUP the old process
	p.pty = f
	p.cmd = cmd
	p.gen++
	p.term = emu.New(emu.WithSize(geom.Vec2{R: p.rect.Rows, C: p.rect.Cols}), emu.WithWriter(f), emu.WithHistoryLimit(historyLimit))
	p.ptScan = ptScanner{} // drop any half-parsed passthrough from the dead process
	p.dead = false         // reviving a remain-on-exit dead pane
	return nil
}

// currentCommand reports the name of the pane's foreground process (tmux's
// pane_current_command): the process group currently in control of the pty,
// not just the login shell, so it tracks into vim/nvim/etc.
func (p *pane) currentCommand() string {
	pgid, err := unix.IoctlGetInt(int(p.pty.Fd()), unix.TIOCGPGRP)
	if err != nil {
		return ""
	}
	comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pgid))
	if err != nil {
		return ""
	}
	name := string(comm)
	if n := len(name); n > 0 && name[n-1] == '\n' {
		name = name[:n-1]
	}
	return name
}

func (p *pane) resize(r rect) {
	p.rect = r
	pty.Setsize(p.pty, &pty.Winsize{Rows: uint16(r.Rows), Cols: uint16(r.Cols)})
	p.term.Resize(geom.Vec2{R: r.Rows, C: r.Cols})
}

// cwd reads the pane's shell's actual current directory via /proc, which
// reflects any `cd`s the user has run since the shell started.
func (p *pane) cwd() string {
	link, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", p.cmd.Process.Pid))
	if err != nil {
		return ""
	}
	return link
}

func (p *pane) Close() {
	if p.dead {
		// remain-on-exit already reaped the process and closed the pty in
		// markDead; nothing live to kill.
		return
	}
	if p.pipeW != nil {
		p.pipeW.Close()
		p.pipeW = nil
	}
	p.pty.Close()
	p.cmd.Process.Kill()
	p.cmd.Wait()
}

// markDead reaps the pane's already-exited process (no kill), closes its pty,
// and marks it dead so it stays frozen in the layout (remain-on-exit). Returns
// the process exit code, for the "failed" policy. respawn clears p.dead.
func (p *pane) markDead() int {
	if p.pipeW != nil {
		p.pipeW.Close()
		p.pipeW = nil
	}
	p.pty.Close()
	p.cmd.Wait() // the reader hit EOF, so the process is already gone
	p.dead = true
	if st := p.cmd.ProcessState; st != nil {
		return st.ExitCode()
	}
	return 0
}

// cursor reports the pane-local cursor position to show: the pane's own
// terminal cursor.
func (p *pane) cursor() emu.Cursor {
	return p.term.Cursor()
}

func (p *pane) cursorVisible() bool {
	return p.term.CursorVisible()
}

// wantsMouse reports whether the pane's app has requested mouse tracking, for
// PaneRect.WantsMouse (the client's own-vs-forward decision). Nil-safe so
// layout-geometry tests can build panes without a term.
func (p *pane) wantsMouse() bool {
	return p.term != nil && p.term.Mode()&emu.ModeMouseMask != 0
}

// keyFlags reports the pane app's kitty-keyboard progressive-enhancement flags
// (0 = legacy), for PaneRect.KeyFlags — the client negotiates the same with its
// outer terminal. Nil-safe for layout-geometry tests.
func (p *pane) keyFlags() int {
	if p.term == nil {
		return 0
	}
	return int(p.term.KeyState())
}

// copySnapshot freezes the pane's scrollback + current screen for a client to
// browse in copy-mode locally, with the cursor at the live cursor position.
// Copy-mode is client-side now; the server just hands over the frozen buffer.
func (p *pane) copySnapshot() *proto.CopyModeEnter {
	hist := p.term.History()
	lines := make([]emu.Line, 0, len(hist)+p.rect.Rows)
	lines = append(lines, hist...)
	for y := 0; y < p.rect.Rows; y++ {
		line := make(emu.Line, p.rect.Cols)
		for x := 0; x < p.rect.Cols; x++ {
			line[x] = p.term.Cell(x, y)
		}
		lines = append(lines, line)
	}
	cur := p.term.Cursor()
	return &proto.CopyModeEnter{
		PaneID:  p.id,
		Lines:   lines,
		CursorY: len(lines) - p.rect.Rows + cur.R,
		CursorX: cur.C,
	}
}

// fullContent reports every cell of the pane, for messages that also carry
// a fresh Layout — the client has no prior per-pane buffer to diff against
// once the arrangement itself just changed (attach, split, close, resize,
// window switch).
func (p *pane) fullContent() proto.PaneContent {
	p.term.Changes().Reset()
	lines := make(map[int]emu.Line, p.rect.Rows)
	for y := 0; y < p.rect.Rows; y++ {
		line := make(emu.Line, p.rect.Cols)
		for x := 0; x < p.rect.Cols; x++ {
			line[x] = p.term.Cell(x, y)
		}
		lines[y] = line
	}
	return proto.PaneContent{
		PaneID: p.id, Lines: lines, Cursor: p.cursor(), CursorVisible: p.cursorVisible(),
	}
}

// dirtyContent reports only the pane's rows that changed since the last
// call (or since fullContent last reset them) — the hot path for ordinary
// pty output.
func (p *pane) dirtyContent() proto.PaneContent {
	changes := p.term.Changes()
	lines := make(map[int]emu.Line, len(changes.Lines))
	for localRow := range changes.Lines {
		if localRow < 0 || localRow >= p.rect.Rows {
			continue
		}
		line := make(emu.Line, p.rect.Cols)
		for x := 0; x < p.rect.Cols; x++ {
			line[x] = p.term.Cell(x, localRow)
		}
		lines[localRow] = line
	}
	changes.Reset()
	return proto.PaneContent{
		PaneID: p.id, Lines: lines, Cursor: p.cursor(), CursorVisible: p.cursorVisible(),
	}
}
