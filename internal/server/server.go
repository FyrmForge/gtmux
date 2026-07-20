// Package server implements the gtmux daemon. One daemon process serves
// every session; each session is a single goroutine that owns its
// windows/panes and runs independently of whether a client is attached, so
// detaching and reattaching never disturbs its state.
package server

import (
	"encoding/gob"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/FyrmForge/gtmux/internal/config"
	"github.com/FyrmForge/gtmux/internal/proto"
)

// registry is the daemon's lookup of live sessions by name. It's touched
// only rarely (session create/lookup/remove, not the PTY/redraw hot path),
// so a plain mutex is simpler here than routing everything through channels.
type registry struct {
	mu       sync.Mutex
	sessions map[string]*session
	nameFmt  string // fmt template for auto-named sessions; contains %d
	// lastSession is the name a client last switched away from — the target for
	// switch-client -l. ponytail: server-global, not per-client (gtmux clients
	// have no persistent identity across the switch handoff); good enough for the
	// common single-client case, per-client if multi-client -l ever matters.
	lastSession string
	// runtimeBinds records runtime bind-key/unbind-key commands (id → display
	// line) so list-keys can report them. Config binds live in each client's Lua
	// VM and are opaque here, so list-keys shows only these runtime ones.
	runtimeBinds map[string]string
	// mainPaneW/H size the main pane in main-vertical/main-horizontal layouts
	// (tmux's main-pane-width/height); read by each session's owner goroutine.
	mainPaneW, mainPaneH int
	// baseIndex/paneBaseIndex offset displayed+targeted window/pane numbers.
	baseIndex, paneBaseIndex int
	// displayTime is how long a transient status message stays up, in ms. Read
	// once by each session's owner goroutine (like base-index) and overridable
	// per session at runtime via set-option.
	displayTime int
	// messageLimit caps each session's show-messages log (tmux's message-limit).
	messageLimit int
	// autoRename/allowRename/autoRenameFmt drive window naming; read once per
	// session goroutine (like base-index) and runtime-overridable per session.
	autoRename, allowRename bool
	autoRenameFmt           string
	// session lifecycle policy (read once per session goroutine).
	destroyUnattached, detachOnDestroy bool
	// visual-activity / visual-bell: status message on an alert.
	visualActivity, visualBell bool
	// pane-border-status / -format: reserved per-pane label row.
	paneBorderStatus, paneBorderFormat string
	// windowSize is the multi-client grid policy: latest/smallest/largest.
	windowSize string
	// aggressiveResize (tmux's aggressive-resize) sizes a shared window only to
	// sessions where it's the current window; default off.
	aggressiveResize bool
	// synchronizePanes (tmux's synchronize-panes) mirrors input to every pane in
	// a window; session-wide default, per-window overridable. Default off.
	synchronizePanes bool
	// remainOnExit (tmux's remain-on-exit): "off"/"on"/"failed" default for panes
	// whose process exits; a window option, per-window overridable at runtime.
	remainOnExit     string
	copyCommand      string
	updateEnv        []string
	focusEvents      bool
	allowPassthrough bool
	// bellAction/activityAction (tmux's bell-action/activity-action) scope which
	// windows alert: any/none/current/other. Read once per session goroutine.
	bellAction, activityAction string
	// exitEmpty (tmux's exit-empty, default on): the daemon exits when its last
	// session closes. Server-global; a runtime set-option updates it under mu.
	exitEmpty bool
	// hooks are the global event→command bindings; each session copies them at
	// start, so runtime set-hook mutates only that session's copy.
	hooks map[string][]string
	// globalEnv is set-environment -g. Unlike hooks it's read live at each pane
	// spawn (through the locked accessors below), so a runtime `set -g` reaches
	// every session's future panes — tmux's cross-session global-env semantics.
	globalEnv map[string]string
	// clientOpts is the last value of each client option pushed via set-option,
	// replayed to a late-attaching client so it inherits runtime option changes
	// (HISTORY.md, runtime-options). Guarded by mu with the rest of the registry.
	clientOpts map[string]string
	// wait-for channels: script sync primitives. Guarded by waitMu (separate from
	// mu since a lock/wait blocks the connection goroutine while held-registered).
	waitMu sync.Mutex
	waitCh map[string]*waitChan
	// snapshots is each session's self-reported summary (windows/panes), stored on
	// its 1s tick so cross-session widget queries (gtmux.sessions/find_panes) see
	// detached sessions too. snapWant counts attached clients that use widgets; the
	// summary build + status stamp are skipped when it's zero. Guarded by mu.
	snapshots map[string]*proto.SnapSession
	snapWant  int
}

