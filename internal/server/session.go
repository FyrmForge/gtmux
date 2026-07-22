package server

import (
	"encoding/gob"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/FyrmForge/gtmux/internal/emu"
	"github.com/FyrmForge/gtmux/internal/format"
	"github.com/FyrmForge/gtmux/internal/proto"
)

// attachment is one client attached to a session. A session can have several
// at once (sharing the same windows); the grid is sized to the smallest of
// them. cols/rows are that client's own terminal size, used for negotiation.
type attachment struct {
	conn       net.Conn
	enc        *gob.Encoder
	cols, rows int  // rows is the window (content) height; the client already subtracted its status bar
	readOnly   bool // attach -r: input never reaches a pane
	wantSnap   bool // client uses widget queries: keep the registry snapshot count up
}

// session is one gtmux session: a set of windows/panes that keep running
// whether or not a client is currently attached. All of its state (windows,
// active window, prefix-key mode, the current attachment) is owned
// exclusively by session.run's goroutine, so none of it needs locking.
type session struct {
	name   string
	events chan interface{}
}

// ptyOutput is a chunk of output read from one pane's PTY, or its
// terminating error (process exited).
type ptyOutput struct {
	pane *pane
	gen  int // the pane generation this reader was started for (respawn guard)
	data []byte
	err  error
}

// silenceEvent is posted by a view's monitor-silence timer when the interval
// lapses with no output; the handler checks the window is still non-current for
// that view's session. Carries the view (alert state is per-view) and its window.
type silenceEvent struct {
	view   *view
	window *window
}

// linkRequest asks a session (the source) for one of its window actors so the
// requesting session can link that window in (link-window). The source resolves
// spec against its own windows and replies with the actor (nil if not found).
type linkRequest struct {
	spec  string
	reply chan *windowActor
}

// winlinkGone tells a session that a window it links has been destroyed elsewhere
// (kill-window, or its last pane exited): drop the winlink referencing actor.
type winlinkGone struct{ actor *windowActor }

// groupJoinRequest asks a session for all its window actors so a new group member
// (new-session -t) can display the same windows. Replies with a snapshot.
type groupJoinRequest struct{ reply chan []*windowActor }

// windowResized tells a session that a window it shares changed grid size because
// another viewer's client did (window-size/aggressive-resize). Redraw it if it's
// this session's current window.
type windowResized struct{ actor *windowActor }

type clientInput struct {
	epoch int
	data  []byte
}
type clientResize struct {
	epoch      int
	cols, rows int // rows is the window (content) height (client already subtracted its status bar)
}
type clientMouse struct {
	epoch    int
	cb, x, y int
	press    bool
}

// resizeBorderEvent / copyDragEvent carry the two mouse gestures the client
// recognizes from its own Layout and turns into semantic requests, so the
// server no longer decodes them from raw mouse coordinates (client-owned mouse).
type resizeBorderEvent struct {
	epoch      int
	index, pos int
}
type copyDragEvent struct {
	epoch    int
	paneID   int
	row, col int
}
type clientGone struct{ epoch int }

// pbuf is one named paste buffer (tmux's buffer model).
type pbuf struct{ name, data string }

// setPasteEvent sets the session's paste buffer, sent by a client after a
// copy-mode yank (copy-mode is client-side now) so prefix+] paste still works.
type setPasteEvent struct {
	text string
	pipe bool // copy-mode yank: also pipe through copy-command
}

// actionEvent runs a command on behalf of a client — a prompt/picker commit,
// and (after Stage 4) every keybind. Routed straight to runCommand.
type actionEvent struct {
	epoch int
	args  []string
}
type attachEvent struct {
	conn           net.Conn
	enc            *gob.Encoder
	cols, rows     int               // rows is the window (content) height (client subtracted its status bar)
	statusCmds     []string          // #server(cmd) bodies this client's formats use
	statusInterval int               // client's #server() cache cadence, seconds
	env            map[string]string // client env, for update-environment
	readOnly       bool              // attach -r
	wantSnap       bool              // client uses widget queries
	epochCh        chan int
}
type infoEvent struct{ replyCh chan proto.SessionInfo }

// clientInfo is one client's attachment, for a global list-clients.
type clientInfo struct {
	session    string
	epoch      int
	cols, rows int
}
type clientsEvent struct{ replyCh chan []clientInfo }
type previewEvent struct{ replyCh chan []emu.Line }
type killEvent struct{ replyCh chan struct{} }
type renameEvent struct {
	name    string
	replyCh chan struct{}
}
type hideNumbersEvent struct{ window *window }

// messageEvent sets the transient status-line message (run-shell output,
// command errors); clearMessageEvent removes it after its display timeout.
type messageEvent struct{ text string }
type clearMessageEvent struct{}

// ifShellResult carries an if-shell's chosen then/else command back to the
// session's owner goroutine, so the shell can run async without blocking it.
type ifShellResult struct{ cmd []string }

// commandEvent runs one `gtmux run` command on the session's owner
// goroutine; the reply carries any stdout (info commands) and an error or "".
type commandEvent struct {
	args    []string
	replyCh chan cmdReply
}

type cmdReply struct{ out, err string }

func newSession(name string) *session {
	return &session{name: name, events: make(chan interface{}, 64)}
}

// attach hands a freshly connected client off to the session's owner
// goroutine and returns the epoch assigned to it, used to tag that
// connection's later input/resize/close events.
func (s *session) attach(conn net.Conn, enc *gob.Encoder, cols, rows int, statusCmds []string, statusInterval int, env map[string]string, readOnly, wantSnap bool) int {
	ch := make(chan int, 1)
	s.events <- attachEvent{conn: conn, enc: enc, cols: cols, rows: rows, statusCmds: statusCmds, statusInterval: statusInterval, env: env, readOnly: readOnly, wantSnap: wantSnap, epochCh: ch}
	return <-ch
}

// info asks the session's owner goroutine for a snapshot of its state.
func (s *session) info() proto.SessionInfo {
	ch := make(chan proto.SessionInfo, 1)
	s.events <- infoEvent{replyCh: ch}
	return <-ch
}

// clients asks the session's owner goroutine for its attached clients, for a
// global list-clients. Must not be called from the session's own goroutine
// (it would deadlock) — the aggregator reads self locally.
func (s *session) clients() []clientInfo {
	ch := make(chan []clientInfo, 1)
	s.events <- clientsEvent{replyCh: ch}
	return <-ch
}

// previewLines asks the session's owner goroutine for a styled snapshot of its
// active pane, for a cross-session picker preview (self reads locally instead).
func (s *session) previewLines() []emu.Line {
	ch := make(chan []emu.Line, 1)
	s.events <- previewEvent{replyCh: ch}
	return <-ch
}

// previewSnap deep-copies a pane's screen for a static picker preview: trailing
// blank rows dropped, capped to the last 60 (a near-full-screen preview).
func previewSnap(scr []emu.Line) []emu.Line {
	blank := func(l emu.Line) bool {
		for _, g := range l {
			if g.Char != ' ' && g.Char != 0 {
				return false
			}
		}
		return true
	}
	end := len(scr)
	for end > 0 && blank(scr[end-1]) {
		end--
	}
	start := 0
	if end > 60 {
		start = end - 60
	}
	out := make([]emu.Line, 0, end-start)
	for i := start; i < end; i++ {
		l := make(emu.Line, len(scr[i]))
		copy(l, scr[i])
		out = append(out, l)
	}
	return out
}

// kill asks the session's owner goroutine to shut down and blocks until it
// has closed every pane and deregistered itself.
func (s *session) kill() {
	ch := make(chan struct{}, 1)
	s.events <- killEvent{replyCh: ch}
	<-ch
}

// command asks the session's owner goroutine to execute one command-mode
// command (from `gtmux run`), returning its error message or "".
func (s *session) command(args []string) (out, errText string) {
	ch := make(chan cmdReply, 1)
	s.events <- commandEvent{args: args, replyCh: ch}
	r := <-ch
	return r.out, r.err
}

// rename tells the session's owner goroutine its new display name. s.name is
// otherwise only read/written by that goroutine, so the update is routed
// through the event channel rather than set directly from the registry's
// goroutine.
func (s *session) rename(name string) {
	ch := make(chan struct{}, 1)
	s.events <- renameEvent{name: name, replyCh: ch}
	<-ch
}

