package server

import (
	"bytes"
	"sync"
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
	events chan any
	// ctl carries doMsg/stopMsg with priority over events: under a flood the
	// events buffer stays full of queued pty output (with reader goroutines
	// parked on its sendq), so a control message routed through events would
	// wait behind seconds of parse work — or, worse, an actorDo select-send
	// competing with parked readers starves forever. run() checks ctl first
	// on every iteration, so control runs within one output chunk's parse.
	ctl      chan any
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
	renders  chan<- *view
	notify   chan<- any // the viewing session's event channel (winlinkGone, etc.)
	isActive bool

	// renderMu protects a bounded, coalescing mailbox between the window actor
	// and this viewing session. dirtyContent() is destructive: once a pane's
	// changed rows have been copied out, dropping that render permanently loses
	// them. Keep the newest complete copy of every pending row per pane instead.
	// The notifier puts at most one *view token on renders, so a slow/dead session
	// can neither block the actor nor accumulate a FIFO of stale frames.
	renderMu       sync.Mutex
	renderPending  []renderMsg
	renderNotified bool
	renderWake     chan struct{}
	renderStop     chan struct{}
	renderDone     chan struct{}
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

func newView(renders chan<- *view, notify chan<- any) *view {
	v := &view{
		renders:    renders,
		notify:     notify,
		renderWake: make(chan struct{}, 1),
		renderStop: make(chan struct{}),
		renderDone: make(chan struct{}),
	}
	go v.forwardRenderNotice()
	return v
}

// forwardRenderNotice is the only potentially-blocking part of render
// delivery. It sends a lightweight view token, not a frame. While it waits for
// the session, queueRender continues folding new pane state into the mailbox.
func (v *view) forwardRenderNotice() {
	defer close(v.renderDone)
	for {
		select {
		case <-v.renderWake:
			select {
			case v.renders <- v:
			case <-v.renderStop:
				return
			}
		case <-v.renderStop:
			return
		}
	}
}

func cloneRenderMsg(rm renderMsg) renderMsg {
	if rm.content != nil {
		content := *rm.content
		content.Lines = make(map[int]emu.Line, len(rm.content.Lines))
		for row, line := range rm.content.Lines {
			content.Lines[row] = line
		}
		rm.content = &content
	}
	rm.hostOut = append([]byte(nil), rm.hostOut...)
	rm.cmdExits = append([]int(nil), rm.cmdExits...)
	rm.clipboards = append([]string(nil), rm.clipboards...)
	return rm
}

func mergeRenderMsg(dst *renderMsg, src renderMsg) {
	if src.content != nil {
		if dst.content == nil {
			cloned := cloneRenderMsg(renderMsg{content: src.content})
			dst.content = cloned.content
		} else {
			for row, line := range src.content.Lines {
				dst.content.Lines[row] = line
			}
			dst.content.Cursor = src.content.Cursor
			dst.content.CursorVisible = src.content.CursorVisible
		}
	}
	dst.bell = dst.bell || src.bell
	dst.modeFlip = dst.modeFlip || src.modeFlip
	dst.hostOut = append(dst.hostOut, src.hostOut...)
	dst.cmdExits = append(dst.cmdExits, src.cmdExits...)
	dst.clipboards = append(dst.clipboards, src.clipboards...)
}

func (v *view) queueRender(rm renderMsg) {
	v.renderMu.Lock()
	for i := range v.renderPending {
		if v.renderPending[i].pane == rm.pane {
			mergeRenderMsg(&v.renderPending[i], rm)
			rm = renderMsg{}
			break
		}
	}
	if rm.pane != nil {
		v.renderPending = append(v.renderPending, cloneRenderMsg(rm))
	}
	wake := !v.renderNotified
	if wake {
		v.renderNotified = true
	}
	v.renderMu.Unlock()

	if wake {
		select {
		case v.renderWake <- struct{}{}:
		default:
		}
	}
}

// takeRenders consumes the freshest coalesced state represented by one view
// token. Resetting renderNotified under the same lock makes an output racing
// with this drain schedule a new token rather than getting stranded.
func (v *view) takeRenders() []renderMsg {
	v.renderMu.Lock()
	pending := v.renderPending
	v.renderPending = nil
	v.renderNotified = false
	v.renderMu.Unlock()
	return pending
}

func (v *view) stopRenders() {
	// The view has already been removed from wa.views on the actor goroutine, so
	// no producer can add more. Clear anything represented by a token that may
	// already be sitting in the session channel; takeRenders will then be a no-op
	// instead of painting a window after it was unlinked.
	v.renderMu.Lock()
	v.renderPending = nil
	v.renderNotified = false
	v.renderMu.Unlock()
	close(v.renderStop)
	<-v.renderDone
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

// stopMsg ends run(). finishStop enqueues it (on ctl) instead of closing
// wa.events: pane reader goroutines send outputMsg straight to origin.events
// (session.go), and a straggler read landing after a close() would panic
// ("send on closed channel") — which, being in a goroutine, would take the
// whole server down. ctl priority means queued-but-unparsed output is dropped
// at stop — fine, the window is being torn down.
type stopMsg struct{}

// renderMsg is what the actor hands back to the session on each applied output:
// the pane, its diff (non-nil only when this window is current and the pane
// visible — the session fans it out), and whether the chunk contained a BEL (for
// monitor-bell). The session also does activity/silence detection off this.
type renderMsg struct {
	pane    *pane
	content *proto.PaneContent
	bell    bool
	// modeFlip: the pane's app toggled a mode the client mirrors in PaneRect —
	// mouse tracking (WantsMouse) or kitty keyboard (KeyFlags) → resend Layout.
	// One flag because the only consumer (session.go) reacts identically to both.
	modeFlip bool
	hostOut  []byte // un-doubled allow-passthrough payload to forward raw to the client terminal
	cmdExits []int  // OSC 133 command-finished exit codes in this chunk (gtmux.on("command-exited"))
	// clipboards: OSC 52 set-clipboard payloads an app emitted in this chunk,
	// forwarded to the outer terminal (set-clipboard) + the paste buffer. Rides
	// the same visibility gate as hostOut: only views that see the pane.
	clipboards []string
}

func newWindowActor(w *window) *windowActor {
	wa := &windowActor{window: w, events: make(chan any, 256), ctl: make(chan any, 16), relay: map[*pane]*windowActor{}, done: make(chan struct{})}
	w.actor = wa // back-ref: pane.win.actor reaches the owner (routes PTY events)
	return wa
}

// start subscribes the owning session (its render + event channels) as this
// window's first view and launches the actor goroutine. The returned view is the
// session's handle for toggling isActive via setActive.
func (wa *windowActor) start(renders chan<- *view, notify chan<- any) *view {
	vw := newView(renders, notify)
	wa.views = append(wa.views, vw)
	go wa.run()
	return vw
}

func (wa *windowActor) run() {
	defer close(wa.done) // signal stopActor that the goroutine has drained + exited
	// handleCtl runs one control message; true means stop.
	handleCtl := func(e any) bool {
		switch ev := e.(type) {
		case doMsg:
			ev.fn()
			close(ev.done)
		case stopMsg:
			return true
		}
		return false
	}
	for {
		// Control first (see the ctl field comment): actorDo must not wait
		// behind a flood's queued output.
		select {
		case c := <-wa.ctl:
			if handleCtl(c) {
				return // defer close(wa.done) fires; unread stragglers are dropped
			}
			continue
		default:
		}
		var e any
		select {
		case c := <-wa.ctl:
			if handleCtl(c) {
				return
			}
			continue
		case e = <-wa.events:
		}
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
			clips := ev.pane.takeClipboards()
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
			//   - queue in each view's bounded coalescing mailbox. One slow/dead
			//     viewer must never stall the actor, but dirty rows must not be
			//     dropped either: dirtyContent consumes them, and a later cursor-only
			//     diff cannot reconstruct a discarded character update.
			//   - compute the destructive diff ONCE and share the (read-only) pointer
			//     across current views, since dirtyContent consumes dirty state. Each
			//     mailbox clones it before merging and also preserves bell/mode/hook
			//     side effects across a coalesced burst.
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
					rm.clipboards = clips
				}
				vw.queueRender(rm)
			}
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
