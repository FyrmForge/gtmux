package server

import (
	"bytes"
	"time"

	"github.com/FyrmForge/gtmux/internal/emu"
	"github.com/FyrmForge/gtmux/internal/proto"
)

// windowActor is the redesign's window-ownership unit (see WINDOW_ACTORS.md).
//
// It embeds *window (so wa.panes/wa.active/wa.reflow()/… promote unchanged) and
// runs its own goroutine that is the SOLE mutator of the window's grids and pane
// tree. The session never touches window state concurrently: it sends output
// (outputMsg) and runs reads/mutations via actorDo (doMsg), and the actor pushes
// renderMsgs back for the session to fan out + run alert detection on.
type windowActor struct {
	*window
	events   chan any
	views    []*view                // sessions displaying this window (P1: always exactly one)
	relay    map[*pane]*windowActor // panes that migrated away: forward their output to the new actor
	done     chan struct{}          // closed when run() exits (stopActor waits on it)
	stopped  bool                   // set by finishStop before the stopMsg; guards late reader events (session goroutine only)
	stopping bool                   // torn down but still relaying migrated panes: finish when relay empties
	// The window owns its grid size (tmux window-size + aggressive-resize),
	// computed from its views' votes — so several sessions of different sizes
	// sharing one window don't fight over it. winSize is the combine policy
	// (smallest/largest/latest/manual); sizeSeq stamps votes for "latest".
	winSize string
	sizeSeq int
}

// view is one (session, this-window) pair the actor renders to: the viewing
// session's pumped fan-out/alert channel, its event channel (so the session that
// destroys a shared window can tell the other viewers to drop their winlink), and
// whether this window is that session's current one. Winlinks make the set >1
// (one window in several sessions). See the fan-out note in run().
type view struct {
	renders  chan<- renderMsg
	notify   chan<- any // the viewing session's event channel (winlinkGone, etc.)
	isActive bool
	// This session's size vote for the window (its effectiveSize) and whether it
	// wants aggressive-resize; seq is the actor's stamp for "latest". cols == 0
	// means the session hasn't sized this window yet (skipped in the combine).
	cols, rows int
	aggressive bool
	seq        int
	// Alert state is per-VIEW (tmux's per-winlink flags): a window can show
	// activity in a session that isn't looking at it while being current in
	// another. Mutated only on the viewing session's goroutine (handleRender /
	// switchToWindow), so no lock even for a shared window.
	activity   bool        // output seen while this session isn't viewing the window (monitor-activity)
	bell       bool        // a BEL seen while not viewing (monitor-bell)
	silence    bool        // no output for the monitor-silence interval while not viewing
	silenceTmr *time.Timer // fires a silenceEvent after the interval; reset on each output
}

// outputMsg is pty output for a pane (gen guards against a pre-respawn reader).
type outputMsg struct {
	pane *pane
	gen  int
	data []byte
}

// doMsg runs a window-touching closure on the actor goroutine (actorDo). fn must
// touch only window state and must not call actorDo on the same actor (deadlock).
type doMsg struct {
	fn   func()
	done chan struct{}
}

// stopMsg ends run(). finishStop enqueues it instead of closing wa.events: pane
// reader goroutines send outputMsg straight to origin.events (session.go), and a
// straggler read landing after a close() would panic ("send on closed channel")
// — which, being in a goroutine, would take the whole server down. FIFO means
// run() drains everything queued before the sentinel; anything a reader sends
// AFTER it just sits unread in the buffered channel until GC.
type stopMsg struct{}

// renderMsg is what the actor hands back to the session on each applied output:
// the pane, its diff (non-nil only when this window is current and the pane
// visible — the session fans it out), and whether the chunk contained a BEL (for
// monitor-bell). The session also does activity/silence detection off this.
type renderMsg struct {
	pane      *pane
	content   *proto.PaneContent
	bell      bool
	// modeFlip: the pane's app toggled a mode the client mirrors in PaneRect —
	// mouse tracking (WantsMouse) or kitty keyboard (KeyFlags) → resend Layout.
	// One flag because the only consumer (session.go) reacts identically to both.
	modeFlip  bool
	hostOut   []byte // un-doubled allow-passthrough payload to forward raw to the client terminal
	cmdExits  []int  // OSC 133 command-finished exit codes in this chunk (gtmux.on("command-exited"))
}

func newWindowActor(w *window) *windowActor {
	wa := &windowActor{window: w, events: make(chan any, 256), relay: map[*pane]*windowActor{}, done: make(chan struct{})}
	w.actor = wa // back-ref: pane.win.actor reaches the owner (routes PTY events)
	return wa
}

// start subscribes the owning session (its render + event channels) as this
// window's first view and launches the actor goroutine. The returned view is the
// session's handle for toggling isActive via setActive.
func (wa *windowActor) start(renders chan<- renderMsg, notify chan<- any) *view {
	vw := &view{renders: renders, notify: notify}
	wa.views = append(wa.views, vw)
	go wa.run()
	return vw
}