// wantSnapshot adjusts the count of attached clients that use widget queries. A
// widget client bumps it +1 on attach, -1 on detach; when it hits zero the
// per-session summary build and the status stamp are skipped (common no-widget
// path pays nothing).
func (r *registry) wantSnapshot(delta int) {
	r.mu.Lock()
	if r.snapWant += delta; r.snapWant < 0 {
		r.snapWant = 0
	}
	r.mu.Unlock()
}

func (r *registry) snapshotsActive() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapWant > 0
}

// putSnapshot stores a session's current summary (called from its owner
// goroutine each tick when snapshots are active).
func (r *registry) putSnapshot(name string, s *proto.SnapSession) {
	r.mu.Lock()
	if r.snapshots == nil {
		r.snapshots = map[string]*proto.SnapSession{}
	}
	r.snapshots[name] = s
	r.mu.Unlock()
}

// allSnapshots returns every session's summary, sorted by name for a stable
// widget list order (tmux lists sessions alphabetically).
func (r *registry) allSnapshots() []proto.SnapSession {
	r.mu.Lock()
	names := make([]string, 0, len(r.snapshots))
	for n := range r.snapshots {
		names = append(names, n)
	}
	out := make([]proto.SnapSession, 0, len(names))
	sort.Strings(names)
	for _, n := range names {
		out = append(out, *r.snapshots[n])
	}
	r.mu.Unlock()
	return out
}

// waitChan is one wait-for channel: signal/wait (-S wakes bare waiters) and
// lock/unlock (-L acquires or queues, -U releases one or clears the lock).
type waitChan struct {
	locked bool
	signal []chan struct{} // bare `wait-for chan` waiters, all released by -S
	lockq  []chan struct{} // `wait-for -L chan` waiters, released one at a time by -U
}

// waitFor implements tmux's wait-for: `-S` signal, `-L` lock, `-U` unlock, or
// (no flag) wait. Lock/wait block the calling connection goroutine until
// released — never a session goroutine, so nothing stalls.
func (r *registry) waitFor(args []string) error {
	mode, name := "wait", ""
	for _, a := range args {
		switch a {
		case "-S":
			mode = "signal"
		case "-L":
			mode = "lock"
		case "-U":
			mode = "unlock"
		default:
			if !strings.HasPrefix(a, "-") {
				name = a
			}
		}
	}
	if name == "" {
		return fmt.Errorf("wait-for: no channel")
	}

	r.waitMu.Lock()
	wc := r.waitCh[name]
	if wc == nil {
		wc = &waitChan{}
		r.waitCh[name] = wc
	}
	switch mode {
	case "signal":
		for _, c := range wc.signal {
			close(c)
		}
		wc.signal = nil
		r.waitMu.Unlock()
	case "unlock":
		if len(wc.lockq) > 0 {
			c := wc.lockq[0]
			wc.lockq = wc.lockq[1:]
			close(c) // wake one -L waiter; it inherits the lock (stays locked)
		} else {
			wc.locked = false
		}
		r.waitMu.Unlock()
	case "lock":
		if !wc.locked {
			wc.locked = true
			r.waitMu.Unlock()
		} else {
			ch := make(chan struct{})
			wc.lockq = append(wc.lockq, ch)
			r.waitMu.Unlock()
			<-ch // released by a -U, which leaves the lock held for us
		}
	default: // wait
		ch := make(chan struct{})
		wc.signal = append(wc.signal, ch)
		r.waitMu.Unlock()
		<-ch
	}
	return nil
}

// setGlobalEnv sets or (unset) clears a global environment variable.
func (r *registry) setGlobalEnv(name, value string, unset bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if unset {
		delete(r.globalEnv, name)
	} else {
		r.globalEnv[name] = value
	}
}

