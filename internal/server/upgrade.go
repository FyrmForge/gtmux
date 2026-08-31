package server

import (
	"encoding/gob"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/FyrmForge/gtmux/internal/emu"
	"github.com/FyrmForge/gtmux/internal/geom"
	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// In-place upgrade: the daemon dumps its session shape to a gob file, marks
// its PTY masters + listening socket inheritable, and execs the installed
// binary with --resume. Same PID, so the pane processes stay our children;
// clients are told to reconnect (SwitchSession to their own session) and land
// on the new image. Pane contents are carried as rendered text and replayed
// into a fresh emu, then each child gets a size bounce (SIGWINCH) so
// full-screen apps repaint themselves — see the design note in HISTORY.md.
//
// ponytail: v1 drops popups, pipe-pane targets, remain-on-exit corpses, and
// per-session scalar option overrides (they reset to config); a window linked
// into several sessions is adopted by the first only.

type savedPane struct {
	ID, PID, FD          int
	Row, Col, Rows, Cols int
	Marked               bool
	Content              string // rendered history + screen, CRLF-separated, SGR kept
	CurR, CurC           int
	Modes                string // escape sequences re-requesting the app's emu modes (mouse, 2004, DECCKM, kitty) — see modeSeqs
}

type savedNode struct {
	Pane int // pane id for a leaf; -1 for a split
	Dir  int
	Frac float64
	A, B *savedNode
}

type savedWindow struct {
	Manual     *string
	AutoName   string
	Opts       map[string]string
	Zoomed     bool
	LayoutName string
	MainW      int
	MainH      int
	Root       *savedNode
	Active     int // pane id
	Panes      []savedPane
}

type savedBuffer struct{ Name, Data string }

type savedSession struct {
	Name       string
	Env        map[string]string
	UserOpts   map[string]string
	CmdAlias   map[string]string
	Hooks      map[string][]string
	Buffers    []savedBuffer
	Active     int
	LastWindow int
	Cols, Rows int // the session's grid vote, so reattaching clients don't force a resize
	Windows    []savedWindow
}

type savedServer struct {
	Sessions     []savedSession
	GlobalEnv    map[string]string
	ClientOpts   map[string]string
	UserOpts     map[string]string
	RuntimeBinds map[string]string
	LastSession  string
	NextPaneID   int64
	ListenFD     int
	ParkedFDs    []int // client connections accepted mid-upgrade, not yet read
}

// dumpEvent asks a session goroutine for its savedSession. Handling it also
// tells the session's clients to reconnect and freezes the session (no more
// events applied) so nothing changes between the dump and the exec.
type dumpEvent struct{ reply chan savedSession }

// unfreezeEvent lifts the dump freeze if an in-place upgrade aborts (the exec
// failed), so the session keeps serving instead of being deaf forever.
type unfreezeEvent struct{}

// modeSeqs renders the mode state a pane's app had requested as the escape
// sequences that request it, replayed into the resumed image's fresh emu.
// Sequences rather than raw ModeFlag bits: the gob crosses binary versions
// (old image writes, new image reads), and bit layouts can shift between
// them while the wire sequences never do. Without this, an upgrade silently
// dropped mouse tracking, bracketed paste, app-cursor keys, and kitty
// keyboard flags in every running full-screen app until it was restarted.
// ponytail: alt-screen and the kitty push-stack aren't carried — replaying
// 1049h would blank the replayed content, and the size bounce makes the app
// repaint (and re-assert cursor state) anyway; only the current kitty flags
// survive, not flags pushed beneath them.
func modeSeqs(t emu.Terminal) string {
	var b strings.Builder
	mode := t.Mode()
	for _, m := range []struct {
		bit emu.ModeFlag
		num int
	}{
		{emu.ModeAppCursor, 1},
		{emu.ModeMouseX10, 9},
		{emu.ModeMouseButton, 1000},
		{emu.ModeMouseMotion, 1002},
		{emu.ModeMouseMany, 1003},
		{emu.ModeFocus, 1004},
		{emu.ModeMouseSgr, 1006},
		{emu.ModeBracketedPaste, 2004},
	} {
		if mode&m.bit != 0 {
			fmt.Fprintf(&b, "\x1b[?%dh", m.num)
		}
	}
	if f := t.KeyState(); f != 0 {
		fmt.Fprintf(&b, "\x1b[=%d;1u", int(f))
	}
	return b.String()
}

// dumpPane renders a pane's scrollback + screen with attributes kept, so the
// replay into a fresh emu restores both text and colors.
func dumpPane(p *pane) savedPane {
	var b strings.Builder
	for _, l := range p.term.History() {
		b.WriteString(strings.TrimRight(emu.RenderLine(l), " "))
		b.WriteString("\r\n")
	}
	screen := p.term.Screen()
	for i, l := range screen {
		b.WriteString(strings.TrimRight(emu.RenderLine(l), " "))
		if i < len(screen)-1 {
			b.WriteString("\r\n")
		}
	}
	cur := p.term.Cursor()
	return savedPane{
		ID: p.id, PID: p.cmd.Process.Pid, FD: rawFD(p.pty),
		Row: p.rect.Row, Col: p.rect.Col, Rows: p.rect.Rows, Cols: p.rect.Cols,
		Marked: p.marked, Content: b.String(), CurR: cur.R, CurC: cur.C,
		Modes: modeSeqs(p.term),
	}
}

func dumpNode(n *layoutNode) *savedNode {
	if n == nil {
		return nil
	}
	if n.pane != nil {
		return &savedNode{Pane: n.pane.id}
	}
	return &savedNode{Pane: -1, Dir: int(n.dir), Frac: n.frac, A: dumpNode(n.a), B: dumpNode(n.b)}
}

// dumpWindow runs on the window's actor goroutine (actorDo) — it reads panes
// and emu grids the actor owns.
func dumpWindow(w *window) savedWindow {
	sw := savedWindow{
		Manual: w.manualName, AutoName: w.autoName, Opts: w.opts, Zoomed: w.zoomed,
		LayoutName: w.layoutName, MainW: w.mainW, MainH: w.mainH, Root: dumpNode(w.root),
	}
	if w.active != nil {
		sw.Active = w.active.id
	}
	for _, p := range w.panes {
		if p.dead {
			continue // no process, no fd: dropped from the layout on upgrade
		}
		sw.Panes = append(sw.Panes, dumpPane(p))
	}
	return sw
}

// rawFD returns f's descriptor without switching it to blocking mode (Fd()
// would), so the reader goroutine and the new image's poller stay happy.
func rawFD(f *os.File) int {
	fd := -1
	if rc, err := f.SyscallConn(); err == nil {
		rc.Control(func(x uintptr) { fd = int(x) })
	}
	return fd
}

// inherit clears FD_CLOEXEC so the descriptor survives the exec.
func inherit(fd int) {
	unix.FcntlInt(uintptr(fd), unix.F_SETFD, 0)
}

// upgrade is the server-side handler for `gtmux upgrade`. Runs on a
// connection goroutine. Returns only on failure.
func (r *registry) upgrade(ln *os.File) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.upgrading = true
	names := make([]string, 0, len(r.sessions))
	for n := range r.sessions {
		names = append(names, n)
	}
	sort.Strings(names)
	r.mu.Unlock()

	st := savedServer{
		GlobalEnv: r.globalEnvCopy(), ClientOpts: r.clientOptsCopy(), UserOpts: r.userOptsCopy(),
		LastSession: r.getLastSession(), NextPaneID: nextPaneID.Load(), ListenFD: rawFD(ln),
	}
	r.mu.Lock()
	st.RuntimeBinds = map[string]string{}
	for k, v := range r.runtimeBinds {
		st.RuntimeBinds[k] = v
	}
	r.mu.Unlock()
	for _, n := range names {
		s, ok := r.get(n)
		if !ok {
			continue
		}
		reply := make(chan savedSession, 1)
		s.events <- dumpEvent{reply: reply}
		select {
		case ss := <-reply:
			st.Sessions = append(st.Sessions, ss)
		case <-time.After(5 * time.Second):
			return fmt.Errorf("session %s did not answer the dump", n)
		}
	}
	// Let the writer goroutines flush the SwitchSession handoffs before the
	// exec closes the client connections. ponytail: fixed grace instead of
	// per-attachment drain tracking.
	time.Sleep(300 * time.Millisecond)

	r.mu.Lock()
	for _, f := range r.parked {
		fd := rawFD(f)
		inherit(fd)
		st.ParkedFDs = append(st.ParkedFDs, fd)
	}
	r.mu.Unlock()
	inherit(st.ListenFD)
	for _, ss := range st.Sessions {
		for _, w := range ss.Windows {
			for _, p := range w.Panes {
				inherit(p.FD)
			}
		}
	}

	f, err := os.CreateTemp("", "gtmux-upgrade-*.gob")
	if err != nil {
		return err
	}
	if err := gob.NewEncoder(f).Encode(&st); err != nil {
		f.Close()
		return err
	}
	f.Close()
	log.Printf("gtmux: upgrading in place → %s (%d sessions)", exe, len(st.Sessions))
	return syscall.Exec(exe, []string{os.Args[0], "server", "--resume", f.Name()}, os.Environ())
}