// run is the session's owner goroutine: the sole mutator of its windows,
// panes, and attachment state. Removes the session from reg when its last
// pane exits.
func (s *session) run(reg *registry, cols, rows int, cwd, groupTarget string) {
	// The client reserves its own status rows and reports the window (content)
	// height directly, so the server sizes the grid to rows as-is — status-bar
	// reservation is entirely client-side. (winRows stays a func for the many
	// call sites and in case a future server-side inset needs it back.)
	winRows := func() int {
		if rows > 0 {
			return rows
		}
		return 1
	}

	watchPane := func(p *pane) {
		// Capture the specific PTY file and generation: after a respawn swaps
		// p.pty, this goroutine keeps reading the OLD file (until it closes) and
		// its events carry the old gen, so the handler drops them.
		// Output goes DIRECTLY to the pane's window actor — session-independent, so
		// a window shared by several sessions keeps flowing even if the session that
		// created it ends (the readers are actor-owned, not session-owned). The
		// origin (birth window) never changes; break/join set origin.relay[p] so it
		// forwards a migrated pane's output to the new actor — one ordered path,
		// no retargeting of this (unpausable) reader. A popup pane (win == nil) has
		// no actor, so it stays on s.events; exits always go to s.events (the
		// session owns closePane/teardown).
		f, gen := p.pty, p.gen
		if p.origin == nil && p.win != nil {
			p.origin = p.win.actor
		}
		origin := p.origin
		go func() {
			buf := make([]byte, 4096)
			for {
				n, err := f.Read(buf)
				if n > 0 {
					data := append([]byte(nil), buf[:n]...)
					if origin != nil {
						origin.events <- outputMsg{pane: p, gen: gen, data: data}
					} else {
						s.events <- ptyOutput{pane: p, gen: gen, data: data}
					}
				}
				if err != nil {
					s.events <- ptyOutput{pane: p, gen: gen, err: err}
					return
				}
			}
		}()
	}

	// sessionEnv is the set-environment map, injected into every new pane. Shared
	// by reference with each window, so a later set-environment reaches windows
	// created earlier (future panes only, like tmux). ponytail: session-scoped;
	// tmux -g (global) deferred.
	sessionEnv := map[string]string{}

	// new-session -t <group>: display the target session's current windows (a
	// snapshot — this session subscribes a view to each of the target's actors
	// below). ponytail: snapshot only; tmux keeps the group's window LIST
	// synchronized (new-window in one appears in all) — that needs a shared winlink
	// set, deferred.
	var groupActors []*windowActor
	if groupTarget != "" {
		if src, ok := reg.get(groupTarget); ok && src != s {
			reply := make(chan []*windowActor, 1)
			src.events <- groupJoinRequest{reply: reply}
			select {
			case groupActors = <-reply:
			case <-time.After(2 * time.Second):
			}
		}
	}

	// first is this session's own initial window (nil for a group member, which
	// only borrows the target's windows — no window of its own to start/watch).
	var first *windowActor
	var windows []winlink
	if len(groupActors) > 0 {
		for _, a := range groupActors {
			windows = append(windows, winlink{actor: a}) // view filled in below
		}
	} else {
		firstWin, err := newWindow(cols, winRows(), cwd, "", s.name, sessionEnv, reg.globalEnvCopy())
		if err != nil {
			log.Printf("session %s: spawn window: %v", s.name, err)
			reg.remove(s.name)
			return
		}
		first = newWindowActor(firstWin)
		watchPane(first.active)
		windows = []winlink{{actor: first}} // view filled in when first.start() runs below
	}
	active := 0
	lastWindow := 0 // tmux last-window: the window active before the current one
	// winGrid caches each window actor's real grid (cols,rows) — the window owns
	// its size and it can differ from this session's vote when a smaller viewer
	// shares it. Updated wherever the size changes (pushSize / windowResized /
	// resize-window) so #{window_width} reads it lock-free instead of an actorDo
	// per status tick (that stalled on a flooding actor).
	winGrid := map[*windowActor][2]int{}

	// Live server-side options, owned by this goroutine (no lock). Seeded from
	// the daemon config; set-option mutates them, and the next select-layout
	// picks them up — tmux doesn't auto-relayout on the option change either.
	mainPaneW, mainPaneH := reg.mainPaneW, reg.mainPaneH
	baseIndex, paneBaseIndex := reg.baseIndex, reg.paneBaseIndex
	displayTime := reg.displayTime
	messageLimit := reg.messageLimit
	autoRename, allowRename := reg.autoRename, reg.allowRename
	autoRenameFmt := reg.autoRenameFmt
	destroyUnattached, detachOnDestroy := reg.destroyUnattached, reg.detachOnDestroy
	// monitor-activity / monitor-bell: session defaults (tmux off), per-window
	// overridable via the window opts store. Read at detection time.
	monitorActivity, monitorBell := false, false
	monitorSilence := 0 // seconds of no output before a silence alert (0 = off)
	visualActivity, visualBell := reg.visualActivity, reg.visualBell
	paneBorderStatus, paneBorderFormat := reg.paneBorderStatus, reg.paneBorderFormat
	windowSize := reg.windowSize
	aggressiveResize := reg.aggressiveResize
	// synchronize-panes: session-wide default (tmux off), per-window overridable
	// via the opts store. Resolved at input time in handleInput.
	synchronizePanes := reg.synchronizePanes
	remainOnExit := reg.remainOnExit
	copyCommand := reg.copyCommand
	updateEnv := reg.updateEnv
	focusEvents := reg.focusEvents
	allowPassthrough := reg.allowPassthrough
	bellAction, activityAction := reg.bellAction, reg.activityAction

	// userOpts holds @foo user options (readable in formats as #{@foo}); cmdAlias
	// holds command-alias name→expansion, resolved at dispatch. ponytail: both are
	// session-scoped (not cross-session -g) — one session covers the POC use.
	userOpts := map[string]string{}
	cmdAlias := map[string]string{}
	// withUserOpts merges the @foo options into a freshly-built format var map so
	// #{@foo} resolves like any other variable.
	withUserOpts := func(m map[string]string) map[string]string {
		for k, v := range userOpts {
			m[k] = v
		}
		return m
	}

	// windowName resolves a window's display label (tmux's name resolution): a
	// manual rename-window wins; else an app-set OSC title if allow-rename is on;
	// else the automatic-rename-format expansion (which also refreshes the frozen
	// autoName so turning automatic-rename off keeps the last value). Runs on the
	// session goroutine, so mutating w.autoName here needs no lock. autoName is
	// built from a small pane-only var set (no window_name) to avoid recursion.
	windowName := func(w *windowActor) string {
		// Resolve the naming options per window: a setw override on this window
		// wins over the session default.
		ar, al, af := autoRename, allowRename, autoRenameFmt
		if v, ok := w.opts["automatic_rename"]; ok {
			ar = onOff(v)
		}
		if v, ok := w.opts["allow_rename"]; ok {
			al = onOff(v)
		}
		if v, ok := w.opts["automatic_rename_format"]; ok {
			af = v
		}
		if w.manualName != nil {
			return *w.manualName
		}
		if al {
			if t := w.active.term.Title(); t != "" {
				return t
			}
		}
		if ar {
			w.autoName = format.Expand(af, map[string]string{
				"pane_command": w.active.currentCommand(),
				"pane_path":    w.active.cwd(),
				"pane_title":   w.active.term.Title(),
				"pane_id":      fmt.Sprintf("%%%d", w.active.id),
			})
			return w.autoName
		}
		if w.autoName != "" {
			return w.autoName
		}
		if c := w.active.currentCommand(); c != "" {
			return c
		}
		return "?"
	}

	// initWindowBorder stamps the current pane-border-status/-format onto a new
	// window and reflows it (reserving the label row) when the status is on.
	initWindowBorder := func(w *windowActor) {
		w.borderStatus, w.borderFormat = paneBorderStatus, paneBorderFormat
		if paneBorderStatus != "off" {
			w.reflow()
		}
	}
	// refreshBorderLabels re-expands each pane's border label (pane-border-format)
	// before a layout is sent. ponytail: refreshed on layout change, not on every
	// output — a #{pane_command} label can lag until the next relayout.
	refreshBorderLabels := func(w *windowActor) {
		if w.borderStatus == "off" {
			return
		}
		for i, p := range w.panes {
			p.borderLabel = format.Expand(w.borderFormat, map[string]string{
				"pane_index":   strconv.Itoa(i + paneBaseIndex),
				"pane_command": p.currentCommand(),
				"pane_title":   p.term.Title(),
				"pane_id":      fmt.Sprintf("%%%d", p.id),
				"pane_active":  bitStr(p == w.active),
				"window_name":  windowName(w),
			})
		}
	}
	// A group member has no own window (first == nil); its borrowed actors
	// already carry the owner's border settings.
	if first != nil {
		initWindowBorder(first)
	}

	// hooks is this session's own copy of the global event→command bindings, so
	// a runtime set-hook stays session-local (no shared mutable state). fireHook
	// is forward-declared: it runs commands via runCommand (defined below), while
	// createWindow/splitPane/etc call it — so the two close over each other.
	hooks := map[string][]string{}
	for k, v := range reg.hooks {
		hooks[k] = append([]string(nil), v...)
	}
	firing := map[string]bool{} // hook names currently firing, for per-name re-entry guard
	var fireHook func(string)
	// handleRender processes one renderMsg from a window actor (fan-out + activity/
	// bell/silence detection). Forward-declared so actorDo can pump it while it
	// waits; assigned once its dependencies (monitor opts, showMessage) exist.
	var handleRender func(renderMsg)
	renderCh := make(chan renderMsg, 256) // actors push here; drained by the main select and actorDo's pump

	// buffers is the session's paste-buffer stack, newest first (buffers[0] is
	// the default paste target, tmux's "buffer0"). Set by copy-mode's yank and
	// set-buffer, read by paste-buffer. ponytail: session-scoped (tmux's buffers
	// are server-global) — keeps the no-mutex owner model; a global store would
	// need a lock. bufSeq names auto buffers buffer0, buffer1, …
	var buffers []pbuf
	bufSeq := 0
	// addBuffer prepends a buffer. A given name replaces any existing same-named
	// one; an empty name gets the next auto name.
	addBuffer := func(name, data string) {
		if name == "" {
			name = fmt.Sprintf("buffer%d", bufSeq)
			bufSeq++
		} else {
			for i, b := range buffers {
				if b.name == name {
					buffers = append(buffers[:i], buffers[i+1:]...)
					break
				}
			}
		}
		buffers = append([]pbuf{{name: name, data: data}}, buffers...)
	}
	// findBuffer returns the named buffer, or the newest when name is "".
	findBuffer := func(name string) (pbuf, bool) {
		if name == "" {
			if len(buffers) > 0 {
				return buffers[0], true
			}
			return pbuf{}, false
		}
		for _, b := range buffers {
			if b.name == name {
				return b, true
			}
		}
		return pbuf{}, false
	}
	// bufFlag pulls a leading "-b name" off args, returning the name and the rest.
	bufFlag := func(args []string) (name string, rest []string) {
		if len(args) >= 2 && args[0] == "-b" {
			return args[1], args[2:]
		}
		return "", args
	}
	// respawnCommand joins the command words in a respawn-pane/window's leftover
	// args, dropping tmux's -k (we always kill+restart). Empty → the shell.
	respawnCommand := func(args []string) string {
		var w []string
		for _, a := range args {
			if a != "-k" {
				w = append(w, a)
			}
		}
		return strings.Join(w, " ")
	}

	// popups holds each client's display-popup floating terminal, keyed by the
	// epoch that opened it (per-client: only that client sees and drives it).
	// popupEpoch finds the owner of a popup pane for output/close routing.
	popups := map[int]*pane{}
	popupEpoch := func(p *pane) (int, bool) {
		for ep, pp := range popups {
			if pp == p {
				return ep, true
			}
		}
		return 0, false
	}

	done := false
	nextEpoch := 0
	attachments := map[int]*attachment{}
	// actingEpoch is the client whose input/mouse event is being handled
	// right now, for actions (detach, session-switch) that target only the
	// client that triggered them rather than broadcasting.
	actingEpoch := 0
	var killReplyCh chan struct{}
	killed := false // session ends by kill (not last-window-close): detach-on-destroy applies

	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "gtmux"
	}
	created := strconv.FormatInt(time.Now().Unix(), 10) // session_created, for #{t:...}
	var cachedClock, cachedGitBranch string
	refreshStatus := func() {
		cachedClock = time.Now().Format("15:04")
		cachedGitBranch = gitBranch(windows[active].actor.active.cwd())
	}
	refreshStatus()

	// statusMsg is a transient status-line message (run-shell output,
	// command-mode errors), cleared a few seconds after being set. Prompts are
	// fully client-owned now; the only overlay the server still opens is the
	// choose-session picker (OpenPicker), whose list it alone holds.
	statusMsg := ""

	// msgLog is the show-messages ring: every transient message that passes
	// through showMessage is recorded here, capped at messageLimit.
	var msgLog []string

	// #server(cmd) commands collected from attached clients' status formats
	// (the client owns the formats, so it sends the list at attach), run each
	// tick and streamed back so the client can expand #server() locally.
	// ponytail: union set, never shrinks — a detached client's commands keep
	// running until the session dies. Fine for a POC; prune per-attachment if
	// it ever matters.
	var serverCmds []string
	shellRunner := newServerShell(15 * time.Second)

	// cmdOut is where info commands (display-message, list-panes) leave their
	// stdout; the commandEvent/actionEvent handlers reset it before each run
	// and read it after, so runCommand keeps its plain error-string return.
	var cmdOut string

	// buildSnapSession summarizes this session (windows → panes → clients) for the
	// cross-session widget snapshot; stored into the registry on the 1s tick so
	// detached sessions stay visible to another client's gtmux.sessions()/find_panes.
	// Defined before currentStatus so the latter can refresh this session's entry
	// fresh on each status send (the 1s cache lags a window change otherwise).
	buildSnapSession := func() *proto.SnapSession {
		snap := &proto.SnapSession{Name: s.name, Attached: len(attachments) > 0}
		for i, wl := range windows {
			w := wl.actor
			sw := proto.SnapWindow{Index: i + baseIndex, Name: windowName(w), Active: i == active, Zoomed: w.zoomed, Activity: wl.view.activity, Bell: wl.view.bell, Silence: wl.view.silence}
			for j, p := range w.panes {
				pid := 0
				if p.cmd != nil && p.cmd.Process != nil {
					pid = p.cmd.Process.Pid
				}
				sw.Panes = append(sw.Panes, proto.PaneInfo{
					Number: j + paneBaseIndex, ID: p.id,
					Command: p.currentCommand(), Path: p.cwd(), Title: p.term.Title(),
					PID: pid, Active: p == w.active, Marked: p.marked,
					Width: p.rect.Cols, Height: p.rect.Rows,
				})
			}
			snap.Windows = append(snap.Windows, sw)
		}
		for ep, a := range attachments {
			snap.Clients = append(snap.Clients, proto.SnapClient{
				Name: fmt.Sprintf("%s:%d", s.name, ep), Session: s.name, Width: a.cols, Height: a.rows,
			})
		}
		for _, b := range buffers {
			preview := b.data
			if nl := strings.IndexByte(preview, '\n'); nl >= 0 {
				preview = preview[:nl]
			}
			if len(preview) > 50 {
				preview = preview[:50]
			}
			snap.Buffers = append(snap.Buffers, proto.SnapBuffer{Name: b.name, Preview: preview})
		}
		return snap
	}

	// currentStatus builds the status bar's raw data (not expanded — the client
	// owns the formats and expands them): the variable map, the #server() shell
	// output, the window list, and any transient server message.
	currentStatus := func() *proto.StatusInfo {
		p := windows[active].actor.active
		info := &proto.StatusInfo{
			Vars: map[string]string{
				"host":         hostname,
				"session":      s.name,
				"window_name":  windowName(windows[active].actor),
				"window_index": strconv.Itoa(active + baseIndex),
				"git_branch":   cachedGitBranch,
				"clock":        cachedClock,
				"pane_path":    p.cwd(),
				"pane_command": p.currentCommand(),
				"pane_title":   p.term.Title(),
			},
			ServerShell: shellRunner.run(serverCmds),
		}
		withUserOpts(info.Vars) // @foo user options readable in status formats
		for i, wl := range windows {
			w := wl.actor
			info.Windows = append(info.Windows, proto.WindowInfo{Index: i + baseIndex, Name: windowName(w), Active: i == active, Zoomed: w.zoomed, Activity: wl.view.activity, Bell: wl.view.bell, Silence: wl.view.silence, Panes: len(w.panes)})
		}
		// copy-mode and prompts are drawn client-side now; the server only
		// still owns transient status messages (run-shell output, errors).
		if statusMsg != "" {
			info.PromptLabel, info.PromptText = "message", statusMsg
		}
		// Widget clients get the whole-server snapshot (every session's summary,
		// self-reported on each session's tick). Skipped entirely when no attached
		// client uses widget queries.
		if reg.snapshotsActive() {
			// Refresh THIS session's registry entry now (not just on the 1s tick),
			// so an attached client's status bar — gtmux.windows() of its own
			// session — reflects a window change immediately instead of lagging a
			// tick. Other sessions stay from their last self-report.
			reg.putSnapshot(s.name, buildSnapSession())
			info.Snapshot = &proto.StateSnapshot{Sessions: reg.allSnapshots()}
		}
		return info
	}

	stampStatus := func(msg *proto.ServerMsg) {
		msg.Status = currentStatus()
	}

	// send broadcasts a message to every attached client, stamping the
	// current status onto it: the status bar (window list, clock, git
	// branch, or a prompt) can change independently of anything else, so
	// every outgoing message carries it.
	// ponytail: writes to all encoders sequentially on the session
	// goroutine, so one wedged client stalls the others — same failure mode
	// as the old single-client path, just wider. Upgrade path if it bites:
	// a per-client output goroutine with its own buffered queue.
	send := func(msg *proto.ServerMsg) {
		stampStatus(msg)
		for _, a := range attachments {
			a.enc.Encode(msg)
		}
	}

	// sendTo delivers a message to one client only, for actions that concern
	// just the client that triggered them (a session-switch handoff).
	sendTo := func(epoch int, msg *proto.ServerMsg) {
		stampStatus(msg)
		if a := attachments[epoch]; a != nil {
			a.enc.Encode(msg)
		}
	}

	// sendPassthrough forwards allow-passthrough bytes to writable clients only —
	// a read-only (attach -r) client observes, so pane-driven side effects (an
	// app's OSC 52 clipboard set) must not reach its terminal.
	sendPassthrough := func(raw []byte) {
		msg := &proto.ServerMsg{Passthrough: raw}
		for _, a := range attachments {
			if !a.readOnly {
				a.enc.Encode(msg)
			}
		}
	}

	activeWindow := func() *windowActor { return windows[active].actor }

	// sendFocus notifies a pane of a focus change (tmux focus-events): only a
	// pane that requested focus reporting (DECSET 1004) receives the escape.
	sendFocus := func(p *pane, in bool) {
		if !focusEvents || p == nil || p.term.Mode()&emu.ModeFocus == 0 {
			return
		}
		if in {
			p.pty.Write([]byte("\x1b[I"))
		} else {
			p.pty.Write([]byte("\x1b[O"))
		}
	}
	// refocus emits the focus-out/in pair when the focused pane changes. Callers
	// snapshot the focused pane before the change and pass it as prev.
	refocus := func(prev *pane) {
		cur := windows[active].actor.active
		if cur != prev {
			sendFocus(prev, false)
			sendFocus(cur, true)
		}
	}

	// actorDo runs a window-touching closure on wa's actor goroutine — the single
	// seam through which the session reads/mutates window state (grids + pane
	// tree). Deadlock-free per the 3a spike (.tmp/spike3.go): the session pumps
	// renders while it BOTH enqueues the request and awaits the reply, because a
	// bare send/wait would stall render drain and deadlock under output
	// backpressure. fn must be a LEAF — never call actorDo on the same actor.
	// pumpRender drains a render while an actorDo waits. handleRender is a closure
	// assigned further down (it captures state built during startup); an actorDo
	// that runs before that assignment — e.g. the initial setActive, when a
	// default-command pane emits output the instant it spawns — would otherwise
	// call a nil handleRender and panic. Dropping such an early render is safe: no
	// client is attached yet, so nothing renders, and the first attach fullSyncs
	// the grid state anyway.
	pumpRender := func(rm renderMsg) {
		if handleRender != nil {
			handleRender(rm)
		}
	}
	actorDo := func(wa *windowActor, fn func()) {
		done := make(chan struct{})
		msg := doMsg{fn: fn, done: done}
		for enq := false; !enq; {
			select {
			case wa.events <- msg:
				enq = true
			case rm := <-renderCh:
				pumpRender(rm)
			}
		}
		for {
			select {
			case <-done:
				return
			case rm := <-renderCh:
				pumpRender(rm)
			}
		}
	}

	// finishStop ends a window actor's run() so it touches no more pane state,
	// discarding renderCh meanwhile so a final render can't deadlock the wait.
	// After it returns the window's panes are safe to Close (both applyOutput and
	// p.Close touch p.pipeW). Renders are discarded, not handled: mid-session that
	// dodges reading the mid-adjustment `active`, and a dropped render self-heals
	// (a fullSync on window switch, or the next output chunk).
	//
	// It enqueues stopMsg rather than close(wa.events): a pane reader goroutine
	// sends outputMsg straight to origin.events (watchPane), and a straggler read
	// after a close would panic on a closed channel — fatal to the whole server.
	// FIFO drains everything already queued first; late reader sends land unread
	// in the buffer. Session-goroutine ptyOutput stragglers are still guarded by
	// wa.stopped in the handler below.
	finishStop := func(wa *windowActor) {
		wa.stopped = true // late reader events (exit) must skip this actor now
		wa.events <- stopMsg{}
		for {
			select {
			case <-wa.done:
				return
			case <-renderCh:
			}
		}
	}

	// stopActor tears a window actor down — unless it still relays panes that
	// migrated away (reader→origin→dest). Those readers post to this actor, so it
	// must keep running to forward, even though its own window is gone: it becomes a
	// relay-only zombie (stopping), and dropRelay calls finishStop once its last
	// relayed pane exits. ponytail: a zombie whose session ends before its relayed
	// pane exits leaks until process end — bounded (only migrated panes), like the
	// reader-goroutine leak.
	stopActor := func(wa *windowActor) {
		relaying := false
		actorDo(wa, func() {
			relaying = len(wa.relay) > 0
			if relaying {
				wa.stopping = true
			}
		})
		if relaying {
			return
		}
		finishStop(wa)
	}

	// setRelay points a migrated pane's origin actor at the pane's new window actor
	// so the origin forwards that pane's output there — one ordered path
	// reader→origin→dest. Runs on the origin's goroutine; the caller must already
	// have removed the pane from its old window's tree so the origin stops applying
	// it. ponytail: re-migrating an already-migrated pane re-points the origin,
	// whose old target may still hold a straggler — rare, self-heals; a chain would
	// be exact but isn't worth it for break/join frequency.
	setRelay := func(p *pane, dest *windowActor) {
		if p.origin == nil {
			return
		}
		actorDo(p.origin, func() { p.origin.relay[p] = dest })
	}

	// dropRelay retires a migrated pane's relay entry as it exits; if its origin was
	// a relay-only zombie and this was its last relayed pane, finish stopping it.
	dropRelay := func(p *pane) {
		o := p.origin
		if o == nil {
			return
		}
		finish := false
		actorDo(o, func() {
			delete(o.relay, p)
			finish = o.stopping && len(o.relay) == 0
		})
		if finish {
			finishStop(o)
		}
	}

	// subscribeView adds a session's view (render + notify channels) to a window
	// actor — actor-coordinated, so it's safe to call on an actor another session
	// already runs (link-window). The caller keeps the returned view as its winlink
	// handle.
	subscribeView := func(wa *windowActor, renders chan<- renderMsg, notify chan<- any) *view {
		vw := &view{renders: renders, notify: notify}
		actorDo(wa, func() { wa.views = append(wa.views, vw) })
		return vw
	}

	// unsubscribeView removes a session's view (actor-coordinated) and reports
	// whether it was the last — i.e. no session views this window anymore.
	unsubscribeView := func(wa *windowActor, vw *view) bool {
		last := false
		actorDo(wa, func() {
			for i, v := range wa.views {
				if v == vw {
					wa.views = append(wa.views[:i], wa.views[i+1:]...)
					break
				}
			}
			last = len(wa.views) == 0
			// Our vote is gone: the remaining viewers may size the window back up.
			if !last {
				wa.recomputeSize(nil)
			}
		})
		return last
	}

	// releaseWindow drops THIS session's view of a window. A window shared by
	// another session keeps running (only the winlink here goes); only the last
	// viewer stops the actor and — if closePanes — closes its panes. This is the
	// refcount teardown: at one viewer (today's common case) last is always true,
	// so it matches the old unconditional stop.
	releaseWindow := func(wl winlink, closePanes bool) {
		delete(winGrid, wl.actor)
		if !unsubscribeView(wl.actor, wl.view) {
			return // another session still views this window
		}
		stopActor(wl.actor) // may defer to a relay zombie if still relaying
		if wl.view.silenceTmr != nil {
			wl.view.silenceTmr.Stop()
		}
		if closePanes {
			for _, p := range wl.actor.panes {
				p.Close()
			}
		}
	}

	// setActive mirrors the session's current-window choice into its view of that
	// window so the actor knows whether to render/fan out for this session. Toggles
	// this session's own view (wl.view), which disambiguates it from other sessions
	// viewing the same shared window.
	setActive := func(wl winlink, active bool) {
		actorDo(wl.actor, func() {
			wl.view.isActive = active
			// aggressive-resize: whether this window counts a session depends on
			// its being current there, so toggling current can resize it.
			wl.actor.recomputeSize(wl.view)
		})
	}

	// Subscribe this session's views and mark the first window current. A normal
	// session launches its own window's actor (start); a group member borrows the
	// target's already-running actors, so it only subscribes a view to each.
	if first != nil {
		windows[0].view = first.start(renderCh, s.events)
	} else {
		for i := range windows {
			windows[i].view = subscribeView(windows[i].actor, renderCh, s.events)
		}
	}
	setActive(windows[0], true)

	// Seed each window's size vote from this session's grid so the actor counts
	// us when combining viewers (a group member's vote can shrink a shared window).
	for i := range windows {
		wl := windows[i]
		var gc, gr int
		actorDo(wl.actor, func() {
			wl.actor.setViewSize(wl.view, cols, winRows(), aggressiveResize, windowSize)
			gc, gr = wl.actor.cols, wl.actor.rows
		})
		winGrid[wl.actor] = [2]int{gc, gr}
	}

	// fullSync reports the active window's full layout and every pane's
	// full content — needed whenever the client has no prior state to diff
	// against in the new arrangement (attach, split, close, resize, switch).
	fullSync := func() *proto.ServerMsg {
		w := activeWindow()
		var msg *proto.ServerMsg
		actorDo(w, func() {
			refreshBorderLabels(w)
			msg = &proto.ServerMsg{Layout: w.layout(), PaneContent: w.content()}
		})
		return msg
	}

	// sendLayout broadcasts a window's layout, reading it on the actor (layout()
	// touches the pane tree). Replaces bare send(ServerMsg{Layout: w.layout()}).
	sendLayout := func(w *windowActor) {
		var l *proto.Layout
		actorDo(w, func() { l = w.layout() })
		send(&proto.ServerMsg{Layout: l})
	}

	switchToWindow := func(idx int) {
		// active may be stale here when called from removeWindowAt (the slice was
		// already shrunk), so range-guard the old-window deactivate.
		var prevFocus *pane
		if active >= 0 && active < len(windows) {
			prevFocus = windows[active].actor.active
		}
		if idx != active && active >= 0 && active < len(windows) {
			setActive(windows[active], false) // old window stops rendering
		}
		if idx != active {
			lastWindow = active
		}
		active = idx
		setActive(windows[idx], true) // new current window renders + fans out
		// Viewing a window clears this session's pending alerts for it (per-view).
		wv := windows[idx].view
		wv.activity, wv.bell, wv.silence = false, false, false
		if wv.silenceTmr != nil {
			wv.silenceTmr.Stop()
			wv.silenceTmr = nil
		}
		send(fullSync())
		refocus(prevFocus)
		fireHook("after-select-window")
	}

	// selectLastWindow implements tmux's last-window / select-window -l: swap
	// to the window active before the current one, if it's still valid.
	selectLastWindow := func() {
		if lastWindow < len(windows) && lastWindow != active {
			switchToWindow(lastWindow)
		}
	}

	// closeClient drops one client (its connection and slot); the session
	// and its panes keep running for any others.
	closeClient := func(epoch int) {
		if a := attachments[epoch]; a != nil {
			if a.wantSnap {
				reg.wantSnapshot(-1)
			}
			a.conn.Close()
			delete(attachments, epoch)
		}
	}

	// anyEpoch returns some live attachment's epoch, or 0 if none — used to
	// re-point actingEpoch after the acting client leaves.
	anyEpoch := func() int {
		for ep := range attachments {
			return ep
		}
		return 0
	}

	// effectiveSize is the grid the window is laid out at: the acting client's
	// terminal (tmux window-size "latest"). Each client then clips or dot-fills
	// that grid to its own physical size. If the acting client just left, fall
	// back to any remaining one; with no clients, keep the current size so a
	// detached session holds its layout until someone reattaches.
	effectiveSize := func() (int, int) {
		switch windowSize {
		case "manual":
			// Grid is set explicitly by resize-window, not by any client — keep
			// whatever it currently is.
			return cols, rows
		case "smallest", "largest":
			smallest := windowSize == "smallest"
			bc, br := 0, 0
			for _, a := range attachments {
				if bc == 0 || (smallest && a.cols < bc) || (!smallest && a.cols > bc) {
					bc = a.cols
				}
				if br == 0 || (smallest && a.rows < br) || (!smallest && a.rows > br) {
					br = a.rows
				}
			}
			if bc > 0 {
				return bc, br
			}
		default: // "latest": the grid follows the acting (most recent) client.
			if a := attachments[actingEpoch]; a != nil {
				return a.cols, a.rows
			}
			if a := attachments[anyEpoch()]; a != nil {
				return a.cols, a.rows
			}
		}
		return cols, rows
	}

	// pushSize hands this session's size vote (its effectiveSize) + resize options
	// to a window's actor, which combines all its viewers' votes into the grid. It
	// returns whether the grid actually changed. Also seeds a new window's vote so
	// a session that later shares it counts toward its size.
	pushSize := func(wl winlink) bool {
		changed := false
		var gc, gr int
		actorDo(wl.actor, func() {
			changed = wl.actor.setViewSize(wl.view, cols, winRows(), aggressiveResize, windowSize)
			gc, gr = wl.actor.cols, wl.actor.rows
		})
		winGrid[wl.actor] = [2]int{gc, gr}
		return changed
	}

	// revoteWindows re-pushes this session's vote to every window (after a size or
	// window-size/aggressive-resize change) and reports whether the CURRENT window's
	// grid changed. Other viewers of a shared window are told via windowResized.
	revoteWindows := func() bool {
		curChanged := false
		for i := range windows {
			if pushSize(windows[i]) && i == active {
				curChanged = true
			}
		}
		return curChanged
	}

	// applySize recomputes this session's grid from its clients and re-votes every
	// window; returns true when the CURRENT window's grid changed (so the caller
	// full-syncs).
	applySize := func() bool {
		nc, nr := effectiveSize()
		if nc == cols && nr == rows {
			return false
		}
		cols, rows = nc, nr
		return revoteWindows()
	}

	// detachEpoch drops one specific client. If it was the acting one, the
	// window follows it, so re-point to a remaining client to resize the grid.
	detachEpoch := func(epoch int) {
		closeClient(epoch)
		if epoch == actingEpoch {
			actingEpoch = anyEpoch()
		}
		if applySize() {
			send(fullSync())
		}
		if destroyUnattached && len(attachments) == 0 {
			done = true
		}
	}
	detach := func() { detachEpoch(actingEpoch) }

	// switchToSession hands the acting client off to another live session (the
	// choose-session picker and switch-client share this): record where it came
	// from (for switch-client -l), tell the client to reconnect, then detach here.
	switchToSession := func(name string, winIdx int) {
		if name == "" || name == s.name {
			return
		}
		peer, ok := reg.get(name)
		if !ok {
			return
		}
		// choose-tree window row: focus the chosen window in the target session
		// before the client reattaches, so it lands on that window. Runs on the
		// peer's goroutine (a different one than ours), so it can't self-deadlock.
		if winIdx >= 0 {
			peer.command([]string{"select-window", "-t", strconv.Itoa(winIdx)})
		}
		reg.setLastSession(s.name)
		sendTo(actingEpoch, &proto.ServerMsg{SwitchSession: name})
		detach()
		fireHook("client-session-changed")
	}

	// adjacentSession returns the session `step` places from `cur` in the sorted
	// session list (switch-client -n = +1, -p = -1), wrapping. "" if there's no
	// other session.
	adjacentSession := func(cur string, step int) string {
		names := reg.names()
		if len(names) < 2 {
			return ""
		}
		for i, n := range names {
			if n == cur {
				return names[((i+step)%len(names)+len(names))%len(names)]
			}
		}
		return ""
	}

	createWindow := func(command, dir, name string) {
		if dir == "" {
			dir = activeWindow().active.cwd()
		}
		win, err := newWindow(cols, winRows(), dir, command, s.name, sessionEnv, reg.globalEnvCopy())
		if err != nil {
			log.Printf("create window: %v", err)
			return
		}
		if name != "" {
			win.rename(name) // new-window -n: manual name wins over auto/OSC naming
		}
		w := newWindowActor(win)
		initWindowBorder(w) // reflow before the goroutine starts (no concurrency yet)
		vw := w.start(renderCh, s.events)
		watchPane(w.active)
		windows = append(windows, winlink{actor: w, view: vw})
		pushSize(windows[len(windows)-1]) // seed the vote so a later sharer counts us
		switchToWindow(len(windows) - 1)
		fireHook("after-new-window")
	}

	removeWindowAt := func(idx int) {
		wl := windows[idx]
		windows = append(windows[:idx], windows[idx+1:]...)
		// Drop this session's view of the window. If it was the last viewer the
		// actor stops and its panes close (closePanes is a no-op when the window is
		// already empty, e.g. the last pane just exited); a window another session
		// still links keeps running. The window is out of the slice now, so the
		// switchToWindow/setActive below can't target it.
		releaseWindow(wl, true)
		if len(windows) == 0 {
			done = true
			return
		}
		// Keep lastWindow pointing at the same window across the index shift;
		// if that window is the one removed, there's no meaningful last.
		switch {
		case idx == lastWindow:
			lastWindow = active
		case idx < lastWindow:
			lastWindow--
		}
		switch {
		case idx < active:
			active--
		case idx == active:
			switchToWindow(active % len(windows))
		}
	}

	splitPane := func(dir splitDir, command, cwd string) {
		w := activeWindow()
		var err error
		grew := false
		actorDo(w, func() {
			before := len(w.panes)
			err = w.split(dir, command, cwd)
			grew = len(w.panes) > before
		})
		if err != nil {
			log.Printf("split: %v", err)
			return
		}
		if grew {
			watchPane(w.active)
			send(fullSync())
			fireHook("after-split-window")
		}
	}

	// closePaneAt kills pane p in window w (index idx). tmux's kill-pane honors
	// -t, so the target comes from resolveTarget, not always the active pane.
	closePaneAt := func(w *windowActor, p *pane, idx int) {
		p.Close()
		survived := false
		actorDo(w, func() { survived = w.closePane(p) })
		if !survived {
			removeWindowAt(idx)
			return
		}
		send(fullSync())
		fireHook("pane-exited") // window survived the close (see removeWindowAt path above)
	}

	navigate := func(dir string) {
		w := activeWindow()
		wasZoomed, moved := false, false
		actorDo(w, func() {
			if w.zoomed {
				// Selecting another pane drops out of zoom first, like tmux;
				// adjacency needs the restored tiled rects anyway.
				wasZoomed = true
				w.unzoom()
				if adj := w.adjacent(dir); adj != nil {
					w.lastActive = w.active
					w.setActive(adj)
				}
			} else if adj := w.adjacent(dir); adj != nil {
				moved = true
				w.lastActive = w.active
				w.setActive(adj)
			}
		})
		if wasZoomed {
			send(fullSync())
		} else if moved {
			sendLayout(w)
		}
	}

	// selectLastPane implements tmux's select-pane -l: swap to the pane that
	// was active before the current one, if it still exists.
	selectLastPane := func() {
		w := activeWindow()
		swapped := false
		actorDo(w, func() {
			if w.lastActive == nil || w.lastActive == w.active {
				return
			}
			for _, p := range w.panes {
				if p == w.lastActive {
					prev := w.active
					w.setActive(w.lastActive)
					w.lastActive = prev
					swapped = true
					return
				}
			}
		})
		if swapped {
			sendLayout(w)
		}
	}

	toggleZoom := func() {
		w := activeWindow()
		actorDo(w, func() { w.toggleZoom() })
		send(fullSync())
	}

	swapPane := func(dir string) {
		w := activeWindow()
		actorDo(w, func() { w.swapPane(dir) })
		send(fullSync())
	}

	// swapWindow moves the active window one slot left ("prev") or right
	// ("next") in the window list, wrapping around; the moved window stays
	// active. tmux's move-window/swap-window, reduced to neighbor swaps.
	swapWindow := func(dir string) {
		if len(windows) < 2 {
			return
		}
		other := (active + 1) % len(windows)
		if dir == "prev" {
			other = (active - 1 + len(windows)) % len(windows)
		}
		windows[active], windows[other] = windows[other], windows[active]
		active = other
		send(fullSync())
	}

	openPicker := func(title, verb string, filter bool, items, targets []string, previews [][]emu.Line, sel int) {
		sendTo(actingEpoch, &proto.ServerMsg{OpenPicker: &proto.OpenPicker{
			Title: title, Verb: verb, Items: items, Targets: targets, Filter: filter, Previews: previews, Sel: sel,
		}})
	}

	// sessionPreview returns a static styled snapshot of session n's active pane
	// (last ~12 non-blank rows, real colors), for the picker preview. Self reads
	// local state; a peer replies via its own goroutine (like chooseTree's
	// list-windows) — a wedged peer stalls the picker open (interactive, acceptable).
	sessionPreview := func(n string) []emu.Line {
		if n == s.name {
			return previewSnap(activeWindow().active.term.Screen())
		}
		if other, ok := reg.get(n); ok {
			return other.previewLines()
		}
		return nil
	}

	// chooseSession opens a client-side picker: the server sends the session
	// list and the client owns navigation, sending back {switch-session, name}
	// as an Action on Enter. (choose-window is built client-side from the
	// window list the client already mirrors — no server round-trip.)
	chooseSession := func() {
		names := reg.names()
		items := make([]string, len(names))
		previews := make([][]emu.Line, len(names))
		sel := 0
		for i, n := range names {
			items[i] = n
			if n == s.name {
				items[i] += " (attached)"
				sel = i // open on the attached session, not the top of the list
			}
			previews[i] = sessionPreview(n)
		}
		openPicker("choose session", "switch-session", false, items, names, previews, sel)
	}

	// chooseTree opens a filterable session→window tree picker: each session is
	// a header row, its windows indented beneath. Selecting a window row switches
	// the client to that session and focuses the window (a header just switches).
	// Other sessions' window names come from a cross-session list-windows (their
	// own goroutine); ours comes from local state to avoid a self-deadlock.
	chooseTree := func(filter string) {
		var items, targets []string
		var previews [][]emu.Line
		for _, n := range reg.names() {
			// -f <format-expr>: keep only sessions the expression is truthy for
			// (e.g. workspacer's `#{m:prefix-*,#{session_name}}`). Evaluated with
			// the session's own vars via the pure format engine.
			if filter != "" {
				if r := format.Expand(filter, map[string]string{"session_name": n}); r == "" || r == "0" {
					continue
				}
			}
			// One capture per session, reused for its header + window rows.
			preview := sessionPreview(n)
			header := n
			if n == s.name {
				header += " (attached)"
			}
			items = append(items, header)
			targets = append(targets, "switch-session '"+n+"'")
			previews = append(previews, preview)

			var wtext string
			if n == s.name {
				var b strings.Builder
				for i, lk := range windows {
					if i > 0 {
						b.WriteByte('\n')
					}
					fmt.Fprintf(&b, "%d\t%s", i+baseIndex, windowName(lk.actor))
				}
				wtext = b.String()
			} else if other, ok := reg.get(n); ok {
				// ponytail: blocks our goroutine on the peer's until it replies. A
				// wedged/flooded peer stalls the picker open, and two clients opening
				// choose-tree at once could deadlock (each blocked on the other's
				// reply). Fine for interactive use; make it async with a timeout if it
				// ever bites.
				wtext, _ = other.command([]string{"list-windows", "-F", "#{window_index}\t#{window_name}"})
			}
			for _, line := range strings.Split(wtext, "\n") {
				tab := strings.IndexByte(line, '\t')
				if tab < 0 {
					continue
				}
				idx, name := line[:tab], line[tab+1:]
				items = append(items, "  "+idx+": "+name)
				previews = append(previews, preview)
				if n == s.name {
					targets = append(targets, "select-window -t "+idx)
				} else {
					targets = append(targets, "switch-session '"+n+"' "+idx)
				}
			}
		}
		openPicker("choose tree", "run", true, items, targets, previews, 0) // ponytail: header/window rows + filter make preselect fiddly; top is fine
	}

	// gatherClients enumerates every client across all sessions (self read
	// locally, peers via their own goroutine), sorted by session then epoch.
	// Shared by list-clients and choose-client. gtmux has no client tty, so
	// clients are id'd by (session, epoch).
	gatherClients := func() []clientInfo {
		var infos []clientInfo
		for _, n := range reg.names() {
			if n == s.name {
				for ep, a := range attachments {
					infos = append(infos, clientInfo{session: s.name, epoch: ep, cols: a.cols, rows: a.rows})
				}
			} else if other, ok := reg.get(n); ok {
				infos = append(infos, other.clients()...)
			}
		}
		sort.Slice(infos, func(i, j int) bool {
			if infos[i].session != infos[j].session {
				return infos[i].session < infos[j].session
			}
			return infos[i].epoch < infos[j].epoch
		})
		return infos
	}

	// chooseClient opens a filterable picker of every connected client;
	// selecting one detaches it (tmux choose-client). Server-originated: the
	// client list lives server-side.
	chooseClient := func() {
		infos := gatherClients()
		items := make([]string, len(infos))
		targets := make([]string, len(infos))
		for i, ci := range infos {
			items[i] = fmt.Sprintf("client-%d: %s [%dx%d]", ci.epoch, ci.session, ci.cols, ci.rows)
			// Quote the target so a spaced session name stays one arg through
			// the client tokenizer.
			targets[i] = fmt.Sprintf("detach-client -t 'client-%d@%s'", ci.epoch, ci.session)
		}
		openPicker("choose client", "run", true, items, targets, nil, 0)
	}

	// chooseBuffer opens a picker of the paste buffers; selecting one pastes it
	// (reusing the display-menu "run" verb: each target is a paste-buffer
	// command). Server-originated because the buffers live server-side.
	chooseBuffer := func() {
		if len(buffers) == 0 {
			return
		}
		items := make([]string, len(buffers))
		targets := make([]string, len(buffers))
		for i, b := range buffers {
			sample := strings.ReplaceAll(b.data, "\n", " ")
			if len(sample) > 50 {
				sample = sample[:50]
			}
			items[i] = b.name + ": " + sample
			// Single-quote the name so the client's tokenizer keeps a spaced name
			// as one arg.
			targets[i] = "paste-buffer -b '" + b.name + "'"
		}
		openPicker("choose buffer", "run", false, items, targets, nil, 0)
	}

	// breakPane moves the active pane out into its own new window (prefix+!).
	breakPane := func() {
		w := activeWindow()
		if len(w.panes) < 2 {
			return // already a whole window by itself
		}
		p := w.active
		// Build p's new window without reflowing (no p.term touch) — the origin is
		// still p's sole writer until the relay handoff below.
		nw := newWindowActor(adoptWindow(p, cols, winRows(), s.name))
		nw.borderStatus, nw.borderFormat = paneBorderStatus, paneBorderFormat
		actorDo(w, func() { w.closePane(p) }) // remove p from the old window's tree
		setRelay(p, nw)                       // origin now forwards p's output to nw (stops applying it)
		nwView := nw.start(renderCh, s.events)
		windows = append(windows, winlink{actor: nw, view: nwView})
		actorDo(nw, func() { nw.reflow() }) // nw is p's sole writer now; reflow on its goroutine
		pushSize(windows[len(windows)-1])   // seed the vote so a later sharer counts us
		switchToWindow(len(windows) - 1)
	}

	// marked is the pane tagged by prefix+m, the target for joinMarked —
	// tmux's mark-and-join-pane workflow, standing in for join-pane's -s
	// flag until there's a command mode.
	var marked *pane

	// setMarked moves the mark to p (or clears it when p is nil), keeping the
	// pane's own flag — read by layout() for the client's border indicator —
	// in sync with the session pointer.
	setMarked := func(p *pane) {
		// p.marked is read by layout() on the actor, so writes go through it.
		if marked != nil {
			old := marked
			actorDo(old.win.actor, func() { old.marked = false })
		}
		marked = p
		if p != nil {
			actorDo(p.win.actor, func() { p.marked = true })
		}
	}

	markPane := func() {
		p := activeWindow().active
		if marked == p {
			setMarked(nil)
		} else {
			setMarked(p)
		}
		sendLayout(activeWindow())
	}

	// joinPaneOp moves pane p out of its window and into dst, splitting dst's
	// pane `at` along dir (tmux join-pane/move-pane). Drops p's source window if
	// p was its last pane (removeWindowAt keeps active/lastWindow valid). No-op
	// if p already lives in dst or its window isn't in this session (cross-session
	// join is unsupported). p must still be alive in its window — the caller
	// checks that.
	joinPaneOp := func(p *pane, dst *windowActor, at *pane, dir splitDir) {
		src := p.win
		if src == dst.window {
			return // can't join a pane into its own window
		}
		srcIdx := -1
		for i, wl := range windows {
			if wl.actor.window == src {
				srcIdx = i
				break
			}
		}
		if srcIdx == -1 {
			return // source not in this session
		}
		lastInSrc := false
		actorDo(src.actor, func() { lastInSrc = !src.closePane(p) }) // remove p from src's tree
		setRelay(p, dst)                                             // origin forwards p to dst (src stops applying it)
		if lastInSrc {
			removeWindowAt(srcIdx) // src is empty now; canonical drop fixes indices/active
		}
		actorDo(dst, func() { dst.joinPaneAt(p, at, dir) }) // dst owns p now; reflows on its goroutine
		send(fullSync())
	}

	// joinMarked pulls the marked pane out of its window and stacks it under the
	// current active pane — the prefix+m / prefix+g approximation of join-pane -s.
	joinMarked := func() {
		if marked == nil || marked.win == activeWindow().window {
			return
		}
		alive := false
		for _, pp := range marked.win.panes {
			if pp == marked {
				alive = true
				break
			}
		}
		if !alive {
			setMarked(nil) // the marked pane's shell exited in the meantime
			return
		}
		p := marked
		setMarked(nil)
		w2 := activeWindow()
		joinPaneOp(p, w2, w2.active, splitHorizontal)
	}

	resizePane := func(dir string, n int) {
		w := activeWindow()
		actorDo(w, func() { w.resizePane(dir, n) })
		send(fullSync())
	}

	showMessage := func(text string) {
		statusMsg = text
		msgLog = append(msgLog, time.Now().Format("15:04:05")+" "+text)
		if messageLimit > 0 && len(msgLog) > messageLimit {
			msgLog = msgLog[len(msgLog)-messageLimit:]
		}
		send(&proto.ServerMsg{})
		time.AfterFunc(time.Duration(displayTime)*time.Millisecond, func() { s.events <- clearMessageEvent{} })
	}

	// handleRender processes one renderMsg a window actor produced on output: fan
	// the diff out to clients (content is non-nil only for the current window's
	// visible pane), and run activity/bell/silence detection (session-owned flags).
	handleRender = func(rm renderMsg) {
		p := rm.pane
		w := p.win
		if rm.content != nil {
			send(&proto.ServerMsg{PaneContent: []proto.PaneContent{*rm.content}})
		}
		// The pane's app toggled mouse tracking: push the layout so clients
		// refresh PaneRect.WantsMouse (own-vs-forward decision for that pane).
		// Only for the current window — sendLayout is unconditional and the
		// client applies whatever Layout it gets, so resending a background
		// window's here would paint it over the foreground. A background
		// window's fresh WantsMouse arrives when it's switched to (switchToWindow
		// re-pushes Layout).
		if rm.modeFlip && w == activeWindow().window {
			sendLayout(w.actor)
		}
		// allow-passthrough: forward the un-doubled DCS payload raw to this
		// session's clients when the option is on. Two gates: the actor set hostOut
		// only for a view that saw the emitting pane (current window, not zoom-hidden),
		// AND we recheck the current window here — the rm may have been queued before
		// a select-window, so its window could no longer be the one on screen. The
		// scanner always strips the wrapper from emu input regardless; this is only
		// the forward.
		if len(rm.hostOut) > 0 && allowPassthrough && w == activeWindow().window {
			sendPassthrough(rm.hostOut)
		}
		// Alert state is per-view: find THIS session's view of the window. A render
		// for a window we no longer link (winlink just dropped) has no view — skip.
		wi := indexOfWindow(windows, w)
		if wi < 0 {
			return
		}
		wv := windows[wi].view
		// OSC 133 command-finished: notify this session's clients so a Lua
		// gtmux.on("command-exited") callback can react (e.g. flag the pane).
		for _, code := range rm.cmdExits {
			send(&proto.ServerMsg{CommandExits: []proto.CommandExit{{
				Session: s.name, Window: wi + baseIndex, PaneID: p.id, ExitCode: code,
			}}})
		}
		// monitor-silence: any output clears the flag and rearms the timer.
		wv.silence = false
		sil := monitorSilence
		if v, ok := w.opts["monitor_silence"]; ok {
			if n, err := strconv.Atoi(v); err == nil {
				sil = n
			}
		}
		if wv.silenceTmr != nil {
			wv.silenceTmr.Stop()
			wv.silenceTmr = nil
		}
		if sil > 0 {
			tv, tw := wv, w
			wv.silenceTmr = time.AfterFunc(time.Duration(sil)*time.Second, func() {
				s.events <- silenceEvent{view: tv, window: tw}
			})
		}
		// monitor-activity / monitor-bell: raise an alert, scoped by
		// activity-action / bell-action (which windows count — any/none/current/
		// other). tmux defaults: activity "other" (only windows you're not looking
		// at), bell "any" (including the current one).
		isCurrent := w == activeWindow().window
		monA, monB := monitorActivity, monitorBell
		if v, ok := w.opts["monitor_activity"]; ok {
			monA = onOff(v)
		}
		if v, ok := w.opts["monitor_bell"]; ok {
			monB = onOff(v)
		}
		if monA && !wv.activity && alertFires(activityAction, isCurrent) {
			wv.activity = true
			send(&proto.ServerMsg{})
			fireHook("alert-activity")
			if visualActivity {
				showMessage(fmt.Sprintf("activity in window %d", wi+baseIndex))
			}
		}
		if monB && !wv.bell && rm.bell && alertFires(bellAction, isCurrent) {
			wv.bell = true
			send(&proto.ServerMsg{})
			fireHook("alert-bell")
			if visualBell {
				showMessage(fmt.Sprintf("bell in window %d", wi+baseIndex))
			}
		}
	}

	// enterCopyModeAction hands the acting client a frozen snapshot of the
	// active pane and lets it run copy-mode locally (client-owned copy-mode).
	// Per-client: only the client that asked enters copy-mode.
	enterCopyModeAction := func(pageUp bool) {
		w := activeWindow()
		p := w.active
		var snap *proto.CopyModeEnter
		actorDo(w, func() { snap = p.copySnapshot() }) // grid read on the actor
		if pageUp {
			snap.CursorY -= p.rect.Rows // prefix+PgUp starts a page up, like tmux
		}
		sendTo(actingEpoch, &proto.ServerMsg{CopyModeEnter: snap})
	}

	// destroyWindow removes a window from EVERY session that links it — for
	// kill-window and for a window whose last pane exited (it's gone for good).
	// Each other viewer is told to drop its winlink (winlinkGone); they unlink on
	// their own goroutines, and the last unsubscribe (mine or theirs) stops the
	// actor + closes the panes. ponytail: the notify send is blocking on a peer's
	// event channel — fine, peers drain it; a hung peer is the same rare risk as
	// any cross-session message.
	destroyWindow := func(wl winlink) {
		var others []chan<- any
		actorDo(wl.actor, func() {
			for _, v := range wl.actor.views {
				if v != wl.view {
					others = append(others, v.notify)
				}
			}
		})
		for _, ch := range others {
			ch <- winlinkGone{actor: wl.actor}
		}
		if idx := indexOfWindow(windows, wl.actor.window); idx >= 0 {
			removeWindowAt(idx)
		}
	}


	// linkWindow links a window from another session into this one: it obtains
	// that window's actor from the source session (a request on the source's event
	// channel) and subscribes a view — so both sessions display the same live
	// window. spec is "session:window".
	linkWindow := func(spec string) string {
		srcSession, winSpec := s.name, spec
		if i := strings.IndexByte(spec, ':'); i >= 0 {
			if spec[:i] != "" {
				srcSession = spec[:i]
			}
			winSpec = spec[i+1:]
		}
		if srcSession == s.name {
			return "link-window: source and target are the same session"
		}
		src, ok := reg.get(srcSession)
		if !ok {
			return "link-window: no such session: " + srcSession
		}
		reply := make(chan *windowActor, 1)
		src.events <- linkRequest{spec: winSpec, reply: reply}
		// Wait for the source to resolve it, but time out rather than hang: if both
		// sessions link each other at the same instant, each is blocked here and
		// can't service the other's request — a rare mutual deadlock the timeout
		// breaks (both link commands just fail).
		var wa *windowActor
		select {
		case wa = <-reply:
		case <-time.After(2 * time.Second):
			return "link-window: source session did not respond"
		}
		if wa == nil {
			return "link-window: no such window: " + spec
		}
		if indexOfWindow(windows, wa.window) >= 0 {
			return "link-window: already linked in this session"
		}
		vw := subscribeView(wa, renderCh, s.events)
		windows = append(windows, winlink{actor: wa, view: vw})
		pushSize(windows[len(windows)-1]) // our size now counts toward the shared window
		switchToWindow(len(windows) - 1)
		return ""
	}

	// unlinkWindow drops this session's link to the current window (its view). A
	// window still linked elsewhere survives; if this was the last viewer it's
	// destroyed (like kill-window).
	unlinkWindow := func() {
		removeWindowAt(active)
	}

	// showPaneNumbers implements tmux's display-panes (prefix+q): flash each
	// pane's index for a second, then redraw without it.
	showPaneNumbers := func() {
		w := activeWindow()
		actorDo(w, func() { w.showNumbers = true; w.paneBase = paneBaseIndex })
		sendLayout(w)
		time.AfterFunc(time.Second, func() {
			s.events <- hideNumbersEvent{window: w.window}
		})
	}

	// flagDir maps tmux's directional flags to navigate/resize directions.
	flagDir := map[string]string{"-L": "left", "-R": "right", "-U": "up", "-D": "down"}

	// resolveWindowSpec maps a target's window component to a window index in
	// this session, or -1. Handles index (base-index-adjusted), name, and
	// relative forms (+ / - / +N / -N / {next} / {previous} / {last} / {start} /
	// {end}).
	resolveWindowSpec := func(spec string) int {
		n := len(windows)
		switch spec {
		case "":
			return active
		case "+", "{next}":
			return (active + 1) % n
		case "-", "{previous}", "{prev}":
			return (active - 1 + n) % n
		case "{last}":
			return lastWindow
		case "{start}":
			return 0
		case "{end}":
			return n - 1
		}
		if d, err := strconv.Atoi(spec[1:]); err == nil && spec[0] == '+' {
			return ((active+d)%n + n) % n
		}
		if d, err := strconv.Atoi(spec[1:]); err == nil && spec[0] == '-' {
			return ((active-d)%n + n) % n
		}
		if idx, err := strconv.Atoi(spec); err == nil && idx >= baseIndex && idx < baseIndex+n {
			return idx - baseIndex
		}
		for wi, wl := range windows {
			if windowName(wl.actor) == spec {
				return wi
			}
		}
		return -1
	}

	// resolvePaneSpec maps a target's pane component to a pane in window w, or
	// nil. Handles index (pane-base-index-adjusted), %id, relative (+ / - / +N /
	// -N), and {last}.
	resolvePaneSpec := func(w *windowActor, spec string) *pane {
		switch {
		case spec == "":
			return w.active
		case spec == "{last}":
			return w.lastActive
		case strings.HasPrefix(spec, "%"):
			if id, err := strconv.Atoi(spec[1:]); err == nil {
				for _, p := range w.panes {
					if p.id == id {
						return p
					}
				}
			}
			return nil
		}
		cur, m := 0, len(w.panes)
		for i, p := range w.panes {
			if p == w.active {
				cur = i
				break
			}
		}
		rel := func(sign int) *pane {
			d := 1
			if len(spec) > 1 {
				v, err := strconv.Atoi(spec[1:])
				if err != nil {
					return nil
				}
				d = v
			}
			return w.panes[((cur+sign*d)%m+m)%m]
		}
		switch spec[0] {
		case '+':
			return rel(1)
		case '-':
			return rel(-1)
		}
		if idx, err := strconv.Atoi(spec); err == nil && idx >= paneBaseIndex && idx < paneBaseIndex+m {
			return w.panes[idx-paneBaseIndex]
		}
		return nil
	}

	// resolveTarget pulls a `-t <target>` out of args and resolves it to a
	// window+pane in this session. Grammar: [session:][window][.pane]; the
	// session component is resolved at the dispatch layer (cross-session routing
	// in server.go), so it's stripped and ignored here. A bare %id is a global
	// pane lookup across every window. Missing or unknown -> the active
	// window/pane (targeted=false). Returns the stripped args.
	// resolvePaneStr resolves a `[sess:]window.pane` / bare `%id` target string to
	// a window/pane in THIS session (the session component is already routed away
	// by the dispatch layer). ok is false for an unresolvable/cross-session spec.
	resolvePaneStr := func(target string) (tw *windowActor, twi int, tp *pane, ok bool) {
		if c := strings.Index(target, ":"); c >= 0 {
			target = target[c+1:] // strip session component (already routed)
		}
		// Bare global pane id (no window.pane split).
		if strings.HasPrefix(target, "%") && !strings.Contains(target, ".") {
			if id, err := strconv.Atoi(target[1:]); err == nil {
				for wi, wl := range windows {
					for _, p := range wl.actor.panes {
						if p.id == id {
							return wl.actor, wi, p, true
						}
					}
				}
			}
			return nil, -1, nil, false
		}
		winSpec, paneSpec := target, ""
		if d := strings.LastIndex(target, "."); d >= 0 {
			winSpec, paneSpec = target[:d], target[d+1:]
		}
		if wi := resolveWindowSpec(winSpec); wi >= 0 {
			w := windows[wi].actor
			p := resolvePaneSpec(w, paneSpec)
			if p == nil {
				p = w.active
			}
			return w, wi, p, true
		}
		return nil, -1, nil, false
	}

	resolveTarget := func(args []string) (tw *windowActor, tp *pane, twi int, rest []string, targeted bool) {
		tw, twi, tp = activeWindow(), active, activeWindow().active
		for i := 0; i < len(args); i++ {
			if args[i] == "-t" && i+1 < len(args) {
				if w, wi, p, ok := resolvePaneStr(args[i+1]); ok {
					tw, twi, tp, targeted = w, wi, p, true
				}
				i++
				continue
			}
			rest = append(rest, args[i])
		}
		return tw, tp, twi, rest, targeted
	}

	// joinPaneCmd implements join-pane/move-pane: -s source pane (default the
	// active pane), -t target pane whose window it joins and whose pane it splits
	// (default the active window/pane), -h side by side / -v stacked (default).
	// Within-session only — a cross-session -s resolves to nothing here and no-ops.
	joinPaneCmd := func(args []string) {
		srcSpec, dstSpec := "", ""
		dir := splitHorizontal // tmux's default join is stacked; -h makes it side by side
		for i := 0; i < len(args); i++ {
			switch args[i] {
			case "-s":
				if i+1 < len(args) {
					srcSpec = args[i+1]
					i++
				}
			case "-t":
				if i+1 < len(args) {
					dstSpec = args[i+1]
					i++
				}
			case "-h":
				dir = splitVertical
			case "-v", "-b", "-d":
				// -v explicit stacked (default); -b before / -d don't-focus not modeled.
			}
		}
		srcPane := activeWindow().active
		if srcSpec != "" {
			_, _, p, ok := resolvePaneStr(srcSpec)
			if !ok {
				return
			}
			srcPane = p
		}
		dstW, at := activeWindow(), activeWindow().active
		if dstSpec != "" {
			w, _, p, ok := resolvePaneStr(dstSpec)
			if !ok {
				return
			}
			dstW, at = w, p
		}
		joinPaneOp(srcPane, dstW, at, dir)
	}

	// paneVars builds the #{...} variable map for one pane, for the info
	// commands' format expansion. ponytail: git_branch shells out per call
	// (fine on-demand); the status bar's own path stays cached.
	paneVars := func(w *windowActor, p *pane, wi int) map[string]string {
		pi := 0
		for i, pp := range w.panes {
			if pp == p {
				pi = i + paneBaseIndex
			}
		}
		return withUserOpts(map[string]string{
			"host": hostname, "session": s.name, "session_created": created,
			"window_name": windowName(w), "window_index": strconv.Itoa(wi + baseIndex),
			"pane_title":         p.term.Title(),
			"window_zoomed_flag": bitStr(w.zoomed),
			"clock":              cachedClock, "git_branch": gitBranch(p.cwd()),
			"pane_path": p.cwd(), "pane_command": p.currentCommand(),
			"pane_id": fmt.Sprintf("%%%d", p.id), "pane_index": strconv.Itoa(pi),
			"pane_active": bitStr(p == w.active),
			"pane_width":  strconv.Itoa(p.rect.Cols), "pane_height": strconv.Itoa(p.rect.Rows),
			"pane_left": strconv.Itoa(p.rect.Col), "pane_top": strconv.Itoa(p.rect.Row),
			"pane_right": strconv.Itoa(p.rect.Col + p.rect.Cols - 1), "pane_bottom": strconv.Itoa(p.rect.Row + p.rect.Rows - 1),
		})
	}
	// bufferVars builds the #{...} map for one paste buffer (list-buffers). The
	// sample is the content flattened to one line, capped for the listing.
	bufferVars := func(b pbuf) map[string]string {
		sample := strings.ReplaceAll(b.data, "\n", " ")
		if len(sample) > 50 {
			sample = sample[:50]
		}
		return map[string]string{
			"buffer_name":   b.name,
			"buffer_size":   strconv.Itoa(len(b.data)),
			"buffer_sample": sample,
		}
	}
	// windowVars builds the #{...} map for one window (list-windows). wi is the
	// wi is the 0-based slice index; window_index applies base-index.
	windowVars := func(wl winlink, wi int) map[string]string {
		w := wl.actor
		// The window owns its grid (it can differ from this session's vote when
		// another, smaller viewer shares it) — read the cached real size.
		ww, wh := cols, winRows()
		if g, ok := winGrid[w]; ok {
			ww, wh = g[0], g[1]
		}
		return withUserOpts(map[string]string{
			"session":              s.name,
			"session_name":         s.name,
			"window_index":         strconv.Itoa(wi + baseIndex),
			"window_name":          windowName(w),
			"window_active":        bitStr(wi == active),
			"window_panes":         strconv.Itoa(len(w.panes)),
			"window_width":         strconv.Itoa(ww),
			"window_height":        strconv.Itoa(wh),
			"window_zoomed_flag":   bitStr(w.zoomed),
			"window_activity_flag": bitStr(wl.view.activity),
			"window_bell_flag":     bitStr(wl.view.bell),
			"window_silence_flag":  bitStr(wl.view.silence),
		})
	}
	// loopVars provides the per-item var maps for #{S:}/#{W:}/#{P:} format loops:
	// every session (S), this session's windows (W), and the active window's panes
	// (P). ponytail: flat only — a nested #{W:#{P:}} reuses the active window's P.
	loopVars := func(kind string) []map[string]string {
		switch kind {
		case "W":
			out := make([]map[string]string, len(windows))
			for i, wl := range windows {
				out[i] = windowVars(wl, i)
			}
			return out
		case "P":
			w := activeWindow()
			out := make([]map[string]string, len(w.panes))
			for i, p := range w.panes {
				out[i] = paneVars(w, p, active)
			}
			return out
		case "S":
			names := reg.names()
			out := make([]map[string]string, len(names))
			for i, n := range names {
				out[i] = map[string]string{"session": n, "session_name": n}
			}
			return out
		}
		return nil
	}
	// The ctrl key handed to vim per direction when a vim-nav bind fires over a
	// vim pane (C-h/j/k/l, C-\ for last) — mirrors vim-tmux-navigator's mapping.
	vimNavKey := map[string]byte{"-L": 0x08, "-D": 0x0a, "-U": 0x0b, "-R": 0x0c, "-l": 0x1c}

	// runCommand executes one command (prefix+: line, or `gtmux run` from
	// outside). Names and flags follow tmux; commands act on the active
	// pane/window (no -t targets). Returns an error message, or "".
	runCommand := func(fields []string) string {
		if len(fields) == 0 {
			return ""
		}
		cmd, args := fields[0], fields[1:]
		// command-alias: a bare command name that matches an alias is replaced by
		// its expansion (tmux resolves these at dispatch), with the original args
		// appended — e.g. alias "x=split-window -h" run as `x foo` → `split-window
		// -h foo`.
		if exp, ok := cmdAlias[cmd]; ok {
			if ef := strings.Fields(exp); len(ef) > 0 {
				args = append(ef[1:], args...)
				cmd = ef[0]
			}
		}
		rest := strings.Join(args, " ")
		switch cmd {
		case "split-window":
			dir, command, _, horiz := parseSpawn(args)
			d := splitHorizontal
			if horiz {
				d = splitVertical
			}
			splitPane(d, command, dir)
		case "new-window":
			dir, command, name, _ := parseSpawn(args)
			createWindow(command, dir, name)
		case "new-session":
			// new-session [-d] [-s name] [-c dir]: always detached (the acting
			// client can't be moved mid-command). ponytail: no [command] arg
			// (first pane is a shell) and no -A attach-or-create.
			nsName, nsDir := "", ""
			for i := 0; i < len(args); i++ {
				switch args[i] {
				case "-s":
					if i+1 < len(args) {
						nsName, i = args[i+1], i+1
					}
				case "-c":
					if i+1 < len(args) {
						nsDir, i = args[i+1], i+1
					}
				}
			}
			if nsDir == "" {
				nsDir = activeWindow().active.cwd()
			}
			if _, err := reg.resolveGroup(nsName, true, cols, rows, nsDir, ""); err != nil {
				cmdOut = "new-session: " + err.Error()
			}
		case "next-window":
			switchToWindow((active + 1) % len(windows))
		case "previous-window":
			switchToWindow((active - 1 + len(windows)) % len(windows))
		case "select-window":
			// -l = last window; -t <target> = full target syntax; a bare arg is a
			// base-index-adjusted window number (the client's status click / digit
			// bind / choose-window picker all send the displayed index). All forms
			// resolve through the base-index-aware spec resolvers.
			switch {
			case len(fields) > 1 && fields[1] == "-l":
				selectLastWindow()
			case len(fields) > 2 && fields[1] == "-t":
				if _, _, twi, _, targeted := resolveTarget(fields[1:]); targeted {
					switchToWindow(twi)
				}
			case len(fields) > 1:
				if wi := resolveWindowSpec(fields[1]); wi >= 0 {
					switchToWindow(wi)
				}
			}
		case "last-window":
			selectLastWindow()
		case "select-layout":
			if len(args) > 0 {
				w := activeWindow()
				actorDo(w, func() { w.setLayout(args[0], mainPaneW, mainPaneH) })
				send(fullSync())
			}
		case "next-layout":
			w := activeWindow()
			actorDo(w, func() { w.cycleLayout(1, mainPaneW, mainPaneH) })
			send(fullSync())
		case "previous-layout":
			w := activeWindow()
			actorDo(w, func() { w.cycleLayout(-1, mainPaneW, mainPaneH) })
			send(fullSync())
		case "set-option", "set", "setw", "set-window-option":
			// setw/set-window-option alias set-option (gtmux has no separate
			// per-window option store — ponytail: window-scoped options fold into
			// the session's). Leading flags (-g/-w/-s/-a/-q/-u, -t target) are
			// stripped; option names accept tmux's hyphens or our underscores.
			// Server-owned options are stored live (next select-layout uses them,
			// no auto-relayout — like tmux). Anything else is a client option
			// arriving via scripting (`gtmux run … set-option`): push it to every
			// attached client to apply to its own config.
			// Window-scoped set: `setw`/`set-window-option`, or set-option with -w,
			// and no -g, targets one window's override store (the -t window, or the
			// active one) rather than the session default. Only the naming options
			// are window-scoped in gtmux today.
			global, windowScoped, unset := false, cmd == "setw" || cmd == "set-window-option", false
			for _, f := range args {
				switch f {
				case "-g":
					global = true
				case "-w":
					windowScoped = true
				case "-u":
					unset = true
				}
			}
			perWindow := windowScoped && !global
			targetW := activeWindow()
			if perWindow {
				if tw, _, _, _, targeted := resolveTarget(args); targeted {
					targetW = tw
				}
			}

			a := stripOptFlags(args)
			if unset && len(a) >= 1 {
				// set-option -u removes an override so it falls back to the default:
				// a @foo user option, or a per-window override (setw -u / set -wu).
				// ponytail: unsetting a session-scoped scalar back to its config
				// default isn't wired (each is a plain Go var); add per-option if it
				// ever matters — the map-backed overrides are the real -u use.
				if strings.HasPrefix(a[0], "@") {
					delete(userOpts, a[0])
				} else if perWindow {
					delete(targetW.opts, strings.ReplaceAll(a[0], "-", "_"))
				}
				break
			}
			if len(a) >= 2 {
				// Normalize the option name for the switch: @foo user options and
				// command-alias[N] both have dynamic names, so map them to fixed keys
				// and read the original a[0]/value inside their cases.
				key := strings.ReplaceAll(a[0], "-", "_")
				switch {
				case strings.HasPrefix(a[0], "@"):
					key = "@"
				case strings.HasPrefix(a[0], "command-alias"):
					key = "command_alias"
				}
				switch key {
				case "@":
					// User option: store the whole value, readable in formats as #{@foo}.
					userOpts[a[0]] = strings.Join(a[1:], " ")
				case "command_alias":
					// "name=expansion" → alias resolved at dispatch.
					if def := strings.Join(a[1:], " "); strings.Contains(def, "=") {
						n, exp, _ := strings.Cut(def, "=")
						cmdAlias[strings.TrimSpace(n)] = strings.TrimSpace(exp)
					}
				case "main_pane_width":
					if n, err := strconv.Atoi(a[1]); err == nil && n > 0 {
						mainPaneW = n
					}
				case "main_pane_height":
					if n, err := strconv.Atoi(a[1]); err == nil && n > 0 {
						mainPaneH = n
					}
				case "window_size":
					if v := a[1]; v == "latest" || v == "smallest" || v == "largest" || v == "manual" {
						windowSize = v
						if revoteWindows() { // re-vote: grid unchanged, policy did
							send(fullSync())
						}
					}
				case "aggressive_resize":
					aggressiveResize = onOff(a[1])
					if revoteWindows() {
						send(fullSync())
					}
				case "base_index":
					if n, err := strconv.Atoi(a[1]); err == nil && n >= 0 {
						baseIndex = n
						send(&proto.ServerMsg{}) // re-render the status window list
					}
				case "pane_base_index":
					if n, err := strconv.Atoi(a[1]); err == nil && n >= 0 {
						paneBaseIndex = n
					}
				case "display_time":
					if n, err := strconv.Atoi(a[1]); err == nil && n > 0 {
						displayTime = n
					}
				case "message_limit":
					if n, err := strconv.Atoi(a[1]); err == nil && n >= 0 {
						messageLimit = n
					}
				case "history_limit":
					// Config-time only (see pane.go): the package var is written once
					// at startup and read lock-free, so a runtime set would race with
					// other sessions' pane spawns. Ignore it here; show-options still
					// reports the startup value.
				case "automatic_rename":
					if perWindow {
						targetW.setOpt("automatic_rename", a[1])
					} else {
						autoRename = onOff(a[1])
					}
				case "automatic_rename_format":
					if perWindow {
						targetW.setOpt("automatic_rename_format", a[1])
					} else {
						autoRenameFmt = a[1]
					}
				case "allow_rename":
					if perWindow {
						targetW.setOpt("allow_rename", a[1])
					} else {
						allowRename = onOff(a[1])
					}
				case "destroy_unattached":
					destroyUnattached = onOff(a[1])
				case "detach_on_destroy":
					detachOnDestroy = onOff(a[1])
				case "monitor_activity":
					if perWindow {
						targetW.setOpt("monitor_activity", a[1])
					} else {
						monitorActivity = onOff(a[1])
					}
				case "monitor_bell":
					if perWindow {
						targetW.setOpt("monitor_bell", a[1])
					} else {
						monitorBell = onOff(a[1])
					}
				case "synchronize_panes":
					if perWindow {
						targetW.setOpt("synchronize_panes", a[1])
					} else {
						synchronizePanes = onOff(a[1])
					}
				case "remain_on_exit":
					// on/off/failed — a string, not a plain bool. An unrecognized
					// value reads as off at exit time (tmux rejects it; ponytail).
					if perWindow {
						targetW.setOpt("remain_on_exit", a[1])
					} else {
						remainOnExit = a[1]
					}
				case "copy_command":
					copyCommand = a[1]
				case "focus_events":
					focusEvents = onOff(a[1])
				case "allow_passthrough":
					allowPassthrough = onOff(a[1])
				case "bell_action":
					bellAction = a[1]
				case "activity_action":
					activityAction = a[1]
				case "exit_empty":
					// Server-global (not per-session): update the registry.
					reg.setExitEmpty(onOff(a[1]))
				case "update_environment":
					updateEnv = strings.Fields(a[1])
				case "monitor_silence":
					if perWindow {
						targetW.setOpt("monitor_silence", a[1])
					} else if n, err := strconv.Atoi(a[1]); err == nil && n >= 0 {
						monitorSilence = n
					}
				case "visual_activity":
					visualActivity = onOff(a[1])
				case "visual_bell":
					visualBell = onOff(a[1])
				case "pane_border_status":
					if v := a[1]; v == "off" || v == "top" || v == "bottom" {
						paneBorderStatus = v
						// Re-reserve/-release the label row on every window.
						for _, wl := range windows {
							w := wl.actor
							actorDo(w, func() { w.borderStatus = v; w.reflow() })
						}
						send(fullSync())
					}
				case "pane_border_format":
					paneBorderFormat = a[1]
					for _, wl := range windows {
						wl.actor.borderFormat = a[1]
					}
					send(fullSync())
				default:
					// Client option: normalize tmux's hyphens to our underscores
					// (client applyOption switches on underscored names), record the
					// latest value (replayed to a late-attaching client), and push
					// to currently-attached ones.
					name := strings.ReplaceAll(a[0], "-", "_")
					reg.setClientOpt(name, a[1])
					send(&proto.ServerMsg{SetOption: &proto.SetOption{Name: name, Value: a[1]}})
				}
			}
		case "show-options", "show", "show-window-options", "showw":
			// Print the server-side options gtmux holds (tmux-hyphenated names).
			// -v → value only; a trailing name filters to one option. ponytail:
			// client options (status_*, prefix, mode_keys) aren't server-visible
			// (see HISTORY.md, runtime-options), so they're not listed here.
			// Pull -t out first (so its value isn't misread as the name filter);
			// show-window-options reflects that window's effective naming options.
			tw, _, _, rest, targeted := resolveTarget(args)
			valueOnly := false
			only := ""
			for _, x := range rest {
				switch {
				case x == "-v":
					valueOnly = true
				case strings.HasPrefix(x, "-"):
				default:
					only = strings.ReplaceAll(x, "_", "-")
				}
			}
			// Naming options resolve per window for show-window-options (the -t
			// window, else the active one); show-options shows the session default.
			arv, alv, afv := offOn(autoRename), offOn(allowRename), autoRenameFmt
			if cmd == "showw" || cmd == "show-window-options" {
				sw := activeWindow()
				if targeted {
					sw = tw
				}
				if v, ok := sw.opts["automatic_rename"]; ok {
					arv = offOn(onOff(v))
				}
				if v, ok := sw.opts["allow_rename"]; ok {
					alv = offOn(onOff(v))
				}
				if v, ok := sw.opts["automatic_rename_format"]; ok {
					afv = v
				}
			}
			all := [][2]string{
				{"main-pane-width", strconv.Itoa(mainPaneW)},
				{"main-pane-height", strconv.Itoa(mainPaneH)},
				{"window-size", windowSize},
				{"synchronize-panes", offOn(synchronizePanes)},
				{"remain-on-exit", remainOnExit},
				{"copy-command", copyCommand},
				{"update-environment", strings.Join(updateEnv, " ")},
				{"focus-events", offOn(focusEvents)},
				{"allow-passthrough", offOn(allowPassthrough)},
				{"bell-action", bellAction},
				{"activity-action", activityAction},
				{"exit-empty", offOn(reg.exitEmptyOn())},
				{"default-shell", defaultShell},
				{"default-command", defaultCommand},
				{"base-index", strconv.Itoa(baseIndex)},
				{"pane-base-index", strconv.Itoa(paneBaseIndex)},
				{"history-limit", strconv.Itoa(historyLimit)},
				{"display-time", strconv.Itoa(displayTime)},
				{"message-limit", strconv.Itoa(messageLimit)},
				{"automatic-rename", arv},
				{"automatic-rename-format", afv},
				{"allow-rename", alv},
				{"destroy-unattached", offOn(destroyUnattached)},
				{"detach-on-destroy", offOn(detachOnDestroy)},
				{"monitor-activity", offOn(monitorActivity)},
				{"monitor-bell", offOn(monitorBell)},
				{"monitor-silence", strconv.Itoa(monitorSilence)},
				{"visual-activity", offOn(visualActivity)},
				{"visual-bell", offOn(visualBell)},
				{"pane-border-status", paneBorderStatus},
				{"pane-border-format", paneBorderFormat},
			}
			var lines []string
			for _, kv := range all {
				if only != "" && kv[0] != only {
					continue
				}
				if valueOnly {
					lines = append(lines, kv[1])
				} else {
					lines = append(lines, kv[0]+" "+kv[1])
				}
			}
			cmdOut = strings.Join(lines, "\n")
		case "rotate-window":
			dir := "-U"
			if len(args) > 0 {
				dir = args[0]
			}
			w := activeWindow()
			actorDo(w, func() { w.rotateWindow(dir) })
			send(fullSync())
		case "switch-session":
			// Sent by the choose-session/choose-tree pickers; hand this client off
			// to the named session (the client reconnects on the SwitchSession
			// message). An optional window index (choose-tree window row) focuses
			// that window in the target before the handoff.
			if len(fields) > 1 {
				idx := -1
				if len(fields) > 2 {
					if n, err := strconv.Atoi(fields[2]); err == nil {
						idx = n
					}
				}
				switchToSession(fields[1], idx)
			}
		case "bind-key", "bind":
			// Runtime bind: record for list-keys, then push to this session's
			// clients to apply (binds live client-side). -r/-T are consumed but
			// not modeled for runtime binds. Config binds stay Lua-only.
			root, key, cmd := parseBind(args)
			if key == "" || len(cmd) == 0 {
				return "bind-key: usage: bind-key [-n] key command"
			}
			tbl, id := "prefix", "p"+key
			if root {
				tbl, id = "root", "n"+key
			}
			reg.recordBind(id, fmt.Sprintf("bind-key -T %s %s %s", tbl, key, strings.Join(cmd, " ")))
			send(&proto.ServerMsg{ClientAction: append([]string{"bind-key"}, args...)})
		case "unbind-key", "unbind":
			root, key, _ := parseBind(args)
			if key == "" {
				return "unbind-key: usage: unbind-key [-n] key"
			}
			id := "p" + key
			if root {
				id = "n" + key
			}
			reg.removeBind(id)
			send(&proto.ServerMsg{ClientAction: append([]string{"unbind-key"}, args...)})
		case "list-keys", "lsk":
			// Only runtime binds are introspectable — config binds are opaque Lua
			// closures in each client's VM.
			cmdOut = strings.Join(reg.listBinds(), "\n")
		case "switch-client":
			// switch-client [-t name | -n | -p | -l]: retarget the acting client to
			// another session — -t by name, -n/-p cycle the sorted list, -l the last
			// switched-from. Reuses the choose-session handoff.
			target := ""
			for i := 0; i < len(args); i++ {
				switch args[i] {
				case "-t":
					if i+1 < len(args) {
						target = args[i+1]
						i++
					}
				case "-n":
					target = adjacentSession(s.name, 1)
				case "-p":
					target = adjacentSession(s.name, -1)
				case "-l":
					target = reg.getLastSession()
				}
			}
			switchToSession(target, -1)
		case "kill-pane":
			tw, tp, twi, _, _ := resolveTarget(args)
			closePaneAt(tw, tp, twi)
			if !done {
				fireHook("after-kill-pane")
			}
		case "kill-window":
			// kill-window destroys the window everywhere it's linked (unlike
			// unlink-window). Honors -t; defaults to the active window.
			_, _, twi, _, _ := resolveTarget(args)
			destroyWindow(windows[twi])
			if !done {
				fireHook("after-kill-window")
			}
		case "link-window":
			// link-window -s <session>:<window> (or a bare <session>:<window>):
			// display that window here too. Both sessions share the live window.
			spec := ""
			for i := 1; i < len(fields); i++ {
				if fields[i] == "-s" && i+1 < len(fields) {
					spec = fields[i+1]
					i++
				} else if !strings.HasPrefix(fields[i], "-") {
					spec = fields[i]
				}
			}
			if spec == "" {
				cmdOut = "link-window: usage: link-window -s <session>:<window>"
			} else {
				cmdOut = linkWindow(spec)
			}
		case "unlink-window":
			unlinkWindow()
		case "kill-session":
			// Ends this session after the current event finishes (the loop
			// checks done). Runs on the session's own goroutine, so it can't
			// block on kill() like the CLI path — it just flips the flags.
			killed = true
			done = true
		case "select-pane":
			prevFocus := windows[active].actor.active
			tw, tp, twi, rest, targeted := resolveTarget(args)
			hasDir := len(rest) > 0 && flagDir[rest[0]] != ""
			if targeted {
				active = twi
				if tp != tw.active {
					tw.lastActive = tw.active
					tw.setActive(tp)
				}
			}
			if hasDir {
				navigate(flagDir[rest[0]])
			}
			if targeted {
				// A target may switch windows; navigate only sends Layout (no
				// cells), so fullSync to redraw the target window's content.
				send(fullSync())
			}
			refocus(prevFocus)
			fireHook("after-select-pane")
		case "display-message":
			// display-message [-p] [-t target] <format> -> expand and return.
			tw, tp, twi, rest, _ := resolveTarget(args)
			var fmtStr string
			for _, a := range rest {
				if a != "-p" {
					fmtStr = a
				}
			}
			cmdOut = format.ExpandLoop(fmtStr, paneVars(tw, tp, twi), loopVars)
		case "show-messages", "showmsgs", "server-messages":
			// Dump the message log, newest first like tmux. -J/-T (job/terminal
			// listings) not modeled — this session's status messages only.
			lines := make([]string, len(msgLog))
			for i, m := range msgLog {
				lines[len(msgLog)-1-i] = m
			}
			cmdOut = strings.Join(lines, "\n")
		case "list-panes":
			// list-panes [-t target] [-F format] -> one line per pane.
			tw, _, twi, rest, _ := resolveTarget(args)
			f := "#{pane_index}: [#{pane_width}x#{pane_height}] #{pane_id}#{?pane_active, (active),} #{pane_command}"
			for i := 0; i < len(rest); i++ {
				if rest[i] == "-F" && i+1 < len(rest) {
					f = rest[i+1]
					i++
				}
			}
			var lines []string
			for _, p := range tw.panes {
				lines = append(lines, format.ExpandLoop(f, paneVars(tw, p, twi), loopVars))
			}
			cmdOut = strings.Join(lines, "\n")
		case "list-windows":
			// list-windows [-t target] [-F format] -> one line per window in
			// this session. Same shape as list-panes.
			f := "#{window_index}: #{window_name}#{?window_active,*,} (#{window_panes} panes)"
			for i := 0; i < len(args); i++ {
				if args[i] == "-F" && i+1 < len(args) {
					f = args[i+1]
					i++
				}
			}
			var lines []string
			for i, wl := range windows {
				lines = append(lines, format.ExpandLoop(f, windowVars(wl, i), loopVars))
			}
			cmdOut = strings.Join(lines, "\n")
		case "list-sessions":
			// list-sessions [-F format] -> one line per live session. Runs on
			// this session's goroutine: use local state for ourselves, info()
			// for the others (info() on ourselves would deadlock — it round-
			// trips through this same event channel).
			f := "#{session_name}: #{session_windows} windows#{?session_attached, (attached),}"
			for i := 0; i < len(args); i++ {
				if args[i] == "-F" && i+1 < len(args) {
					f = args[i+1]
					i++
				}
			}
			var lines []string
			for _, n := range reg.names() {
				var si proto.SessionInfo
				if n == s.name {
					si = proto.SessionInfo{Name: n, Windows: len(windows), Attached: len(attachments) > 0}
				} else if other, ok := reg.get(n); ok {
					si = other.info()
				} else {
					continue
				}
				lines = append(lines, format.ExpandLoop(f, map[string]string{
					"session_name":     si.Name,
					"session_windows":  strconv.Itoa(si.Windows),
					"session_attached": bitStr(si.Attached),
				}, loopVars))
			}
			cmdOut = strings.Join(lines, "\n")
		case "pipe-pane":
			// pipe-pane [-t target] [command]: tee the pane's output to command's
			// stdin. No command (or a running pipe) toggles it off. ponytail: -o
			// (only-open) ignored — we always toggle/replace.
			_, tp, _, rest, _ := resolveTarget(args)
			if tp == nil {
				return "pipe-pane: no such pane"
			}
			var words []string
			for _, a := range rest {
				if a != "-o" {
					words = append(words, a)
				}
			}
			// tp.pipeW is also written by applyOutput on the actor, so its
			// reads/writes go through the actor.
			actorDo(tp.win.actor, func() {
				if tp.pipeW != nil { // stop any existing pipe first
					tp.pipeW.Close()
					tp.pipeW = nil
				}
			})
			if cmdStr := strings.Join(words, " "); cmdStr != "" {
				pc := exec.Command("sh", "-c", cmdStr)
				w, err := pc.StdinPipe()
				if err != nil {
					return "pipe-pane: " + err.Error()
				}
				if err := pc.Start(); err != nil {
					return "pipe-pane: " + err.Error()
				}
				actorDo(tp.win.actor, func() { tp.pipeW = w })
				go pc.Wait() // reap when it exits
			}
		case "set-environment", "setenv":
			// set-environment [-g] [-u] name [value]: -g targets the cross-session
			// global env, -u unsets. Applies to panes spawned afterward (tmux's
			// future-panes semantics).
			rest := args
			unset, global := false, false
			for len(rest) > 0 && (rest[0] == "-u" || rest[0] == "-g") {
				if rest[0] == "-u" {
					unset = true
				} else {
					global = true
				}
				rest = rest[1:]
			}
			if len(rest) == 0 {
				return "set-environment: no name"
			}
			switch {
			case global:
				reg.setGlobalEnv(rest[0], strings.Join(rest[1:], " "), unset)
			case unset:
				delete(sessionEnv, rest[0])
			default:
				sessionEnv[rest[0]] = strings.Join(rest[1:], " ")
			}
		case "show-environment", "showenv":
			// show-environment [-g]: -g shows the global env, else the session's.
			env := sessionEnv
			if len(args) > 0 && args[0] == "-g" {
				env = reg.globalEnvCopy()
			}
			var lines []string
			for k, v := range env {
				lines = append(lines, k+"="+v)
			}
			sort.Strings(lines)
			cmdOut = strings.Join(lines, "\n")
		case "respawn-pane":
			// respawn-pane [-k] [-t target] [command]: kill the pane's process and
			// start command (shell if none) in the same slot.
			_, tp, _, rest, _ := resolveTarget(args)
			if tp == nil {
				return "respawn-pane: no such pane"
			}
			var rerr error
			actorDo(tp.win.actor, func() { rerr = tp.respawn(s.name, respawnCommand(rest)) })
			if rerr != nil {
				return "respawn-pane: " + rerr.Error()
			}
			watchPane(tp)
			send(fullSync())
		case "respawn-window":
			// respawn-window [-k] [-t target] [command]: respawn every pane in the
			// target window.
			tw, _, _, rest, _ := resolveTarget(args)
			if tw == nil {
				return "respawn-window: no such window"
			}
			cmd := respawnCommand(rest)
			for _, p := range tw.panes {
				var rerr error
				actorDo(tw.actor, func() { rerr = p.respawn(s.name, cmd) })
				if rerr == nil {
					watchPane(p)
				}
			}
			send(fullSync())
		case "find-window":
			// Select the first window whose name contains the pattern. tmux also
			// searches pane titles/content; name-only here (note).
			if len(args) == 0 {
				return "find-window: no pattern"
			}
			for i, wl := range windows {
				if strings.Contains(windowName(wl.actor), args[0]) {
					switchToWindow(i)
					break
				}
			}
		case "list-clients":
			// All clients across every session (tmux's default).
			// ponytail: no -t session filter yet.
			infos := gatherClients()
			var lines []string
			for _, ci := range infos {
				lines = append(lines, fmt.Sprintf("client-%d: %s [%dx%d]", ci.epoch, ci.session, ci.cols, ci.rows))
			}
			cmdOut = strings.Join(lines, "\n")
		case "refresh-client":
			// Force a full resync to the acting client (tmux refresh-client) —
			// recompute the grid first in case a resize was missed.
			if applySize() {
				send(fullSync())
			} else {
				sendTo(actingEpoch, fullSync())
			}
		case "resize-window":
			// resize-window [-x W] [-y H] [-U/-D/-L/-R [N]]: set/adjust the grid.
			// Only sticks under window-size "manual" (otherwise the next client
			// event recomputes it). -x/-y set absolute; -U/-D/-L/-R nudge by N (1).
			nc, nr := cols, rows
			for i := 0; i < len(args); i++ {
				a := args[i]
				num := func(def int) int {
					if i+1 < len(args) {
						if n, err := strconv.Atoi(args[i+1]); err == nil {
							i++
							return n
						}
					}
					return def
				}
				switch a {
				case "-x":
					nc = num(nc)
				case "-y":
					nr = num(nr)
				case "-L":
					nc -= num(1)
				case "-R":
					nc += num(1)
				case "-U":
					nr -= num(1)
				case "-D":
					nr += num(1)
				}
			}
			nc, nr = max(1, nc), max(1, nr)
			if nc != cols || nr != rows {
				cols, rows = nc, nr
				for _, wl := range windows {
					w := wl.actor
					actorDo(w, func() { w.resize(cols, winRows()) })
					winGrid[w] = [2]int{cols, winRows()}
				}
				send(fullSync())
			}
		case "clear-history":
			// clear-history [-t target]: drop the target pane's scrollback (the
			// visible screen stays). term.ClearHistory locks the emu itself, so
			// this is safe alongside the PTY reader without an actor handshake.
			_, tp, _, _, _ := resolveTarget(args)
			tp.term.ClearHistory()
		case "capture-pane":
			// capture-pane [-p] [-t target] [-S start] [-E end] -> pane text.
			// -p (print) is implicit: we always return via Ack.Out. -S selects
			// how much scrollback to prepend: "-" = all history, "-N" = last N
			// lines. Default is the visible screen only. -E clamps the last row.
			// ponytail: trailing spaces trimmed per line, like tmux's default.
			_, tp, _, rest, _ := resolveTarget(args)
			hist := tp.term.History()
			start, end := 0, len(tp.term.Screen())
			keepEsc, joinWrap := false, false // -e keep SGR escapes; -J join wrapped lines
			for i := 0; i < len(rest); i++ {
				switch rest[i] {
				case "-e":
					keepEsc = true
				case "-J":
					joinWrap = true
				case "-S":
					if i+1 < len(rest) {
						if rest[i+1] == "-" {
							start = -len(hist)
						} else if n, err := strconv.Atoi(rest[i+1]); err == nil {
							start = n
						}
						i++
					}
				case "-E":
					if i+1 < len(rest) {
						if n, err := strconv.Atoi(rest[i+1]); err == nil {
							end = n + 1
						}
						i++
					}
				}
			}
			screen := tp.term.Screen()
			var lines []string
			var wrapped []bool
			for row := start; row < end; row++ {
				var l emu.Line
				if row < 0 {
					if h := len(hist) + row; h >= 0 {
						l = hist[h]
					}
				} else if row < len(screen) {
					l = screen[row]
				}
				if keepEsc {
					lines = append(lines, emu.RenderLine(l))
				} else {
					lines = append(lines, strings.TrimRight(l.String(), " "))
				}
				// -J: a line that wrapped at its right edge (last cell flagged)
				// continues onto the next row, so it's joined rather than split.
				wrapped = append(wrapped, len(l) > 0 && l[len(l)-1].Mode&emu.AttrWrap != 0)
			}
			if joinWrap {
				var merged []string
				cur := ""
				for i, ln := range lines {
					cur += ln
					if wrapped[i] {
						continue
					}
					merged = append(merged, cur)
					cur = ""
				}
				if cur != "" {
					merged = append(merged, cur)
				}
				lines = merged
			}
			cmdOut = strings.Join(lines, "\n")
		case "select-pane-vim":
			// tmux's vim-split nav resolved server-side (live /proc read): if
			// the active pane runs vim, deliver the ctrl key to it; otherwise
			// move to the adjacent pane (or the last pane for -l).
			// ponytail: vim match is a substring, same as the config's grep -iq vim.
			if len(args) == 0 {
				return ""
			}
			flag := args[0]
			if p := activeWindow().active; strings.Contains(p.currentCommand(), "vim") {
				if k := vimNavKey[flag]; k != 0 {
					p.pty.Write([]byte{k})
				}
			} else if flag == "-l" {
				selectLastPane()
			} else if d := flagDir[flag]; d != "" {
				navigate(d)
			}
		case "resize-pane", "resizep":
			if len(args) > 0 && args[0] == "-Z" {
				toggleZoom()
				return ""
			}
			// -x/-y <N|N%>: absolute pane width/height (can combine). -t is ignored
			// (acts on the active pane); select-pane first to target another.
			resized := false
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "-x" || args[i] == "-y" {
					width, spec := args[i] == "-x", args[i+1]
					w := activeWindow()
					actorDo(w, func() { w.resizePaneTo(width, spec) })
					resized = true
					i++
				}
			}
			if resized {
				send(fullSync())
				fireHook("after-resize-pane")
				return ""
			}
			if len(args) > 0 && flagDir[args[0]] != "" {
				n := 1
				if len(args) > 1 {
					if v, err := strconv.Atoi(args[1]); err == nil && v > 0 {
						n = v
					}
				}
				resizePane(flagDir[args[0]], n)
				fireHook("after-resize-pane")
			}
		case "swap-pane":
			dir := "next"
			if len(args) > 0 && args[0] == "-U" {
				dir = "prev"
			}
			swapPane(dir)
		case "move-window", "swap-window":
			dir := "next"
			if len(args) > 0 && (args[0] == "-L" || args[0] == "left") {
				dir = "prev"
			}
			swapWindow(dir)
		case "zoom":
			toggleZoom()
		case "break-pane":
			breakPane()
		case "display-panes":
			showPaneNumbers()
		case "mark-pane":
			markPane()
		case "join-marked":
			joinMarked()
		case "join-pane", "move-pane":
			joinPaneCmd(args)
		case "choose-session":
			chooseSession()
		case "choose-tree":
			filter := ""
			for i := 0; i < len(args); i++ {
				if args[i] == "-f" && i+1 < len(args) {
					filter = args[i+1]
					i++
				}
			}
			chooseTree(filter)
		case "choose-client":
			chooseClient()
		case "choose-buffer":
			chooseBuffer()
		case "copy-mode":
			enterCopyModeAction(len(args) > 0 && args[0] == "-u")
		case "paste", "paste-buffer":
			name, _ := bufFlag(args)
			if b, ok := findBuffer(name); ok {
				// Wrap in bracketed-paste markers when the app asked for them,
				// so embedded newlines insert instead of executing each line.
				// An app that didn't must never see the markers — it would
				// print them literally.
				p := activeWindow().active
				if p.wantsPaste() {
					p.pty.Write([]byte("\x1b[200~" + b.data + "\x1b[201~"))
				} else {
					p.pty.Write([]byte(b.data))
				}
			}
		case "set-buffer":
			// set-buffer [-a] [-b name] data — -a appends to the named (or newest)
			// buffer; data is the rest, joined.
			appendBuf := false
			if len(args) > 0 && args[0] == "-a" {
				appendBuf, args = true, args[1:]
			}
			name, rest := bufFlag(args)
			data := strings.Join(rest, " ")
			if appendBuf {
				if b, ok := findBuffer(name); ok {
					name, data = b.name, b.data+data
				}
			}
			addBuffer(name, data)
		case "delete-buffer":
			name, _ := bufFlag(args)
			for i, b := range buffers {
				if name == "" || b.name == name {
					buffers = append(buffers[:i], buffers[i+1:]...)
					break
				}
			}
		case "list-buffers":
			f := "#{buffer_name}: #{buffer_size} bytes: #{buffer_sample}"
			for i := 0; i < len(args); i++ {
				if args[i] == "-F" && i+1 < len(args) {
					f = args[i+1]
					i++
				}
			}
			var lines []string
			for _, b := range buffers {
				lines = append(lines, format.ExpandLoop(f, bufferVars(b), loopVars))
			}
			cmdOut = strings.Join(lines, "\n")
		case "show-buffer":
			name, _ := bufFlag(args)
			if b, ok := findBuffer(name); ok {
				cmdOut = b.data
			}
		case "save-buffer":
			// save-buffer [-b name] path
			name, rest := bufFlag(args)
			if len(rest) == 0 {
				return "save-buffer: no path"
			}
			b, ok := findBuffer(name)
			if !ok {
				return "save-buffer: no such buffer"
			}
			if err := os.WriteFile(rest[0], []byte(b.data), 0644); err != nil {
				return "save-buffer: " + err.Error()
			}
		case "load-buffer":
			// load-buffer [-b name] path — read path into a buffer (named or new).
			name, rest := bufFlag(args)
			if len(rest) == 0 {
				return "load-buffer: no path"
			}
			data, err := os.ReadFile(rest[0])
			if err != nil {
				return "load-buffer: " + err.Error()
			}
			addBuffer(name, string(data))
		case "detach":
			detach()
		case "detach-client":
			// detach-client [-t client-<epoch>@<session>]: no -t detaches the
			// acting client; -t detaches a specific one (routed to its session's
			// goroutine if it's a peer).
			target := ""
			for i := 0; i < len(args); i++ {
				if args[i] == "-t" && i+1 < len(args) {
					target = args[i+1]
					i++
				}
			}
			if target == "" {
				detach()
			} else if at := strings.IndexByte(target, '@'); at > 0 && strings.HasPrefix(target, "client-") {
				ep, err := strconv.Atoi(target[len("client-") : at])
				sess := target[at+1:]
				if err != nil {
					return "detach-client: bad target " + target
				}
				if sess == s.name {
					if attachments[ep] == nil {
						return "can't find client " + target
					}
					detachEpoch(ep)
				} else if peer, ok := reg.get(sess); ok {
					// Route to the peer's goroutine; propagate its "can't find
					// client" if that epoch isn't attached there either.
					_, errText := peer.command([]string{"detach-client", "-t", target})
					return errText
				} else {
					return "can't find client " + target
				}
			} else {
				return "detach-client: bad target " + target
			}
		case "rename-window":
			if rest != "" {
				activeWindow().rename(rest)
				send(&proto.ServerMsg{})
				fireHook("after-rename-window")
			}
		case "rename-session":
			if rest != "" && rest != s.name {
				if err := reg.relabel(s.name, rest); err != nil {
					return err.Error()
				}
				s.name = rest
				send(&proto.ServerMsg{})
			}
		case "send-keys":
			// -l sends args as literal text (no key-name lookup); -H treats each
			// arg as a hex byte value; -t <target> is accepted and ignored (acts on
			// the active pane). ponytail: -N count / -R reset deferred.
			literal, hexMode := false, false
			payload := args
			for len(payload) > 0 && strings.HasPrefix(payload[0], "-") && payload[0] != "-" {
				switch {
				case payload[0] == "-l":
					literal, payload = true, payload[1:]
				case payload[0] == "-H":
					hexMode, payload = true, payload[1:]
				case payload[0] == "-t" && len(payload) > 1:
					payload = payload[2:]
				default:
					payload = payload[1:]
				}
			}
			var out []byte
			for _, a := range payload {
				switch {
				case hexMode:
					if n, err := strconv.ParseInt(a, 16, 16); err == nil {
						out = append(out, byte(n))
					}
				case literal:
					out = append(out, []byte(a)...)
				default:
					out = append(out, keyBytes(a)...)
				}
			}
			if len(out) > 0 {
				activeWindow().active.pty.Write(out)
			}
		case "if-shell":
			// if-shell shell-cmd then-cmd [else-cmd]: run shell-cmd, then run
			// then-cmd (exit 0) or else-cmd. The shell runs in a goroutine so the
			// session owner goroutine never blocks on it; the chosen command is
			// posted back and run on the owner goroutine (ifShellResult).
			// Encoded by gtmux.if_shell with each part as one arg — typed/scripted
			// use with multi-word parts needs a quote parser (deferred), same as
			// the command-prompt template.
			if len(args) < 2 {
				return "if-shell: usage: if-shell shell-cmd then-cmd [else-cmd]"
			}
			shellCmd, thenCmd := args[0], args[1]
			elseCmd := ""
			if len(args) >= 3 {
				elseCmd = args[2]
			}
			go func() {
				chosen := thenCmd
				if exec.Command("sh", "-c", shellCmd).Run() != nil {
					chosen = elseCmd
				}
				if fields := strings.Fields(chosen); len(fields) > 0 {
					s.events <- ifShellResult{cmd: fields}
				}
			}()
		case "display-popup":
			// display-popup [-w W] [-h H] [-x X] [-y Y] [-d dir] [--] [command]:
			// a floating terminal running command (shell if none). W/H accept N or
			// N%; X/Y accept N, N%, or C (center, the default). Closes when the
			// command exits (tmux's -E, our only mode). ponytail: styling flags
			// (-B/-b/-s/-S/-T) accepted-and-ignored.
			if popups[actingEpoch] != nil {
				return "" // one popup at a time per client
			}
			pw, ph := cols*3/4, winRows()*3/4
			px, py := -1, -1 // -1 = center that axis
			dir := activeWindow().active.cwd()
			command := ""
			for i := 0; i < len(args); i++ {
				switch a := args[i]; a {
				case "-w", "-h", "-x", "-y", "-d", "-b", "-s", "-S", "-T", "-c", "-t":
					if i+1 >= len(args) {
						break
					}
					i++
					switch a {
					case "-w":
						pw = popupDim(args[i], cols)
					case "-h":
						ph = popupDim(args[i], winRows())
					case "-x":
						px = popupPos(args[i], cols)
					case "-y":
						py = popupPos(args[i], winRows())
					case "-d":
						dir = args[i]
					}
					// -b/-s/-S/-T/-c/-t take an arg in tmux (border/style/title/
					// client/target) — consumed and ignored, no styling in the POC.
				case "-E", "-B", "-C": // no-arg flags: close-on-exit / no-border
				case "--":
					command = strings.Join(args[i+1:], " ")
					i = len(args)
				default:
					command = strings.Join(args[i:], " ")
					i = len(args)
				}
			}
			pr := rect{Rows: max(3, min(ph, winRows())), Cols: max(20, min(pw, cols))}
			p, err := spawnPane(nil, s.name, pr, dir, command)
			if err != nil {
				return "display-popup: " + err.Error()
			}
			popups[actingEpoch] = p
			watchPane(p)
			content := p.fullContent()
			// Only the opening client sees/drives its popup.
			sendTo(actingEpoch, &proto.ServerMsg{Popup: &proto.PopupMsg{Open: true, Cols: pr.Cols, Rows: pr.Rows, X: px, Y: py, Content: &content}})
		case "run-shell":
			if rest == "" {
				return "run-shell: no command"
			}
			go func() {
				out, err := exec.Command("sh", "-c", rest).CombinedOutput()
				text := strings.TrimSpace(string(out))
				if nl := strings.IndexByte(text, '\n'); nl >= 0 {
					text = text[:nl]
				}
				if err != nil && text == "" {
					text = err.Error()
				}
				s.events <- messageEvent{text: text}
			}()
		case "set-hook":
			// set-hook [-a] [-u] <name> <cmd>: -u unsets, -a appends, else
			// replaces (tmux default). Mutates this session's hook copy.
			rest := args
			appendMode, unset := false, false
			for len(rest) > 0 && (rest[0] == "-a" || rest[0] == "-u") {
				if rest[0] == "-a" {
					appendMode = true
				} else {
					unset = true
				}
				rest = rest[1:]
			}
			if len(rest) == 0 {
				return "set-hook: no hook name"
			}
			name := rest[0]
			switch {
			case unset:
				delete(hooks, name)
			case appendMode:
				hooks[name] = append(hooks[name], strings.Join(rest[1:], " "))
			default:
				hooks[name] = []string{strings.Join(rest[1:], " ")}
			}
		default:
			return "unknown command: " + cmd
		}
		return ""
	}

	// fireHook runs the commands bound to a hook event, on this (owner)
	// goroutine. firing guards re-entry per hook name: a hook whose command
	// re-triggers the SAME event won't loop, but it can still fire a
	// different-named hook (tmux allows that chaining).
	fireHook = func(name string) {
		if firing[name] {
			return
		}
		cmds := hooks[name]
		if len(cmds) == 0 {
			return
		}
		firing[name] = true
		defer delete(firing, name)
		for _, c := range cmds {
			fields := strings.Fields(c)
			if len(fields) == 0 {
				continue
			}
			// A hook that opens a client-owned overlay (command-prompt /
			// confirm-before / display-menu) can't run server-side — route it to
			// the attached clients to dispatch locally.
			if clientSideCmd(fields[0]) {
				for ep := range attachments {
					sendTo(ep, &proto.ServerMsg{ClientAction: fields})
				}
				continue
			}
			cmdOut = ""
			if errText := runCommand(fields); errText != "" {
				showMessage(errText)
			}
		}
	}

	// handleMouseEvent forwards a mouse event into a pane's own application.
	// The client owns all mouse gesture recognition now (focus-click, border
	// drag, wheel→copy-mode, drag-to-copy) using the Layout it already holds,
	// and only forwards an event here when the target pane's app has requested
	// mouse tracking (PaneRect.WantsMouse) — so this just translates the event
	// for that app's protocol and writes it. cb/cx/cy arrive already decoded.
	// ponytail: the client's WantsMouse view can lag a mouse-mode flip by one
	// Layout push; a mis-routed event self-heals on the next push. Only the
	// server's live emu mode is authoritative, so re-check it here too.
	handleMouseEvent := func(cb, cx, cy int, press bool) {
		w := activeWindow()
		row, col := cy-1, cx-1
		if row < 0 || row >= w.rows || col < 0 || col >= w.cols {
			return
		}
		var target *pane
		if w.zoomed {
			target = w.active
		} else {
			for _, p := range w.panes {
				if row >= p.rect.Row && row < p.rect.Row+p.rect.Rows &&
					col >= p.rect.Col && col < p.rect.Col+p.rect.Cols {
					target = p
					break
				}
			}
		}
		if target == nil || target.term.Mode()&emu.ModeMouseMask == 0 {
			return
		}
		localX, localY := col-target.rect.Col+1, row-target.rect.Row+1
		if data := translateMouseForPane(target, cb, localX, localY, press); data != nil {
			target.pty.Write(data)
		}
	}

	// handleInput passes client keystrokes straight to the focused pane. The
	// client owns all input now: it tracks the prefix and resolves binds
	// locally, so raw Input only ever carries keys meant for the pane itself.
	handleInput := func(epoch int, data []byte) {
		// A display-popup grabs its owner's input while open (its process reads
		// it), exactly like a foreground app in the pane would.
		if p := popups[epoch]; p != nil {
			p.pty.Write(p.filterPaste(data))
			return
		}
		wa := activeWindow()
		// synchronize-panes: mirror the keystrokes to every pane in the window.
		// Resolve the per-window override over the session default; snapshot the
		// pane list under the actor handshake (a concurrent split could grow it),
		// then write outside so a full pty buffer can't stall the actor.
		sync := synchronizePanes
		if v, ok := wa.opts["synchronize_panes"]; ok {
			sync = onOff(v)
		}
		if sync {
			var panes []*pane
			actorDo(wa, func() { panes = append(panes, wa.panes...) })
			for _, p := range panes {
				p.pty.Write(p.filterPaste(data)) // per pane: 2004 state can differ
			}
			return
		}
		wa.active.pty.Write(wa.active.filterPaste(data))
	}

	// Drives the status bar's clock/git-branch fields, which change on their
	// own rather than in response to any pty/client event.
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	fireHook("session-created") // the session is fully set up; run any session-created hook

