// Package client implements the gtmux attach logic: milestone 1 just forwards
// raw stdin/stdout, no key translation or redraw diffing yet.
package client

import (
	"encoding/gob"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/FyrmForge/gtmux/internal/config"
	"github.com/FyrmForge/gtmux/internal/proto"
)

// byteKey canonicalizes a single non-escape input byte into the bind token that
// config.parseKeyName also produces: control bytes 0x01-0x1a → "C-a".."C-z"
// (folded exactly like the config side, so C-a and C-A collapse as they always
// have), DEL → "BSpace", a printable char → itself. "" means the byte has no
// bind token (NUL, other C0) and the caller forwards it raw. ESC (0x1b) never
// reaches here — the escape collector owns it.
func byteKey(b byte) string {
	switch {
	case b >= 0x01 && b <= 0x1a:
		return "C-" + string(rune('a'+b-1))
	case b >= 0x1c && b <= 0x1f: // C-\ C-] C-^ C-_ (0x1b = ESC is the escape lead)
		return "C-" + string(rune(b|0x40))
	case b == 0x7f:
		return "BSpace"
	case b >= 0x20 && b <= 0x7e:
		return string(b)
	}
	return ""
}

// advancePaste steps a bracketed-paste marker matcher: m is how many bytes of
// pat matched so far, b is the next byte. Returns the new progress and whether
// pat just completed. On a mismatch it restarts at 1 if b is pat's first byte
// (ESC), else 0 — enough for "\x1b[200~"/"\x1b[201~" whose only repeatable byte
// is the leading ESC. Used by the mouse pre-scan to know when a paste is in
// flight (so a pasted SGR-mouse-looking sequence isn't latched and eaten).
func advancePaste(m int, pat []byte, b byte) (int, bool) {
	if b == pat[m] {
		if m+1 == len(pat) {
			return 0, true
		}
		return m + 1, false
	}
	if b == pat[0] {
		return 1, false
	}
	return 0, false
}

// csiKeyName maps a CSI sequence body (bytes after "ESC [") to a bind token.
// Modified forms (e.g. "1;5D" = C-Left) are intentionally absent — the full
// modifier matrix (CSI-u / modifyOtherKeys) is out of scope; they fall through
// to raw passthrough (or the hardcoded prefix-resize fallback).
var csiKeyName = map[string]string{
	"A": "Up", "B": "Down", "C": "Right", "D": "Left", "H": "Home", "F": "End",
	"1~": "Home", "2~": "Insert", "3~": "Delete", "4~": "End",
	"5~": "PgUp", "6~": "PgDn", "7~": "Home", "8~": "End",
	"11~": "F1", "12~": "F2", "13~": "F3", "14~": "F4", "15~": "F5",
	"17~": "F6", "18~": "F7", "19~": "F8", "20~": "F9", "21~": "F10",
	"23~": "F11", "24~": "F12",
}

// ss3KeyName maps an SS3 final byte (after "ESC O") to a bind token: F1–F4 and
// the application-cursor-mode arrow/Home/End keys.
var ss3KeyName = map[string]string{
	"P": "F1", "Q": "F2", "R": "F3", "S": "F4",
	"A": "Up", "B": "Down", "C": "Right", "D": "Left", "H": "Home", "F": "End",
}

// guardPanic is deferred at the top of each client goroutine: on a panic it
// runs restore (undo raw mode / mouse reporting so the pane isn't left wedged),
// then re-raises so the crash still surfaces its message and stack. A panic in
// a spawned goroutine bypasses the main goroutine's cleanup defers, so this is
// the only hook that can restore the terminal before the process dies.
func guardPanic(restore func()) {
	if r := recover(); r != nil {
		restore()
		panic(r)
	}
}

// statusReserve is how many rows the status bar occupies for a StatusLines
// setting (tmux `status` 0..5), matching compositor.statusLines(). The client
// reserves these locally and reports only the *content* height to the server —
// status-bar reservation is entirely client-side.
func statusReserve(sl int) int {
	switch {
	case sl < 0:
		return 0
	case sl > 5:
		return 5
	}
	return sl
}

// dockInset sums the size of docked widgets on the given edges (used to shrink
// the window size the client reports to the server). cols is the client's
// physical width, so min_cols-hidden docks don't count; this static path only
// serves attach time — after that comp.reportSize (which also knows
// toggle_dock state) is the source of truth.
func dockInset(widgets []config.WidgetSpec, cols int, edges ...string) int {
	n := 0
	for _, w := range widgets {
		if w.MinCols > 0 && cols > 0 && cols < w.MinCols {
			continue
		}
		for _, e := range edges {
			if w.Dock == e {
				n += w.Size
			}
		}
	}
	return n
}

// frameReserve is the cells reserved on each window edge by pane_borders="framed"
// (the outer frame): 1 per side, so 2 off each dimension. 0 otherwise.
func frameReserve(borders string) int {
	if borders == "framed" {
		return 2
	}
	return 0
}

// contentRows is the window height the client reports to the server: physical
// rows minus the status rows, any top/bottom docks, and the framed border (never
// below 1).
func contentRows(rows, cols, sl int, widgets []config.WidgetSpec, borders string) int {
	if r := rows - statusReserve(sl) - dockInset(widgets, cols, "top", "bottom") - frameReserve(borders); r > 0 {
		return r
	}
	return 1
}

// contentCols is the window width the client reports to the server: physical
// cols minus the columns reserved by left/right docks and the framed border
// (never below 1).
func contentCols(cols int, widgets []config.WidgetSpec, borders string) int {
	if c := cols - dockInset(widgets, cols, "left", "right") - frameReserve(borders); c > 0 {
		return c
	}
	return 1
}

// wantSnapshot reports whether any widget uses a Lua function (text or on_click),
// which needs the server's state snapshot. Static-text widgets don't, so the
// server skips the snapshot work for the common case.
func wantSnapshot(widgets []config.WidgetSpec) bool {
	for _, w := range widgets {
		if w.TextFn != nil || w.OnClick != nil || w.Draw != nil || w.Component != nil {
			return true
		}
	}
	return false
}

// prefixLabel formats a prefix key byte as a tmux-style name (0x02 → "C-b") for
// gtmux.context().
func prefixLabel(b byte) string {
	if b == 0 {
		return ""
	}
	if b < 0x20 {
		return "C-" + string(rune(b+0x60))
	}
	return string(rune(b))
}