// park holds a connection accepted while an upgrade is in flight: its fd is
// handed to the new image, which reads the client's request there. Returns
// false (not upgrading) so the caller handles the connection normally.
func (r *registry) park(f *os.File) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.upgrading {
		return false
	}
	r.parked = append(r.parked, f)
	return true
}

// loadResume reads the state file written by the previous image and removes it.
func loadResume(path string) (*savedServer, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	defer os.Remove(path)
	var st savedServer
	if err := gob.NewDecoder(f).Decode(&st); err != nil {
		return nil, err
	}
	return &st, nil
}

// adoptPane wraps an inherited PTY + live child into a pane, replaying the
// saved rendering into a fresh emu.
func adoptPane(w *window, sp savedPane) (*pane, error) {
	f := os.NewFile(uintptr(sp.FD), fmt.Sprintf("pty-%d", sp.ID))
	if f == nil {
		return nil, fmt.Errorf("pane %%%d: bad fd %d", sp.ID, sp.FD)
	}
	proc, err := os.FindProcess(sp.PID)
	if err != nil {
		return nil, err
	}
	r := rect{sp.Row, sp.Col, sp.Rows, sp.Cols}
	p := &pane{
		id:  sp.ID,
		pty: f,
		// A Cmd with only Process set: Wait/Kill/Pid work on it (the process is
		// still our child — exec keeps the PID), which is all the pane uses.
		cmd:       &exec.Cmd{Process: proc},
		term:      emu.New(emu.WithSize(geom.Vec2{R: r.Rows, C: r.Cols}), emu.WithWriter(f), emu.WithHistoryLimit(historyLimit)),
		win:       w,
		rect:      r,
		marked:    sp.Marked,
		borderRow: -1,
	}
	p.term.Write([]byte(sp.Content))
	p.term.Write([]byte(fmt.Sprintf("\x1b[0m\x1b[%d;%dH", sp.CurR+1, sp.CurC+1)))
	p.term.Write([]byte(sp.Modes))
	return p, nil
}