loop:
	for {
		select {
		case <-ticker.C:
			refreshStatus()
			if reg.snapshotsActive() {
				reg.putSnapshot(s.name, buildSnapSession())
			}
			send(&proto.ServerMsg{})
		case rm := <-renderCh:
			handleRender(rm)
		case ev := <-s.events:
			switch e := ev.(type) {
			case attachEvent:
				nextEpoch++
				ep := nextEpoch
				attachments[ep] = &attachment{conn: e.conn, enc: e.enc, cols: e.cols, rows: e.rows, readOnly: e.readOnly, wantSnap: e.wantSnap}
				if e.wantSnap {
					reg.wantSnapshot(1)
				}
				// Fold this client's #server() commands into the union set the
				// tick runs, and adopt its cache cadence.
				serverCmds = mergeCmds(serverCmds, e.statusCmds)
				if e.statusInterval > 0 {
					shellRunner.interval = time.Duration(e.statusInterval) * time.Second
				}
				// A fresh attach becomes the acting client, so the window
				// resizes to it (tmux window-size "latest").
				actingEpoch = ep
				e.epochCh <- ep
				if applySize() {
					// Grid changed: every client needs the new arrangement.
					send(fullSync())
				} else {
					// Same grid: only the newcomer has a blank screen.
					sendTo(ep, fullSync())
				}
				// Replay runtime client-option changes so a late attacher inherits
				// them (the server doesn't otherwise hold client config).
				for name, val := range reg.clientOptsCopy() {
					sendTo(ep, &proto.ServerMsg{SetOption: &proto.SetOption{Name: name, Value: val}})
				}
				// update-environment: refresh the listed vars from this client's
				// environment into the session env (future panes see the new
				// SSH_AUTH_SOCK/DISPLAY/… on reattach — tmux's behavior). A listed
				// var absent from the client is unset from the session env.
				for _, name := range updateEnv {
					if v, ok := e.env[name]; ok {
						sessionEnv[name] = v
					} else {
						delete(sessionEnv, name)
					}
				}
				fireHook("client-attached")
			case infoEvent:
				e.replyCh <- proto.SessionInfo{Name: s.name, Windows: len(windows), Attached: len(attachments) > 0}
			case clientsEvent:
				infos := make([]clientInfo, 0, len(attachments))
				for ep, a := range attachments {
					infos = append(infos, clientInfo{session: s.name, epoch: ep, cols: a.cols, rows: a.rows})
				}
				e.replyCh <- infos
			case previewEvent:
				e.replyCh <- previewSnap(activeWindow().active.term.Screen())
			case killEvent:
				killReplyCh = e.replyCh
				killed = true
				done = true
			case renameEvent:
				s.name = e.name
				e.replyCh <- struct{}{}
				fireHook("session-renamed")
			case commandEvent:
				cmdOut = ""
				errText := runCommand(e.args)
				e.replyCh <- cmdReply{out: cmdOut, err: errText}
			case messageEvent:
				// Route through showMessage so async messages (run-shell
				// output) get the same 3s auto-clear as synchronous ones —
				// otherwise they stick on the status bar forever.
				showMessage(e.text)
			case clearMessageEvent:
				if statusMsg != "" {
					statusMsg = ""
					send(&proto.ServerMsg{})
				}
			case hideNumbersEvent:
				actorDo(e.window.actor, func() { e.window.showNumbers = false })
				if e.window == activeWindow().window {
					sendLayout(e.window.actor)
				}
			case silenceEvent:
				// The interval lapsed with no output. Alert only if this session
				// still links the window, isn't the current one, and it isn't already
				// flagged (per-view). e.view stays valid: the timer is stopped on
				// unsubscribe, so a fired event means the view is still ours.
				w := e.window
				wi := indexOfWindow(windows, w)
				if wi >= 0 && windows[wi].view == e.view && w != activeWindow().window && !e.view.silence {
					e.view.silence = true
					send(&proto.ServerMsg{})
					fireHook("alert-silence")
					if visualActivity {
						showMessage(fmt.Sprintf("silence in window %d", wi+baseIndex))
					}
				}
			case linkRequest:
				// Another session wants to link one of our windows: resolve the
				// spec against our own window list and hand back the actor (nil if
				// not found). The requester subscribes a view to it.
				var wa *windowActor
				if idx := resolveWindowSpec(e.spec); idx >= 0 {
					wa = windows[idx].actor
				}
				e.reply <- wa
			case groupJoinRequest:
				// A new group member (new-session -t us) wants our current windows.
				actors := make([]*windowActor, len(windows))
				for i, wl := range windows {
					actors[i] = wl.actor
				}
				e.reply <- actors
			case winlinkGone:
				// A window we link was destroyed elsewhere (kill-window / its last
				// pane exited): drop our winlink to it and redraw if it was current.
				if idx := indexOfWindow(windows, e.actor.window); idx >= 0 {
					removeWindowAt(idx)
					if !done {
						send(fullSync())
					}
				}
			case windowResized:
				// A window we share was resized by another viewer's client. Refresh
				// our cached size and redraw if it's our current one (a background
				// one redraws on next switch).
				var gc, gr int
				actorDo(e.actor, func() { gc, gr = e.actor.cols, e.actor.rows })
				winGrid[e.actor] = [2]int{gc, gr}
				if e.actor == activeWindow() {
					send(fullSync())
				}
			case clientGone:
				if _, ok := attachments[e.epoch]; ok {
					closeClient(e.epoch)
					// Tear down that client's popup, if any (its process dies with
					// its only viewer).
					if p := popups[e.epoch]; p != nil {
						p.Close()
						delete(popups, e.epoch)
					}
					// If the acting client left, re-point to a remaining one
					// so the grid resizes to whoever is left.
					if e.epoch == actingEpoch {
						actingEpoch = anyEpoch()
					}
					if applySize() {
						send(fullSync())
					}
					fireHook("client-detached")
					if destroyUnattached && len(attachments) == 0 {
						done = true
					}
				}
			case clientInput:
				if a := attachments[e.epoch]; a != nil && !a.readOnly {
					actingEpoch = e.epoch
					// This client is now the acting one; the window resizes to
					// it if its size differs (tmux window-size "latest").
					if applySize() {
						send(fullSync())
					}
					handleInput(e.epoch, e.data)
				}
			case clientMouse:
				if _, ok := attachments[e.epoch]; ok {
					actingEpoch = e.epoch
					if applySize() {
						send(fullSync())
					}
					handleMouseEvent(e.cb, e.x, e.y, e.press)
				}
			case resizeBorderEvent:
				// Client dragged a pane divider (it recognized the gesture from its
				// Layout). Map the border index back to its split node and set the
				// fraction from the target position, then reflow — same math the
				// server used to run inline off raw mouse motion.
				if _, ok := attachments[e.epoch]; ok {
					actingEpoch = e.epoch
					w := activeWindow()
					if e.index >= 0 && e.index < len(w.borders) {
						n := w.borders[e.index].node
						if n.dir == splitVertical {
							if usable := n.r.Cols - 1; usable > 0 {
								n.frac = float64(e.pos-n.r.Col) / float64(usable)
							}
						} else {
							if usable := n.r.Rows - 1; usable > 0 {
								n.frac = float64(e.pos-n.r.Row) / float64(usable)
							}
						}
						if n.frac < 0 {
							n.frac = 0
						}
						if n.frac > 1 {
							n.frac = 1
						}
						actorDo(w, func() { w.reflow() })
						send(fullSync())
					}
				}
			case copyDragEvent:
				// Client recognized drag-to-copy over a non-tracking pane. Only the
				// server can build the scrollback snapshot, so it does that here and
				// hands the acting client a copy-mode entry with the selection
				// anchored at the pane-local press cell (e.row/e.col).
				if _, ok := attachments[e.epoch]; ok {
					actingEpoch = e.epoch
					w := activeWindow()
					var target *pane
					for _, p := range w.panes {
						if p.id == e.paneID {
							target = p
							break
						}
					}
					if target != nil {
						var snap *proto.CopyModeEnter
						actorDo(target.win.actor, func() { snap = target.copySnapshot() })
						snap.CursorY = len(snap.Lines) - target.rect.Rows + e.row
						snap.CursorX = e.col
						snap.Select = true
						sendTo(actingEpoch, &proto.ServerMsg{CopyModeEnter: snap})
					}
				}
			case ifShellResult:
				cmdOut = ""
				if errText := runCommand(e.cmd); errText != "" {
					showMessage(errText)
				}
			case setPasteEvent:
				addBuffer("", e.text)
				if e.pipe && copyCommand != "" {
					// tmux's copy-command: pipe the selection to a shell command
					// (stdin). Fire-and-forget off the actor so a slow/blocking
					// clipboard tool can't stall the session.
					// ponytail: errors are swallowed like tmux; surface via
					// showMessage if a failing copy-command ever needs debugging.
					c := exec.Command("sh", "-c", copyCommand)
					c.Stdin = strings.NewReader(e.text)
					go c.Run()
				}
			case actionEvent:
				if _, ok := attachments[e.epoch]; ok {
					actingEpoch = e.epoch
					cmdOut = ""
					if errText := runCommand(e.args); errText != "" {
						showMessage(errText)
					}
				}
			case clientResize:
				a := attachments[e.epoch]
				if a == nil {
					continue
				}
				a.cols, a.rows = e.cols, e.rows
				if applySize() {
					send(fullSync())
				}
			case ptyOutput:
				// Drop events from a reader started before a respawn: its process
				// is dead and its pane now runs a newer one.
				if e.gen != e.pane.gen {
					continue
				}
				// Popup output/exit is handled here, before the window path — a
				// popup pane has win == nil, so it must never reach the
				// closePane/removeWindowAt teardown below. Routed only to the
				// client that owns this popup.
				if ep, ok := popupEpoch(e.pane); ok {
					if e.err != nil {
						e.pane.Close()
						delete(popups, ep)
						sendTo(ep, &proto.ServerMsg{Popup: &proto.PopupMsg{Close: true}})
					} else {
						e.pane.term.Write(e.data)
						content := e.pane.dirtyContent()
						sendTo(ep, &proto.ServerMsg{Popup: &proto.PopupMsg{Content: &content}})
					}
					continue
				}
				// The pane's window may already be torn down (kill-window /
				// removeWindowAt stopped its actor and closed its panes); this is a
				// straggler output/exit event still in the queue. The actor is
				// stopped, so the actorDo/forwardOutput below would send on a closed
				// channel — drop it. All on the session goroutine, so wa.stopped is
				// race-free.
				if e.pane.win.actor.stopped {
					if done {
						break loop
					}
					continue
				}
				if e.err != nil {
					w := e.pane.win
					// remain-on-exit: keep the pane frozen in the layout instead of
					// closing it. Resolve the per-window override over the session
					// default (both session-goroutine state, like synchronize-panes).
					roe := remainOnExit
					if v, ok := w.opts["remain_on_exit"]; ok {
						roe = v
					}
					if roe == "on" || roe == "failed" {
						code := e.pane.markDead() // reap; keeps the final screen
						if roe == "on" || code != 0 {
							// Freeze the pane: append a dead marker on the actor (the
							// sole grid mutator, after any queued output), keep it in the
							// window, leave its origin relay intact for a later respawn.
							dp := e.pane
							actorDo(w.actor, func() {
								dp.term.Write([]byte(fmt.Sprintf("\r\n[pane dead: exit %d]", code)))
							})
							send(fullSync())
							fireHook("pane-exited")
							if done {
								break loop
							}
							continue
						}
						// "failed" + clean exit: fall through and close it (markDead
						// already reaped; Close early-returns on a dead pane).
					}
					// Remove the pane on its actor first — the do runs after any
					// queued output for this actor (FIFO), so by the time we Close
					// the pane no applyOutput can still touch its pipeW/grid.
					survived := false
					actorDo(w.actor, func() { survived = w.closePane(e.pane) })
					dropRelay(e.pane) // if it had migrated, retire the origin's relay entry
					e.pane.Close()
					if !survived {
						// The window's last pane exited — it's gone for everyone, so
						// destroy it across all sessions that link it. kill-window may
						// have already removed w here; a late exit must not re-remove.
						if idx := indexOfWindow(windows, w); idx >= 0 {
							destroyWindow(windows[idx])
						}
					} else {
						// The window survived the pane's exit: safe to fire the
						// hook (the session isn't tearing down).
						if w == activeWindow().window {
							send(fullSync())
						}
						fireHook("pane-exited")
					}
					if done {
						break loop
					}
					continue
				}
				// A window pane's non-exit output never reaches s.events — its
				// reader posts straight to the window actor (see watchPane). Only
				// popup output (handled above) and pane exits arrive here.
			}
		}
		if done {
			break loop
		}
	}

	// The session is ending but its state is still intact here — run any
	// session-closed hook before the panes/clients are torn down below.
	fireHook("session-closed")

	// detach-on-destroy off: on a kill, hand attached clients to another session
	// (like switch-session) instead of detaching. Falls back to detach if there's
	// no other session. reg.remove hasn't run yet, so names() still lists self.
	switchTarget := ""
	if !detachOnDestroy && killed {
		for _, n := range reg.names() {
			if n != s.name {
				switchTarget = n
				break
			}
		}
	}
	for ep := range attachments {
		if switchTarget != "" {
			sendTo(ep, &proto.ServerMsg{SwitchSession: switchTarget})
		}
		closeClient(ep)
	}
	for _, p := range popups {
		p.Close()
	}
	for _, wl := range windows {
		// Drop this session's view of each window; a window another session still
		// links survives (only the last viewer stops the actor + closes panes).
		releaseWindow(wl, true)
	}
	reg.remove(s.name)
	if killReplyCh != nil {
		killReplyCh <- struct{}{}
	}
}