func (wa *windowActor) run() {
	defer close(wa.done) // signal stopActor that the goroutine has drained + exited
	for e := range wa.events {
		switch ev := e.(type) {
		case outputMsg:
			if ev.gen != ev.pane.gen { // stale reader from before a respawn
				continue
			}
			// Migrated pane: forward to its new window actor, reliably and in
			// receipt order (one path reader→origin→target). Raw output is real
			// terminal content, never dropped. Applied + rendered by the target.
			if to := wa.relay[ev.pane]; to != nil {
				to.events <- ev
				continue
			}
			modeBefore := ev.pane.term.Mode() & emu.ModeMouseMask
			keyBefore := ev.pane.term.KeyState()
			hostOut := wa.applyOutput(ev.pane, ev.data)
			bell := bytes.IndexByte(ev.data, 0x07) >= 0
			// Drain OSC 133 command-finished exits now (before dirtyContent's diff
			// resets the dirty state below), regardless of any view being active.
			cmdExits := ev.pane.takeCommandExits()
			// An app toggling mouse tracking (DECSET/DECRST 1000-1003) changes
			// PaneRect.WantsMouse, which the client uses to decide own-vs-forward;
			// same for the kitty keyboard protocol (CSI > flags u), where the
			// client renegotiates extended-keys with its outer terminal. Both are
			// infrequent (real mode flips, not every frame), so resend Layout then.
			// Bracketed paste (2004) is deliberately NOT here — it's gated
			// server-side (pane.filterPaste), so the client needs no notification,
			// and zsh toggles 2004 on every prompt.
			modeFlip := ev.pane.term.Mode()&emu.ModeMouseMask != modeBefore ||
				ev.pane.term.KeyState() != keyBefore
			// Fan out to every viewing session, gating content per-view on that
			// session's current-window choice (spike4/spike5 discipline):
			//   - non-blocking send, drop on a full view — one slow/dead viewer must
			//     never stall the actor (that's what froze a shared window / would
			//     deadlock every other session). A live session drains continuously
			//     so its 256-buffer never fills → no drops → identical to blocking;
			//     only a hung/dying view drops, and dropped content self-heals (the
			//     next output chunk, or a fullSync on window switch).
			//   - compute the destructive diff ONCE and share the (read-only) pointer
			//     across current views, since dirtyContent consumes dirty state.
			// ponytail: a bell riding a dropped frame is a missed alert — tolerable
			// best-effort (drops need sustained backpressure to a live session, which
			// doesn't happen); reliable alert delivery isn't worth a second channel.
			var content *proto.PaneContent
			for _, vw := range wa.views {
				rm := renderMsg{pane: ev.pane, bell: bell, modeFlip: modeFlip, cmdExits: cmdExits}
				if vw.isActive && (!wa.zoomed || ev.pane == wa.active) {
					if content == nil {
						c := ev.pane.dirtyContent()
						content = &c
					}
					rm.content = content
					// Passthrough rides the same visibility gate as content: only
					// forward when this view's client actually sees the pane (current
					// window, not hidden behind a zoom) — tmux passes through for the
					// visible pane only.
					rm.hostOut = hostOut
				}
				select {
				case vw.renders <- rm:
				default: // view not draining (hung/dying) — drop; self-heals
				}
			}
		case doMsg:
			ev.fn()
			close(ev.done)
		case stopMsg:
			return // defer close(wa.done) fires; unread stragglers are dropped
		}
	}
}

// setViewSize records one session's size vote (its effectiveSize) + options for
// this window and recomputes the grid. Runs on the actor goroutine (via actorDo).
func (wa *windowActor) setViewSize(vw *view, cols, rows int, aggressive bool, winSize string) bool {
	wa.sizeSeq++
	vw.cols, vw.rows, vw.aggressive, vw.seq = cols, rows, aggressive, wa.sizeSeq
	wa.winSize = winSize
	return wa.recomputeSize(vw)
}

// recomputeSize picks the window's grid from its viewing sessions' votes per
// window-size + aggressive-resize, resizes if it changed, and tells the OTHER
// viewers (not the initiator, who fullSyncs itself) to redraw. Runs on the actor
// goroutine. Single-viewer windows (the common case) just track their one vote.
func (wa *windowActor) recomputeSize(initiator *view) bool {
	if wa.winSize == "manual" {
		return false // grid is fixed by resize-window, not by clients
	}
	vc, vr, bestSeq, have := 0, 0, 0, false
	for _, v := range wa.views {
		if v.cols == 0 { // session hasn't sized this window yet
			continue
		}
		if v.aggressive && !v.isActive { // aggressive-resize: only where it's current
			continue
		}
		switch wa.winSize {
		case "largest":
			if !have || v.cols > vc {
				vc = v.cols
			}
			if !have || v.rows > vr {
				vr = v.rows
			}
		case "latest":
			if !have || v.seq > bestSeq {
				vc, vr, bestSeq = v.cols, v.rows, v.seq
			}
		default: // "smallest": never overflow any client
			if !have || v.cols < vc {
				vc = v.cols
			}
			if !have || v.rows < vr {
				vr = v.rows
			}
		}
		have = true
	}
	if !have || (vc == wa.cols && vr == wa.rows) {
		return false
	}
	wa.resize(vc, vr)
	for _, other := range wa.views {
		if other == initiator {
			continue
		}
		select {
		case other.notify <- windowResized{wa}:
		default: // session busy; it relayouts on its next switch/attach
		}
	}
	return true
}

// applyOutput writes pty output into the pane's grid and tees it to a pipe-pane
// target — runs on the actor goroutine. It first strips any tmux
// allow-passthrough DCS wrappers (which go-vte can't handle; see passthrough.go)
// and returns their un-doubled payload for the session to forward. Stripping
// happens whether or not passthrough is enabled — an un-stripped payload would
// otherwise execute on gtmux's own emu. pipe-pane still sees the raw stream.
func (wa *windowActor) applyOutput(p *pane, data []byte) (host []byte) {
	clean, host := p.ptScan.scan(data)
	p.term.Write(clean)
	if p.pipeW != nil {
		if _, err := p.pipeW.Write(data); err != nil {
			p.pipeW.Close()
			p.pipeW = nil
		}
	}
	return host
}