// bounceSize nudges the child's window size so the kernel raises SIGWINCH:
// full-screen apps (vim, htop, claude) repaint from scratch, replacing the
// replayed approximation with their own true state.
func bounceSize(p *pane) {
	pty.Setsize(p.pty, &pty.Winsize{Rows: uint16(p.rect.Rows + 1), Cols: uint16(p.rect.Cols)})
	pty.Setsize(p.pty, &pty.Winsize{Rows: uint16(p.rect.Rows), Cols: uint16(p.rect.Cols)})
}

// resumeWindow rebuilds a window from its saved shape over adopted panes. Leaves
// whose pane didn't make it (dead, bad fd) are pruned from the tree.
func resumeWindow(sw savedWindow, cols, rows int, sessionName string, env, globalEnv map[string]string) (*window, error) {
	w := &window{sessionName: sessionName, env: env, globalEnv: globalEnv, cols: cols, rows: rows,
		manualName: sw.Manual, autoName: sw.AutoName, opts: sw.Opts, zoomed: sw.Zoomed,
		layoutName: sw.LayoutName, mainW: sw.MainW, mainH: sw.MainH}
	byID := map[int]*pane{}
	for _, sp := range sw.Panes {
		p, err := adoptPane(w, sp)
		if err != nil {
			log.Printf("gtmux: upgrade: %v (pane dropped)", err)
			continue
		}
		byID[sp.ID] = p
		w.panes = append(w.panes, p)
	}
	if len(w.panes) == 0 {
		return nil, fmt.Errorf("window has no live panes")
	}
	var build func(n *savedNode) *layoutNode
	build = func(n *savedNode) *layoutNode {
		if n == nil {
			return nil
		}
		if n.Pane >= 0 {
			if p := byID[n.Pane]; p != nil {
				return &layoutNode{pane: p}
			}
			return nil
		}
		a, b := build(n.A), build(n.B)
		switch {
		case a == nil:
			return b
		case b == nil:
			return a
		}
		return &layoutNode{dir: splitDir(n.Dir), frac: n.Frac, a: a, b: b}
	}
	w.root = build(sw.Root)
	if w.root == nil {
		w.root = &layoutNode{pane: w.panes[0]}
	}
	if w.zoomed && len(w.panes) < 2 {
		w.zoomed = false
	}
	act := byID[sw.Active]
	if act == nil {
		act = w.panes[0]
	}
	w.setActive(act)
	w.reflow()
	return w, nil
}