// Attach is `gtmux attach [-r]`: read-only when ro is set.
func Attach(session string, ro bool) error { return RunGroup(session, false, "", ro) }

// RunGroup is Run with a group target: new-session -t <groupTarget> joins that
// session's group (displays its current windows). readOnly is attach -r.
func RunGroup(session string, create bool, groupTarget string, readOnly bool) error {
	// Refuse to attach to a session on the SAME server from inside one of its
	// own panes — that renders a session into itself. $GTMUX is "sock,pid,name"
	// (set on every spawned pane, pane.go); compare only the socket so attaching
	// to a DIFFERENT server (other -S socket, a remote one) still works. tmux
	// does the same and points at unsetting the var to force it.
	if g := os.Getenv("GTMUX"); g != "" {
		if sock, _, _ := strings.Cut(g, ","); sock == proto.SockPath() {
			return fmt.Errorf("sessions should be nested with care, unset $GTMUX to force")
		}
	}

	if err := ensureServer(); err != nil {
		return err
	}

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return err
	}
	// restoreTerm undoes raw mode and mouse reporting so the pane stays usable.
	// Idempotent (sync.Once): the main-goroutine exit defer calls it, and so does
	// each spawned goroutine's panic recover (guardPanic) — a panic in a goroutine
	// bypasses main's defers, so without this a crash left the pane in raw mode
	// with mouse reporting on (wedged). Skips compMu on purpose: a panicking
	// goroutine may hold it, and a torn stdout write while crashing is harmless.
	var restoreOnce sync.Once
	restoreTerm := func() {
		restoreOnce.Do(func() {
			os.Stdout.Write([]byte("\x1b[?1002l\x1b[?1006l\x1b[?2004l")) // disable mouse (no-op if off) + bracketed paste
			term.Restore(int(os.Stdin.Fd()), oldState)
		})
	}
	defer restoreTerm()

	cwd, _ := os.Getwd()
	// The client owns all input: cliCfg is chrome; binds is the prefix key +
	// keybind table it resolves locally, sending the server an Action (or
	// opening a local prompt/picker overlay) instead of forwarding raw keys.
	cliCfg, binds0 := config.LoadClient(config.ClientConfigPath())
	// binds is swapped live by source-file/set-option, from both the stdin
	// goroutine (local keybinds) and the decode goroutine (a server push), so
	// it's an atomic pointer: per-key reads stay lock-free, swaps are safe from
	// either goroutine. Old bind VMs fall out of reference and are GC'd; the
	// exit defer closes whichever is current.
	var bindsPtr atomic.Pointer[config.ClientBinds]
	bindsPtr.Store(binds0)
	curBinds := func() *config.ClientBinds { return bindsPtr.Load() }
	defer func() { curBinds().Close() }()
	// The client owns the status formats; the server can't see which #server()
	// commands they use, so extract them and send the list at attach.
	serverCmds := extractServerCmds(cliCfg.StatusLeft, cliCfg.StatusRight)

	// enc.Encode is called from the stdin-forwarding goroutine and the
	// SIGWINCH-resize goroutine below; gob's encoder isn't safe for
	// concurrent use, so every send goes through this mutex — which also
	// covers swapping the encoder out on a session-switch reconnect.
	var encMu sync.Mutex
	var rawEnc *gob.Encoder
	send := func(msg *proto.ClientMsg) error {
		encMu.Lock()
		defer encMu.Unlock()
		if rawEnc == nil {
			return nil
		}
		return rawEnc.Encode(msg)
	}

	// comp is the current connection's compositor, shared across the stdin,
	// SIGWINCH, and decode goroutines. compMu guards it and serializes their
	// writes to stdout. The stdin goroutine uses it to intercept copy-mode keys
	// locally (client-owned copy-mode) instead of forwarding them to the server.
	var compMu sync.Mutex
	var comp *compositor
	// On exit, restore the outer terminal's title if set-titles pushed one, and
	// pop any extended-keys kitty entry we pushed.
	defer func() {
		if comp != nil {
			os.Stdout.Write(comp.restoreTitle())
			os.Stdout.Write(comp.restoreKitty())
		}
	}()

	// installHooks wires a bind VM's widget query primitives to live compositor
	// state. Called under compMu (from RunText/RunClick), so the closures read
	// comp fields directly, no locking. Set on binds0 here and on every reload's
	// fresh VM (applyCfg).
	installHooks := func(b *config.ClientBinds) {
		b.Hooks = config.WidgetHooks{
			Snapshot: func() *proto.StateSnapshot {
				if comp != nil {
					return comp.snapshot
				}
				return nil
			},
			Context: func() map[string]string {
				m := map[string]string{}
				if comp != nil && comp.status != nil {
					m["session"] = comp.status.Vars["session"]
					m["window"] = comp.status.Vars["window_index"]
					m["pane"] = comp.status.Vars["pane_id"]
				}
				if comp != nil {
					m["prefix"] = prefixLabel(curBinds().Prefix)
					m["width"] = strconv.Itoa(comp.phyCols)
					m["height"] = strconv.Itoa(comp.phyRows)
				}
				return m
			},
			Expand: func(s string) string {
				if comp != nil && comp.expander != nil && comp.status != nil {
					return comp.expander.expand(s, comp.status.Vars, comp.status.ServerShell)
				}
				return s
			},
			Option: func(name string) string {
				if comp != nil {
					return comp.optionValue(name)
				}
				return ""
			},
		}
	}
	installHooks(binds0)

	// Live config state, shared by the stdin goroutine (local :set / source-file)
	// and the decode goroutine (a server-pushed set-option). cfgMu guards the
	// override list + path; binds is the atomic pointer above; comp.cfg swaps
	// under compMu. cfgPath is the file source-file reloads; overrides are the
	// runtime set-option pairs applied over it (last wins), re-derived from the
	// file on each change so a set-option is exact after a reload.
	var cfgMu sync.Mutex
	cfgPath := config.ClientConfigPath()
	var overrides [][2]string

	// applyCfg swaps in a freshly-derived config and repaints. Caller holds
	// cfgMu; takes compMu for the compositor swap (order: cfgMu → compMu).
	applyCfg := func(newCfg config.ClientConfig, newBinds *config.ClientBinds) {
		installHooks(newBinds)
		bindsPtr.Store(newBinds)
		reported := false
		var rc, rr int
		compMu.Lock()
		if comp != nil {
			// newBinds owns the new config's widget fns — reload rebuilds the
			// widgets against it, so a re-sourced status bar/dock actually changes.
			os.Stdout.Write(comp.reload(newCfg, newBinds))
			rc, rr = comp.reportSize()
			reported = true
		}
		compMu.Unlock()
		// Re-report our size with the NEW insets: docks and pane_borders="framed"
		// change how many rows/cols the window gets, and the server owns the
		// layout. Without this the compositor renders the new chrome against the
		// OLD layout until some unrelated resize — toggling pane_borders live
		// left the dock's box drawn for the old height, so its bottom border
		// landed on a row framed now treats as frame and vanished. From
		// comp.reportSize so min_cols-hidden docks aren't counted. Sent outside
		// compMu (lock order vs the SIGWINCH goroutine).
		if reported {
			send(&proto.ClientMsg{Resize: &proto.Resize{Cols: rc, Rows: rr}})
		} else if cols, rows, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
			send(&proto.ClientMsg{Resize: &proto.Resize{
				Cols: contentCols(cols, newCfg.Widgets, newCfg.PaneBorders),
				Rows: contentRows(rows, cols, newCfg.StatusLines, newCfg.Widgets, newCfg.PaneBorders),
			}})
		}
	}
	// applyOverride records one runtime option and re-derives the config. Used
	// for a local client-option :set and for a server push — no routing, so a
	// pushed unknown option is a harmless no-op (never bounces back).
	applyOverride := func(name, value string) {
		cfgMu.Lock()
		defer cfgMu.Unlock()
		overrides = append(overrides, [2]string{name, value})
		newCfg, newBinds := config.LoadClientWith(cfgPath, overrides)
		applyCfg(newCfg, newBinds)
	}
	// reloadConfig re-runs the config (source-file): fresh VM resets to defaults
	// then re-evals the file (reset-then-eval, so a dropped bind/color really
	// goes away); runtime overrides are cleared — the file is a clean slate.
	// ponytail: no layering — `source-file X` loads defaults+X, not X atop
	// client.lua; the no-arg "reload my config" case is exact, the common one.
	// Status #server() command changes + live mouse toggle are follow-ups.
	reloadConfig := func(args []string) {
		cfgMu.Lock()
		defer cfgMu.Unlock()
		if len(args) > 0 && args[0] != "" {
			cfgPath = args[0]
		}
		overrides = nil
		newCfg, newBinds := config.LoadClientWith(cfgPath, nil)
		applyCfg(newCfg, newBinds)
	}
	// setOption handles a locally-typed set-option: client-owned options apply
	// live here; anything else (server options like main_pane_*) is forwarded to
	// the session's runCommand, which owns that value.
	setOption := func(args []string) {
		if len(args) < 2 {
			return
		}
		if !config.IsClientOption(args[0]) {
			send(&proto.ClientMsg{Action: &proto.Action{Args: append([]string{"set-option"}, args...)}})
			return
		}
		applyOverride(args[0], args[1])
	}
	// applyBindKey installs/removes a runtime keybind on the live bind table
	// (tmux bind-key/unbind-key, pushed from the server). bind-key [-n] key
	// [command...]; unbind-key [-n] key. -r/-T are consumed but not modeled for
	// runtime binds (repeat + custom tables stay config-only).
	applyBindKey := func(action []string) {
		unbind := action[0] == "unbind-key"
		root := false
		rest := action[1:]
		for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
			if rest[0] == "-n" {
				root = true
			} else if rest[0] == "-T" && len(rest) > 1 {
				rest = rest[1:] // skip table name (custom tables not runtime-settable)
			}
			rest = rest[1:]
		}
		if len(rest) == 0 {
			return
		}
		key, ok := config.ParseKey(rest[0])
		if !ok {
			return
		}
		if unbind {
			curBinds().SetOverride(key, root, nil) // shadow any Lua bind as unbound
			return
		}
		if len(rest) < 2 {
			return // bind-key needs a command
		}
		curBinds().SetOverride(key, root, []config.BindOp{{Action: rest[1:]}})
	}
	// dispatch runs one resolved action. Client-local commands (source-file,
	// set-option) are handled here with no server round-trip; anything else goes
	// to the session's runCommand. Callers must not hold compMu.
	dispatch := func(action []string) {
		switch {
		case len(action) > 0 && action[0] == "source-file":
			reloadConfig(action[1:])
		case len(action) > 0 && (action[0] == "bind-key" || action[0] == "unbind-key"):
			// Runtime (un)bind pushed from the server: mutate this client's live
			// bind table. bind-key [-n] key [command...]; unbind-key [-n] key.
			applyBindKey(action)
		case len(action) > 0 && (action[0] == "set-option" || action[0] == "set"):
			setOption(action[1:])
		case len(action) > 0 && (action[0] == "command-prompt" || action[0] == "confirm-before"):
			// Interactive flow control opens a client-owned overlay (never sent to
			// the server). dispatch never holds compMu, so taking it here is safe.
			compMu.Lock()
			if comp != nil {
				os.Stdout.Write(comp.openFlowPrompt(action))
			}
			compMu.Unlock()
		case len(action) > 0 && action[0] == "display-menu":
			compMu.Lock()
			if comp != nil {
				os.Stdout.Write(comp.openMenu(action))
			}
			compMu.Unlock()
		case len(action) > 0 && (action[0] == "clock-mode" || action[0] == "lock" || action[0] == "lock-client"):
			compMu.Lock()
			if comp != nil {
				comp.clock = action[0] == "clock-mode"
				comp.locked = action[0] != "clock-mode"
				os.Stdout.Write(comp.redraw())
			}
			compMu.Unlock()
		case len(action) > 0 && action[0] == "send-prefix":
			// Send the literal prefix byte to the focused pane (for nested apps
			// like an inner gtmux/tmux). The client owns the prefix, so it's the
			// one that knows the byte.
			send(&proto.ClientMsg{Input: &proto.Input{Data: []byte{curBinds().Prefix}}})
		default:
			send(&proto.ClientMsg{Action: &proto.Action{Args: action}})
		}
	}

	// Mouse reporting (1002: button-event tracking, which includes motion
	// while a button is held — needed for drag-select — plus SGR extended
	// coordinates), mirroring the user's `set -g mouse on`. The server
	// parses clicks/drags out of the input stream for focus and copy-mode.
	if cliCfg.Mouse {
		os.Stdout.Write([]byte("\x1b[?1002h\x1b[?1006h")) // restoreTerm disables it on exit/panic
	}

	// Bracketed paste: unconditional, unlike mouse. Without it the outer
	// terminal dumps the clipboard raw, so every \r in pasted text runs as a
	// command, and pasted bytes get eaten by the prefix/bind/mouse machinery
	// below. The 200~/201~ markers let us tell data from keystrokes.
	os.Stdout.Write([]byte("\x1b[?2004h"))

	// Auto-reload: poll the active config file's mtime and re-run reloadConfig on
	// change, so edits to client.lua apply live without a manual source-file.
	// mtime polling (not fsnotify) keeps it dependency-free — 1s latency is fine
	// for a config file. reloadConfig is the same reset-then-eval source-file
	// uses; it reads cfgPath under cfgMu, so this reads it the same way. An editor
	// mid-save may briefly load a partial file; it self-heals on the next write.
	// ponytail: always on; gate behind a `set -g automatic-reload` option if it
	// ever needs to be opt-out.
	go func() {
		defer guardPanic(restoreTerm)
		stamp := func() (int64, bool) {
			cfgMu.Lock()
			p := cfgPath
			cfgMu.Unlock()
			if fi, err := os.Stat(p); err == nil {
				return fi.ModTime().UnixNano(), true
			}
			return 0, false
		}
		last, lastOK := stamp()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for range tick.C {
			// reloadConfig already updated the mtime we read here (it only reads
			// the file), so a real edit — not our own reload — is what differs.
			if mt, ok := stamp(); ok != lastOK || mt != last {
				last, lastOK = mt, ok
				reloadConfig(nil)
			}
		}
	}()

	// Animation ticker: repaint draw/component docks locally at a fast cadence so
	// Lua-drawn spinners animate smoothly, decoupled from the server's status
	// push. animateDocks re-runs the paint against the cached snapshot (no server
	// round-trip) and returns nil when there's nothing to animate. The write is
	// held under compMu, serializing with the render goroutine's own writes.
	go func() {
		defer guardPanic(restoreTerm)
		anim := time.NewTicker(150 * time.Millisecond)
		defer anim.Stop()
		for range anim.C {
			compMu.Lock()
			if comp != nil {
				if out := comp.animateDocks(); len(out) > 0 {
					os.Stdout.Write(out)
				}
			}
			compMu.Unlock()
		}
	}()

	go func() {
		defer guardPanic(restoreTerm) // a crash here must not leave the pane wedged
		buf := make([]byte, 4096)
		var mp mouseParser
		// Paste matcher for the mouse pre-scan, independent of processInput's
		// `pasting` (which drives the prefix/bind bypass). Own state so it works
		// WITHIN the opener's own read chunk and across reads: while a paste is in
		// flight the mouse parser is bypassed, so a pasted SGR-mouse sequence can't
		// be swallowed. pasteOpen/Close are the DECSET 2004 markers.
		pasteOpen := []byte("\x1b[200~")
		pasteClose := []byte("\x1b[201~")
		var openM, closeM int
		mousePasting := false
		// Prefix state machine (client owns input). Persist across reads: a
		// prefix key and its follow byte can land in separate Stdin.Read chunks.
		prefixPending := false
		curTable := "" // active custom key table (tmux switch-client -T); one key, then reverts
		// Escape-sequence collector, shared by the root and post-prefix paths.
		// escStage: 0 none, 1 saw ESC (kind undecided), 2 collecting CSI (ESC [),
		// 3 SS3 (ESC O). escPrefixed = the ESC followed the prefix. escRaw holds
		// the exact bytes from ESC on, forwarded verbatim if nothing binds them.
		escStage := 0
		escPrefixed := false
		var escRaw []byte
		// Bracketed paste: between ESC[200~ and ESC[201~ every byte is DATA, not a
		// key. Persists across reads (a paste easily spans several 4096 chunks).
		// While set, the prefix/bind machine is bypassed so a pasted prefix byte
		// (C-b) or a bind -n key can't fire, and the mouse pre-scan is skipped so a
		// pasted SGR-mouse-looking sequence isn't eaten. Only the ESC[201~
		// terminator ends it.
		pasting := false
		// tmux's -r: after a repeatable bind, the bare key keeps firing until
		// this window lapses. Checked at the next byte, so no timer/goroutine.
		// ponytail: captured once at attach; a runtime `set repeat-time` reload
		// won't retune this goroutine's window — nobody turns this knob live.
		repeatWindow := time.Duration(cliCfg.RepeatTime) * time.Millisecond
		repeatActive := false
		var repeatDeadline time.Time
		arrowFlag := map[byte]string{'A': "-U", 'B': "-D", 'C': "-R", 'D': "-L"}

		// runOps performs a resolved keybind's effects: a server Action, or a
		// local overlay opened from the client's own state. Local opens take
		// compMu; sends must not hold it (lock-order vs the SIGWINCH goroutine).
		runOps := func(ops []config.BindOp) {
			for _, op := range ops {
				if op.Table != "" {
					// switch-client -T: the next key comes from this table.
					curTable = op.Table
				} else if op.Local != "" {
					compMu.Lock()
					if comp != nil {
						os.Stdout.Write(comp.openLocal(op.Local))
					}
					compMu.Unlock()
				} else if op.Modal != nil {
					compMu.Lock()
					if comp != nil {
						comp.openModal(op.Modal, curBinds())
						os.Stdout.Write(comp.redraw())
					}
					compMu.Unlock()
				} else if op.Dock != "" {
					compMu.Lock()
					if comp != nil {
						comp.toggleDockFocus(op.Dock)
						os.Stdout.Write(comp.redraw())
					}
					compMu.Unlock()
				} else if op.ToggleDock != "" {
					// Visibility toggle changes the window's usable size, so the
					// server must re-layout: re-report from the visible set. Send
					// outside compMu (lock order vs the SIGWINCH goroutine).
					var rc, rr int
					toggled := false
					compMu.Lock()
					if comp != nil && comp.toggleDock(op.ToggleDock) {
						os.Stdout.Write(comp.redraw())
						rc, rr = comp.reportSize()
						toggled = true
					}
					compMu.Unlock()
					if toggled {
						send(&proto.ClientMsg{Resize: &proto.Resize{Cols: rc, Rows: rr}})
					}
				} else if op.Command != "" {
					if argv := tokenize(op.Command); len(argv) > 0 {
						dispatch(argv)
					}
				} else if len(op.Action) > 0 {
					// Pane navigation at the window edge steps into a nav-focusable
					// dock on that side instead of bouncing off the edge server-side.
					// (select-pane-vim is left alone: the server may hand the key to
					// vim, which the client can't predict.)
					if op.Action[0] == "select-pane" && len(op.Action) == 2 {
						consumed := false
						compMu.Lock()
						if comp != nil && comp.focusDockNav(op.Action[1]) {
							consumed = true
							os.Stdout.Write(comp.redraw())
						}
						compMu.Unlock()
						if consumed {
							continue
						}
					}
					dispatch(op.Action)
				}
			}
		}

		// forward keys that aren't consumed by the prefix/bind machine, run
		// prefix binds, and translate prefix+arrow into select/resize actions.
		processInput := func(pass []byte) {
			bd := curBinds() // snapshot the live bind table for this input batch
			var fwd []byte

			// finishEsc routes a fully-decoded escape sequence. name is the
			// canonical bind token ("" if the bytes have no token); csiSeq is the
			// CSI body, used only by the hardcoded prefix-navigation fallback; raw
			// is the exact input bytes, forwarded verbatim when nothing consumes
			// them (so a focused app still receives arrows/F-keys/Meta it doesn't
			// bind — the one trap of adding a root-level escape path).
			finishEsc := func(name, csiSeq string, prefixed bool, raw []byte) {
				if prefixed {
					if name != "" {
						if ops := bd.Resolve(name); ops != nil {
							runOps(ops)
							if bd.Repeat[name] {
								repeatActive, repeatDeadline = true, time.Now().Add(repeatWindow)
							}
							return
						}
					}
					// Hardcoded prefix+navigation (unchanged): arrow selects a pane,
					// C-/M-arrow resizes, PgUp enters copy-mode. A user prefix-bind
					// for the same named key (checked above) overrides these.
					switch {
					case len(csiSeq) == 1 && arrowFlag[csiSeq[0]] != "":
						runOps([]config.BindOp{{Action: []string{"select-pane", arrowFlag[csiSeq[0]]}}})
					case len(csiSeq) == 4 && csiSeq[:3] == "1;5" && arrowFlag[csiSeq[3]] != "":
						runOps([]config.BindOp{{Action: []string{"resize-pane", arrowFlag[csiSeq[3]], "1"}}})
					case len(csiSeq) == 4 && csiSeq[:3] == "1;3" && arrowFlag[csiSeq[3]] != "":
						runOps([]config.BindOp{{Action: []string{"resize-pane", arrowFlag[csiSeq[3]], "5"}}})
					case csiSeq == "5~": // prefix+PgUp: copy-mode a page up
						runOps([]config.BindOp{{Action: []string{"copy-mode", "-u"}}})
					}
					return
				}
				// Bracketed-paste markers delimit DATA. Flip the pasting state so the
				// byte loop routes the payload around the prefix/bind machine, and
				// forward the marker unconditionally: the server strips it when the
				// target pane's app hasn't enabled 2004 (it owns that state). The
				// client stays out of per-pane mode tracking.
				if csiSeq == "200~" || csiSeq == "201~" {
					pasting = csiSeq == "200~"
					fwd = append(fwd, raw...)
					return
				}
				// An esc sequence arriving mid-paste is pasted data, not a key —
				// forward it raw, never resolve it as a bind.
				if pasting {
					fwd = append(fwd, raw...)
					return
				}
				// Root (bind -n): fire a root bind, else forward the raw bytes.
				if name != "" {
					if ops := bd.ResolveRoot(name); ops != nil {
						runOps(ops)
						return
					}
				}
				fwd = append(fwd, raw...)
			}

			for i := 0; i < len(pass); i++ {
				b := pass[i]
				// Escape-sequence collector (Meta / CSI / SS3).
				if escStage != 0 {
					escRaw = append(escRaw, b)
					switch escStage {
					case 1: // b decides the sequence kind
						switch b {
						case '[':
							escStage = 2
						case 'O':
							escStage = 3
						default:
							escStage = 0
							name := "" // ESC + printable = Meta; else no token
							if b >= 0x20 && b <= 0x7e {
								name = "M-" + string(b)
							}
							finishEsc(name, "", escPrefixed, escRaw)
						}
					case 2: // CSI body until a final byte 0x40-0x7e
						if b >= 0x40 && b <= 0x7e {
							escStage = 0
							seq := string(escRaw[2:]) // bytes after "ESC ["
							// Modified keys (Ctrl+1, …) arrive as kitty CSI-u or xterm
							// modifyOtherKeys — decode to a bind token. When unbound: a
							// kitty key falls back to raw so a kitty app still gets it; a
							// modifyOtherKeys key is down-converted to its legacy bytes
							// (Alt+b -> ESC b, so readline motions survive) or dropped if
							// it has no legacy form, never forwarded as a garbage CSI.
							// ponytail: the drop/down-convert only triggers on the 27;m;c~
							// form; a terminal that emits modifyOtherKeys as CSI-u instead
							// would leak unbound exotic combos raw to a legacy pane. Low
							// severity (unbound exotic keys only); revisit if it bites.
							if tok, code, mods, form, ok := decodeExtKey(seq); ok && !pasting {
								raw := escRaw
								if form == formMOK {
									raw = mokLegacyBytes(code, mods)
								}
								finishEsc(tok, seq, escPrefixed, raw)
							} else {
								finishEsc(csiKeyName[seq], seq, escPrefixed, escRaw)
							}
						}
					case 3: // SS3: single final byte after "ESC O"
						escStage = 0
						finishEsc(ss3KeyName[string(b)], "", escPrefixed, escRaw)
					}
					continue
				}
				// Inside a bracketed paste: every byte is data. Forward it raw,
				// bypassing the prefix/bind machine below. An ESC still enters the
				// collector so the ESC[201~ terminator (or a pasted esc sequence) is
				// recognized; finishEsc forwards non-terminator esc data raw too.
				if pasting {
					if b == 0x1b {
						escStage, escPrefixed, escRaw = 1, false, []byte{b}
						continue
					}
					fwd = append(fwd, b)
					continue
				}
				// A custom key table (switch-client -T) claims the next key: look it
				// up there, revert to root first so the bind can chain into another
				// table. An unbound key in the table is simply swallowed (tmux does
				// the same — the table consumed the key). ESC-sequence keys in a
				// table aren't supported (byteKey("") → unbound).
				if curTable != "" {
					t := curTable
					curTable = ""
					runOps(bd.ResolveTable(t, byteKey(b)))
					continue
				}
				// Repeat window: a bare key resolves as if the prefix were held.
				// A repeatable bind extends the window; anything else ends it
				// and is reprocessed normally below.
				if repeatActive {
					if time.Now().Before(repeatDeadline) {
						if k := byteKey(b); k != "" {
							if ops := bd.Resolve(k); ops != nil {
								runOps(ops)
								if bd.Repeat[k] {
									repeatDeadline = time.Now().Add(repeatWindow)
								} else {
									repeatActive = false
								}
								continue
							}
						}
					}
					repeatActive = false
				}
				if prefixPending {
					prefixPending = false
					switch {
					case b == 0x1b:
						escStage, escPrefixed, escRaw = 1, true, []byte{b}
					case b == bd.Prefix || (bd.Prefix2 != 0 && b == bd.Prefix2):
						fwd = append(fwd, bd.Prefix) // prefix twice = literal prefix
					default:
						k := byteKey(b)
						runOps(bd.Resolve(k))
						if bd.Repeat[k] {
							repeatActive, repeatDeadline = true, time.Now().Add(repeatWindow)
						}
					}
					continue
				}
				if b == bd.Prefix || (bd.Prefix2 != 0 && b == bd.Prefix2) {
					prefixPending = true
					continue
				}
				if b == 0x1b {
					// Root escape sequence: collect, then bind-or-forward-raw.
					escStage, escPrefixed, escRaw = 1, false, []byte{b}
					continue
				}
				if k := byteKey(b); k != "" {
					if ops := bd.ResolveRoot(k); ops != nil { // tmux bind -n
						runOps(ops)
						continue
					}
				}
				fwd = append(fwd, b)
			}
			// A dangling root ESC at chunk end flushes raw, so a lone Escape isn't
			// held waiting on a key that may never arrive (apps need ESC promptly).
			// ponytail: root escape sequences are recognized only within one read
			// chunk — terminals deliver them atomically, so a bind only misses if
			// its sequence is split across reads (rare); passthrough stays correct
			// either way. A prefixed dangling ESC carries over (preserves the old
			// prefix+arrow behavior). Real disambiguation needs tmux's escape-time
			// timer — the knob to add if split sequences ever bite.
			if escStage != 0 && !escPrefixed {
				fwd = append(fwd, escRaw...)
				escStage, escRaw = 0, nil
			}
			if len(fwd) > 0 {
				send(&proto.ClientMsg{Input: &proto.Input{Data: fwd}})
			}
		}

		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				var pass []byte
				var mouseEvents []proto.MouseEvent
				for _, b := range buf[:n] {
					// While a paste is in flight, bypass the mouse parser: the payload
					// is data, and a pasted SGR-mouse-looking sequence would otherwise
					// be latched and eaten by mp.feed. The close-marker matcher ends it.
					if mousePasting {
						pass = append(pass, b)
						m, done := advancePaste(closeM, pasteClose, b)
						if done {
							mousePasting, closeM = false, 0
						} else {
							closeM = m
						}
						continue
					}
					consumed, flushed := mp.feed(b, func(cb, x, y int, press bool) {
						mouseEvents = append(mouseEvents, proto.MouseEvent{Cb: cb, X: x, Y: y, Press: press})
					})
					if consumed {
						pass = append(pass, flushed...)
					} else {
						pass = append(pass, b)
					}
					// Track the open marker on the RAW byte stream regardless of what
					// mp did with it, so paste starting mid-chunk still gates mp.feed.
					if m, done := advancePaste(openM, pasteOpen, b); done {
						mousePasting, openM = true, 0
					} else {
						openM = m
					}
				}
				pass = append(pass, mp.flush()...)
				// Mouse events run through the same overlay-vs-forward gate as
				// keys: copy-mode consumes them locally (scroll/drag-select), a
				// prompt/picker swallows them, and otherwise the client resolves
				// UI mouse (status-bar click → select-window) or forwards the
				// event for the server to decide (focus / border / app mouse).
				for _, me := range mouseEvents {
					compMu.Lock()
					switch {
					case comp != nil && comp.copy != nil:
						out, res := comp.copyMouse(me)
						if res.exit {
							// A drag-select yank exits copy-mode, same as the keyboard
							// path (compositor.copyFeed). Without this the stale overlay
							// swallows the user's next click — including the click meant
							// to focus another pane — so server focus never leaves the
							// copy-from pane and the subsequent paste lands there.
							comp.copy = nil
						}
						os.Stdout.Write(out)
						compMu.Unlock()
						if res.yank != "" {
							if comp.cfg.SetClipboard != "off" {
								os.Stdout.Write(encodeOSC52(res.yank))
							}
							send(&proto.ClientMsg{SetPaste: &proto.SetPasteBuffer{Text: res.yank, Pipe: true}})
						}
					case comp != nil && (comp.prompt != nil || comp.picker != nil || comp.modal != nil):
						compMu.Unlock() // overlay swallows mouse (modal is keyboard-only for now)
					default:
						// A left-press on a widget with an on_click runs it (like a
						// keybind) and consumes the click. Runs under compMu so the
						// query hooks read consistent compositor state.
						if comp != nil && me.Press && me.Cb&3 == 0 && me.Cb&0x20 == 0 {
							if b, fn, li, lt, cc := comp.clickWidget(me); fn != nil {
								ops := b.binds.RunClick(fn, li, lt, cc)
								// The handler may have mutated ui:state(); re-render this
								// widget and repaint now so the change is immediate,
								// instead of waiting for the next server tick.
								b.rerender()
								os.Stdout.Write(comp.redraw())
								compMu.Unlock()
								runOps(ops)
								continue
							}
						}
						var mr mouseResult
						rowOffset, colOffset := 0, 0
						if comp != nil {
							mr = comp.mouseAction(me)
							rowOffset, colOffset = comp.contentOffset(), comp.contentColOffset()
						} else {
							mr.forward = true
						}
						compMu.Unlock()
						for _, a := range mr.actions {
							dispatch(a)
						}
						if mr.border != nil {
							send(&proto.ClientMsg{ResizeBorder: mr.border})
						}
						if mr.copyDrag != nil {
							send(&proto.ClientMsg{CopyDrag: mr.copyDrag})
						}
						if mr.forward {
							// The server places panes in window space; the client's
							// screen space is shifted by a top status bar / top dock
							// (rows) and a left dock or frame inset (columns). Undo
							// BOTH before forwarding, or the app sees the click at
							// the wrong cell — the column half was missing, so any
							// left dock offset every click by its width.
							me.Y -= rowOffset
							me.X -= colOffset
							send(&proto.ClientMsg{Mouse: &me})
						}
					}
				}
				if len(pass) > 0 {
					// A client-owned overlay (copy-mode / prompt / picker)
					// intercepts keys locally; otherwise the prefix machine runs
					// binds and forwards the rest. All under compMu (shared with
					// decode), except sends, which release it first.
					compMu.Lock()
					switch {
					case comp != nil && comp.clock:
						// clock-mode swallows keys locally; any key dismisses it.
						comp.clock = false
						os.Stdout.Write(comp.redraw())
						compMu.Unlock()
					case comp != nil && comp.locked:
						// Lock swallows keys locally: any key unlocks, unless a
						// lock_password is set (then it must be typed + Enter).
						if comp.feedLock(pass) {
							comp.locked = false
						}
						os.Stdout.Write(comp.redraw())
						compMu.Unlock()
					case comp != nil && comp.popup != nil:
						// A display-popup grabs all input: forward straight to the
						// server (which writes it to the popup's PTY), bypassing the
						// prefix machine. The popup closes when its process exits.
						compMu.Unlock()
						send(&proto.ClientMsg{Input: &proto.Input{Data: pass}})
					case comp != nil && comp.modal != nil:
						// A modal keyboard widget grabs every key: decode the chunk to
						// key names, feed each to on_key, re-render once. Actions the
						// handler recorded (e.g. switch_session) run after.
						var mops []config.BindOp
						closed := false
						for _, name := range decodeKeys(pass) {
							o, cl := comp.modalKey(name)
							mops = append(mops, o...)
							if cl {
								closed = true
								break
							}
						}
						if closed {
							comp.modal = nil
						} else if comp.modal != nil {
							comp.modal.rerender()
						}
						os.Stdout.Write(comp.redraw())
						compMu.Unlock()
						runOps(mops)
					case comp != nil && comp.focusedDock != nil:
						// A focused dock grabs every key, like a modal, until its
						// on_key calls ui:close() to hand focus back to the pane.
						var dops []config.BindOp
						for _, name := range decodeKeys(pass) {
							o, cl := comp.dockKey(name)
							dops = append(dops, o...)
							if cl {
								comp.setDockFocus(comp.focusedDock, false)
								break
							}
						}
						if comp.focusedDock != nil {
							comp.focusedDock.rerender()
						}
						os.Stdout.Write(comp.redraw())
						compMu.Unlock()
						runOps(dops)
					case comp != nil && comp.copy != nil:
						// tmux behavior: the prefix key wins over copy-mode. A chunk
						// that starts with the prefix (or continues a pending prefix /
						// prefixed escape) runs through the normal bind machine, so
						// detach / select-pane / kill-session etc. still work while
						// browsing scrollback. Shadows copy-mode's own binding of the
						// prefix byte (e.g. C-b page-up in vi mode), same as tmux.
						if bd := curBinds(); prefixPending || (escStage != 0 && escPrefixed) ||
							pass[0] == bd.Prefix || (bd.Prefix2 != 0 && pass[0] == bd.Prefix2) {
							compMu.Unlock()
							processInput(pass)
							break
						}
						out, res := comp.copyFeed(pass)
						os.Stdout.Write(out)
						compMu.Unlock()
						if res.yank != "" {
							// Client owns the terminal: OSC52 sets the system
							// clipboard directly (unless set-clipboard off); SetPaste
							// keeps prefix+] working regardless.
							if comp.cfg.SetClipboard != "off" {
								os.Stdout.Write(encodeOSC52(res.yank))
							}
							send(&proto.ClientMsg{SetPaste: &proto.SetPasteBuffer{Text: res.yank, Pipe: true}})
						}
					case comp != nil && comp.prompt != nil:
						out, res := comp.promptFeed(pass)
						os.Stdout.Write(out)
						compMu.Unlock()
						if res.action != nil {
							dispatch(res.action)
						}
					case comp != nil && comp.picker != nil:
						out, res := comp.pickerFeed(pass)
						os.Stdout.Write(out)
						compMu.Unlock()
						if res.action != nil {
							dispatch(res.action)
						}
					default:
						compMu.Unlock()
						processInput(pass)
					}
				}
			}
			if err != nil {
				return
			}
		}
	}()

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		defer guardPanic(restoreTerm) // a crash here must not leave the pane wedged
		for range winch {
			cols, rows, err := term.GetSize(int(os.Stdout.Fd()))
			if err != nil {
				continue
			}
			// setPhysical first: the new width may cross a dock's min_cols
			// breakpoint, and the size we report must match what's visible.
			rc, rr := contentCols(cols, cliCfg.Widgets, cliCfg.PaneBorders), contentRows(rows, cols, cliCfg.StatusLines, cliCfg.Widgets, cliCfg.PaneBorders)
			var respAct []string
			compMu.Lock()
			if comp != nil {
				comp.setPhysical(cols, rows)
				rc, rr = comp.reportSize()
				respAct = comp.responsiveAction()
				os.Stdout.Write(comp.redraw())
			}
			compMu.Unlock()
			send(&proto.ClientMsg{Resize: &proto.Resize{Cols: rc, Rows: rr}})
			if respAct != nil {
				send(&proto.ClientMsg{Action: &proto.Action{Args: respAct}})
			}
		}
	}()

	target := session
	// create applies only to the first attach; the loop clears it on a
	// session-switch reconnect, which always targets an existing session.
	for {
		conn, err := net.Dial("unix", proto.SockPath())
		if err != nil {
			return fmt.Errorf("connect to gtmux server (is it running?): %w", err)
		}
		dec := gob.NewDecoder(conn)
		encMu.Lock()
		rawEnc = gob.NewEncoder(conn)
		encMu.Unlock()

		cols, rows, err := term.GetSize(int(os.Stdout.Fd()))
		if err != nil {
			cols, rows = 80, 24
		}
		// Send the client environment so the server can refresh its
		// update-environment vars into the session (server picks which).
		env := map[string]string{}
		for _, kv := range os.Environ() {
			if i := strings.IndexByte(kv, '='); i > 0 {
				env[kv[:i]] = kv[i+1:]
			}
		}
		if err := send(&proto.ClientMsg{Attach: &proto.Attach{Session: target, Cols: contentCols(cols, cliCfg.Widgets, cliCfg.PaneBorders), Rows: contentRows(rows, cols, cliCfg.StatusLines, cliCfg.Widgets, cliCfg.PaneBorders), Cwd: cwd, Create: create, GroupTarget: groupTarget, ReadOnly: readOnly, StatusCmds: serverCmds, StatusInterval: cliCfg.StatusInterval, Env: env, WantSnapshot: wantSnapshot(cliCfg.Widgets)}}); err != nil {
			conn.Close()
			return err
		}
		// OSC 2: set the terminal window title to the session name, mirroring
		// set-titles/set-titles-string in the user's tmux config.
		os.Stdout.Write([]byte(fmt.Sprintf("\x1b]2;%s\x07", target)))
		os.Stdout.Write([]byte("\x1b[2J\x1b[H"))

		// Fresh compositor per connection: pane IDs and layout belong to
		// the session just attached, not the previous one. Pop any extended-keys
		// kitty entry the old session pushed first (the exit defer won't — a
		// switch loops rather than returning), or it orphans on the outer terminal.
		compMu.Lock()
		if comp != nil {
			os.Stdout.Write(comp.restoreKitty())
		}
		comp = newCompositor()
		comp.cfg = cliCfg
		comp.setPhysical(cols, rows)
		comp.rebuildWidgets(curBinds()) // the widgets' fns live in this VM
		compMu.Unlock()
		switchTo := ""
		var decodeErr error
		// applyHookOps runs the ops a gtmux.on callback recorded (command-exited /
		// program-changed / alerts): a pane:set_border override on the compositor,
		// or a run_command/action dispatched like a keybind. runOps proper lives in
		// the input goroutine; this is its read-loop-scoped counterpart.
		applyHookOps := func(ops []config.BindOp) {
			for _, op := range ops {
				if op.Border != nil {
					compMu.Lock()
					if comp != nil {
						os.Stdout.Write(comp.setPaneBorder(op.Border.PaneID, op.Border.Color))
					}
					compMu.Unlock()
				} else if op.Command != "" {
					if argv := tokenize(op.Command); len(argv) > 0 {
						dispatch(argv)
					}
				} else if len(op.Action) > 0 {
					dispatch(op.Action)
				}
			}
		}
		for {
			var msg proto.ServerMsg
			if err := dec.Decode(&msg); err != nil {
				if err != io.EOF {
					decodeErr = err
				}
				break
			}
			if msg.Ack != nil && !msg.Ack.Ok {
				// Attach rejected (no such session / duplicate): abort; the
				// deferred term.Restore runs and main prints this to stderr.
				conn.Close()
				return fmt.Errorf("%s", msg.Ack.Err)
			}
			if msg.SwitchSession != "" {
				// The server closes the connection right after this;
				// remember where to reattach and drain to EOF.
				switchTo = msg.SwitchSession
				continue
			}
			if msg.SetOption != nil {
				// A client option set elsewhere (scripting / another client):
				// apply it live like a local :set. applyOverride is no-routing,
				// so an unknown name is a harmless no-op (never bounces back).
				applyOverride(msg.SetOption.Name, msg.SetOption.Value)
				continue
			}
			if msg.Passthrough != nil {
				// allow-passthrough: raw bytes from an app in a pane, written
				// straight to our terminal (bypassing the compositor) — e.g. an
				// OSC 52 clipboard set aimed at the outer terminal.
				os.Stdout.Write(msg.Passthrough)
				continue
			}
			if len(msg.Clipboards) > 0 {
				// An app in a pane set the clipboard (OSC 52): re-emit to the
				// outer terminal, same gate as a copy-mode yank.
				if comp.cfg.SetClipboard != "off" {
					for _, text := range msg.Clipboards {
						os.Stdout.Write(encodeOSC52(text))
					}
				}
				continue
			}
			if len(msg.CommandExits) > 0 {
				// A command finished in a pane (OSC 133): fire gtmux.on("command-exited")
				// with a pane object; its ops (set_border / run_command / action) apply.
				for _, ce := range msg.CommandExits {
					applyHookOps(curBinds().RunCommandExit(config.CommandExitEvent{
						Session: ce.Session, Window: ce.Window, PaneID: ce.PaneID, ExitCode: ce.ExitCode,
					}))
				}
				continue
			}
			if msg.ClientAction != nil {
				// A hook fired a client-owned command (command-prompt / display-menu
				// / confirm-before): open the overlay locally. dispatch handles its
				// own locking; it never routes these back to the server.
				dispatch(msg.ClientAction)
				continue
			}
			compMu.Lock()
			out := comp.apply(&msg)
			respAct := comp.responsiveAction()
			alerts := comp.drainAlerts()
			progChanges := comp.drainProgramChanges()
			agentChanges := comp.drainAgentChanges()
			compMu.Unlock()
			os.Stdout.Write(out)
			if respAct != nil {
				// responsive maximize: auto-(un)zoom on a breakpoint crossing.
				// Sent outside compMu like any other action.
				send(&proto.ClientMsg{Action: &proto.Action{Args: respAct}})
			}
			// Fire gtmux.on callbacks outside compMu (Run* take vmMu). A callback
			// most often notifies via os.execute (zero ops); any set_border /
			// run_command / action it records applies like a keybind.
			for _, ev := range alerts {
				applyHookOps(curBinds().RunAlert(ev))
			}
			for _, pc := range progChanges {
				applyHookOps(curBinds().RunProgramChanged(pc.session, pc.window, pc.pane, pc.command, pc.from))
			}
			for _, ac := range agentChanges {
				applyHookOps(curBinds().RunAgentState(ac.session, ac.window, ac.pane, ac.command, ac.state))
			}
		}
		conn.Close()
		if switchTo == "" {
			os.Stdout.Write([]byte("\x1b[2J\x1b[H"))
			return decodeErr
		}
		target = switchTo
		create = false
	}
}