// globalEnvCopy returns a snapshot of the global environment.
func (r *registry) globalEnvCopy() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := make(map[string]string, len(r.globalEnv))
	for k, v := range r.globalEnv {
		m[k] = v
	}
	return m
}

// setClientOpt records the latest value of a client option (for late-attach
// replay).
func (r *registry) setClientOpt(name, value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clientOpts[name] = value
}

// clientOptsCopy returns a snapshot of the recorded client options.
func (r *registry) clientOptsCopy() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := make(map[string]string, len(r.clientOpts))
	for k, v := range r.clientOpts {
		m[k] = v
	}
	return m
}

// resolve picks the session a client wants to attach to. create (`gtmux new`)
// makes a fresh session — auto-named if name is "", else the given name unless
// it's taken. Otherwise (`gtmux attach`) the session must already exist: a bare
// "" attaches the lone session if there's exactly one, else it's an error.
func (r *registry) resolve(name string, create bool, cols, rows int, cwd string) (*session, error) {
	return r.resolveGroup(name, create, cols, rows, cwd, "")
}

func (r *registry) resolveGroup(name string, create bool, cols, rows int, cwd, groupTarget string) (*session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if create {
		if name == "" {
			name = r.autoName()
		} else if _, taken := r.sessions[name]; taken {
			return nil, fmt.Errorf("duplicate session: %s", name)
		}
		s := newSession(name)
		r.sessions[name] = s
		go s.run(r, cols, rows, cwd, groupTarget)
		return s, nil
	}

	if name == "" {
		switch len(r.sessions) {
		case 0:
			return nil, fmt.Errorf("no sessions (use: gtmux new)")
		case 1:
			for _, s := range r.sessions {
				return s, nil
			}
		default:
			names := make([]string, 0, len(r.sessions))
			for n := range r.sessions {
				names = append(names, n)
			}
			sort.Strings(names)
			return nil, fmt.Errorf("multiple sessions, specify one: %s", strings.Join(names, ", "))
		}
	}
	if s, ok := r.sessions[name]; ok {
		return s, nil
	}
	return nil, fmt.Errorf("no such session: %s", name)
}

// autoName returns the lowest-numbered name matching nameFmt not already in
// use. Terminates because nameFmt is validated to contain %d, so each i
// yields a distinct string. Caller holds r.mu.
func (r *registry) autoName() string {
	for i := 0; ; i++ {
		name := fmt.Sprintf(r.nameFmt, i)
		if _, taken := r.sessions[name]; !taken {
			return name
		}
	}
}

// get looks up a session without creating one.
func (r *registry) get(name string) (*session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[name]
	return s, ok
}

func (r *registry) remove(name string) {
	r.mu.Lock()
	delete(r.sessions, name)
	delete(r.snapshots, name)
	empty := len(r.sessions) == 0 && r.exitEmpty
	r.mu.Unlock()
	if empty {
		// exit-empty (tmux default on): last session gone, shut the daemon down.
		// Mirror kill-server's cleanup — os.Exit skips deferred socket removal.
		os.Remove(proto.SockPath())
		os.Exit(0)
	}
}

// setExitEmpty updates the server-global exit-empty option at runtime.
func (r *registry) setExitEmpty(v bool) {
	r.mu.Lock()
	r.exitEmpty = v
	r.mu.Unlock()
}

// exitEmptyOn reports the current exit-empty setting.
func (r *registry) exitEmptyOn() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.exitEmpty
}

// relabel moves a session to a new registry key, without touching s.name.
// Safe to call from a session's own owner goroutine (e.g. handling a
// prefix+$ rename prompt), since it only ever takes r.mu, never s.events.
func (r *registry) relabel(oldName, newName string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.sessions[newName]; exists {
		return fmt.Errorf("session %q already exists", newName)
	}
	s, ok := r.sessions[oldName]
	if !ok {
		return fmt.Errorf("no such session: %s", oldName)
	}
	delete(r.sessions, oldName)
	r.sessions[newName] = s
	return nil
}