// keyBytes translates one send-keys argument: a named key, a C-x control
// chord, or literal text.
func keyBytes(arg string) []byte {
	switch arg {
	case "Enter":
		return []byte{'\r'}
	case "Tab":
		return []byte{'\t'}
	case "Escape":
		return []byte{0x1b}
	case "Space":
		return []byte{' '}
	}
	if len(arg) == 3 && arg[0] == 'C' && arg[1] == '-' {
		return []byte{arg[2] & 0x1f}
	}
	return []byte(arg)
}

// mergeCmds appends any cmds not already in base (dedup by value), so the
// session's #server() union set grows without duplicates as clients attach.
func mergeCmds(base, cmds []string) []string {
	for _, c := range cmds {
		found := false
		for _, b := range base {
			if b == c {
				found = true
				break
			}
		}
		if !found {
			base = append(base, c)
		}
	}
	return base
}

// winlink is one session's reference to a window: the shared window actor plus
// this session's own view handle (its render-gating for that window). tmux's
// winlink pairs a window with a per-session index; here the index is the slice
// position. With winlinks one actor can appear in several sessions' slices — the
// per-session view is what distinguishes "this session's view" among the actor's
// (eventually many) views. Today each actor has exactly one view.
type winlink struct {
	actor *windowActor
	view  *view
}

// indexOfWindow finds the winlink wrapping w (w comes from pane.win, the inner
// *window), or -1. Identity is on the embedded window pointer.
func indexOfWindow(windows []winlink, w *window) int {
	for i, ww := range windows {
		if ww.actor.window == w {
			return i
		}
	}
	return -1
}