// rename is for external callers (not a session's own goroutine): it
// relabels the registry entry, then asks the session's owner goroutine to
// update its own name field, since that field is otherwise only ever
// touched by that goroutine.
func (r *registry) rename(oldName, newName string) error {
	if err := r.relabel(oldName, newName); err != nil {
		return err
	}
	s, _ := r.get(newName)
	s.rename(newName)
	return nil
}

// names returns every live session's name, sorted. Unlike list, it never
// round-trips through session goroutines, so it's safe to call from one —
// which is exactly what choose-session does.
func (r *registry) recordBind(id, line string) {
	r.mu.Lock()
	if r.runtimeBinds == nil {
		r.runtimeBinds = map[string]string{}
	}
	r.runtimeBinds[id] = line
	r.mu.Unlock()
}

func (r *registry) removeBind(id string) {
	r.mu.Lock()
	delete(r.runtimeBinds, id)
	r.mu.Unlock()
}

func (r *registry) listBinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	lines := make([]string, 0, len(r.runtimeBinds))
	for _, l := range r.runtimeBinds {
		lines = append(lines, l)
	}
	sort.Strings(lines)
	return lines
}

func (r *registry) setLastSession(name string) {
	r.mu.Lock()
	r.lastSession = name
	r.mu.Unlock()
}

func (r *registry) getLastSession() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastSession
}

func (r *registry) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.sessions))
	for n := range r.sessions {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// list returns a snapshot of every live session. Queries each session's own
// goroutine for its info rather than reading its state directly.
func (r *registry) list() []proto.SessionInfo {
	r.mu.Lock()
	sessions := make([]*session, 0, len(r.sessions))
	for _, s := range r.sessions {
		sessions = append(sessions, s)
	}
	r.mu.Unlock()

	infos := make([]proto.SessionInfo, len(sessions))
	for i, s := range sessions {
		infos[i] = s.info()
	}
	return infos
}

// targetSession returns the session component of a `-t sess:...` target in
// args, or "" if there's no -t or the target has no session (a leading ":win"
// or a bare "%id"/"win.pane"). It's the name→owner test for cross-session
// command routing.
func targetSession(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-t" {
			if c := strings.Index(args[i+1], ":"); c > 0 {
				return args[i+1][:c]
			}
			return ""
		}
	}
	return ""
}

// clearStaleSocket refuses to start if a server is already listening at
// sockPath, and otherwise removes any leftover file from a crashed run so a
// fresh Listen can bind it.
func clearStaleSocket(sockPath string) error {
	conn, err := net.DialTimeout("unix", sockPath, 200*time.Millisecond)
	if err == nil {
		conn.Close()
		return fmt.Errorf("gtmux server already running on %s", sockPath)
	}
	os.Remove(sockPath)
	return nil
}

// Run starts the gtmux daemon and blocks until the listener errors out.
func Run() error {
	sockPath := proto.SockPath()
	if err := os.MkdirAll(filepath.Dir(sockPath), 0700); err != nil {
		return err
	}
	if err := clearStaleSocket(sockPath); err != nil {
		return err
	}

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return err
	}
	defer ln.Close()
	log.Printf("gtmux server listening on %s", sockPath)

	// Read the server options once at startup.
	cfg := config.LoadServer(config.ServerConfigPath())

	// Seed the config-time-only history-limit before any session (and thus any
	// pane) can spawn, so the package var is written once, read-only after.
	historyLimit = cfg.HistoryLimit
	defaultShell = cfg.DefaultShell
	defaultCommand = cfg.DefaultCommand

	reg := &registry{
		sessions:          map[string]*session{},
		nameFmt:           cfg.SessionName,
		mainPaneW:         cfg.MainPaneWidth,
		mainPaneH:         cfg.MainPaneHeight,
		baseIndex:         cfg.BaseIndex,
		paneBaseIndex:     cfg.PaneBaseIndex,
		displayTime:       cfg.DisplayTime,
		messageLimit:      cfg.MessageLimit,
		autoRename:        cfg.AutomaticRename,
		allowRename:       cfg.AllowRename,
		autoRenameFmt:     cfg.AutomaticRenameFormat,
		destroyUnattached: cfg.DestroyUnattached,
		detachOnDestroy:   cfg.DetachOnDestroy,
		visualActivity:    cfg.VisualActivity,
		visualBell:        cfg.VisualBell,
		paneBorderStatus:  cfg.PaneBorderStatus,
		paneBorderFormat:  cfg.PaneBorderFormat,
		windowSize:        cfg.WindowSize,
		aggressiveResize:  cfg.AggressiveResize,
		synchronizePanes:  cfg.SynchronizePanes,
		remainOnExit:      cfg.RemainOnExit,
		copyCommand:       cfg.CopyCommand,
		updateEnv:         cfg.UpdateEnvironment,
		focusEvents:       cfg.FocusEvents,
		allowPassthrough:  cfg.AllowPassthrough,
		exitEmpty:         cfg.ExitEmpty,
		bellAction:        cfg.BellAction,
		activityAction:    cfg.ActivityAction,
		hooks:             cfg.Hooks,
		globalEnv:         map[string]string{},
		clientOpts:        map[string]string{},
		waitCh:            map[string]*waitChan{},
	}

	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go acceptConn(reg, conn)
	}
}

// acceptConn reads the client's attach request, hands the connection off to
// the named session's owner goroutine, then relays that connection's
// input/resize/close as events to it. It does not itself own any session
// state.
func acceptConn(reg *registry, conn net.Conn) {
	dec := gob.NewDecoder(conn)
	enc := gob.NewEncoder(conn)

	var msg proto.ClientMsg
	if err := dec.Decode(&msg); err != nil {
		// A connect-then-immediately-hang-up with no message is a health
		// check (auto-spawn's probeSocket), not an error worth logging.
		if err != io.EOF {
			log.Printf("expected attach or list message: %v", err)
		}
		conn.Close()
		return
	}

	if msg.List != nil {
		enc.Encode(&proto.ServerMsg{SessionList: &proto.SessionList{Sessions: reg.list()}})
		conn.Close()
		return
	}
	if msg.KillSession != nil {
		if s, ok := reg.get(msg.KillSession.Name); ok {
			s.kill()
			enc.Encode(&proto.ServerMsg{Ack: &proto.Ack{Ok: true}})
		} else {
			enc.Encode(&proto.ServerMsg{Ack: &proto.Ack{Ok: false, Err: "no such session: " + msg.KillSession.Name}})
		}
		conn.Close()
		return
	}
	if msg.HasSession != nil {
		_, ok := reg.get(msg.HasSession.Name)
		if ok {
			enc.Encode(&proto.ServerMsg{Ack: &proto.Ack{Ok: true}})
		} else {
			enc.Encode(&proto.ServerMsg{Ack: &proto.Ack{Ok: false, Err: "no such session: " + msg.HasSession.Name}})
		}
		conn.Close()
		return
	}
	if msg.NewSession != nil {
		// Detached create: spawn the session's owner goroutine without attaching.
		// Seed a default grid (80x24); the first real attach resizes it.
		_, err := reg.resolveGroup(msg.NewSession.Name, true, 80, 24, msg.NewSession.Cwd, msg.NewSession.GroupTarget)
		if err != nil {
			enc.Encode(&proto.ServerMsg{Ack: &proto.Ack{Ok: false, Err: err.Error()}})
		} else {
			enc.Encode(&proto.ServerMsg{Ack: &proto.Ack{Ok: true}})
		}
		conn.Close()
		return
	}
	if msg.KillServer != nil {
		enc.Encode(&proto.ServerMsg{Ack: &proto.Ack{Ok: true}})
		conn.Close()
		// Whole-daemon shutdown: the panes die with the process, which is
		// exactly what kill-server means. os.Exit skips the listener's
		// deferred Close, so remove the socket ourselves — otherwise it's
		// left orphaned and `gtmux list` dials it into "connection refused".
		os.Remove(proto.SockPath())
		os.Exit(0)
	}
	if msg.Command != nil && len(msg.Command.Args) > 0 && msg.Command.Args[0] == "wait-for" {
		// wait-for is a server-global sync primitive, not a session command:
		// handle it here in the connection goroutine (where blocking is fine).
		if err := reg.waitFor(msg.Command.Args[1:]); err != nil {
			enc.Encode(&proto.ServerMsg{Ack: &proto.Ack{Ok: false, Err: err.Error()}})
		} else {
			enc.Encode(&proto.ServerMsg{Ack: &proto.Ack{Ok: true}})
		}
		conn.Close()
		return
	}
	if msg.Command != nil {
		// A `-t sess:...` target overrides which session the command acts on
		// (tmux's cross-session addressing): route the whole command to that
		// session's goroutine, where the window.pane part resolves locally. Safe
		// here — this is the connection goroutine, not a session goroutine, so no
		// goroutine-to-goroutine deadlock.
		sess := msg.Command.Session
		if ts := targetSession(msg.Command.Args); ts != "" {
			sess = ts
		}
		if s, ok := reg.get(sess); ok {
			out, errText := s.command(msg.Command.Args)
			if errText != "" {
				enc.Encode(&proto.ServerMsg{Ack: &proto.Ack{Ok: false, Err: errText}})
			} else {
				enc.Encode(&proto.ServerMsg{Ack: &proto.Ack{Ok: true, Out: out}})
			}
		} else {
			enc.Encode(&proto.ServerMsg{Ack: &proto.Ack{Ok: false, Err: "no such session: " + sess}})
		}
		conn.Close()
		return
	}
	if msg.RenameSession != nil {
		if err := reg.rename(msg.RenameSession.Old, msg.RenameSession.New); err != nil {
			enc.Encode(&proto.ServerMsg{Ack: &proto.Ack{Ok: false, Err: err.Error()}})
		} else {
			enc.Encode(&proto.ServerMsg{Ack: &proto.Ack{Ok: true}})
		}
		conn.Close()
		return
	}
	if msg.Attach == nil {
		log.Printf("expected attach or list message, got neither")
		conn.Close()
		return
	}

	s, err := reg.resolveGroup(msg.Attach.Session, msg.Attach.Create, msg.Attach.Cols, msg.Attach.Rows, msg.Attach.Cwd, msg.Attach.GroupTarget)
	if err != nil {
		enc.Encode(&proto.ServerMsg{Ack: &proto.Ack{Ok: false, Err: err.Error()}})
		conn.Close()
		return
	}
	epoch := s.attach(conn, enc, msg.Attach.Cols, msg.Attach.Rows, msg.Attach.StatusCmds, msg.Attach.StatusInterval, msg.Attach.Env, msg.Attach.ReadOnly, msg.Attach.WantSnapshot)

	for {
		var m proto.ClientMsg
		if err := dec.Decode(&m); err != nil {
			s.events <- clientGone{epoch: epoch}
			return
		}
		switch {
		case m.Input != nil:
			s.events <- clientInput{epoch: epoch, data: m.Input.Data}
		case m.Mouse != nil:
			s.events <- clientMouse{epoch: epoch, cb: m.Mouse.Cb, x: m.Mouse.X, y: m.Mouse.Y, press: m.Mouse.Press}
		case m.ResizeBorder != nil:
			s.events <- resizeBorderEvent{epoch: epoch, index: m.ResizeBorder.Index, pos: m.ResizeBorder.Pos}
		case m.CopyDrag != nil:
			s.events <- copyDragEvent{epoch: epoch, paneID: m.CopyDrag.PaneID, row: m.CopyDrag.Row, col: m.CopyDrag.Col}
		case m.Resize != nil:
			s.events <- clientResize{epoch: epoch, cols: m.Resize.Cols, rows: m.Resize.Rows}
		case m.SetPaste != nil:
			s.events <- setPasteEvent{text: m.SetPaste.Text, pipe: m.SetPaste.Pipe}
		case m.Action != nil:
			// A keybind whose command carries a `-t sess:...` target routes to
			// that session (tmux's cross-session keybinds). Run on the connection
			// goroutine via command() — not a session goroutine, so no deadlock.
			// Same-session (or overlay-opening) actions take the local path so the
			// acting epoch is known for client-targeted replies.
			if ts := targetSession(m.Action.Args); ts != "" && ts != s.name {
				if peer, ok := reg.get(ts); ok {
					peer.command(m.Action.Args)
				}
			} else {
				s.events <- actionEvent{epoch: epoch, args: m.Action.Args}
			}
		}
	}
}
