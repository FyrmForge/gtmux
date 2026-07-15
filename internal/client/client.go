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

// Attach is `gtmux attach [-r]`: read-only when ro is set.
func Attach(session string, ro bool) error { return RunGroup(session, false, "", ro) }

// RunGroup is Run with a group target: new-session -t <groupTarget> joins that
// session's group (displays its current windows). readOnly is attach -r.
func RunGroup(session string, create bool, groupTarget string, readOnly bool) error {
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
			os.Stdout.Write([]byte("\x1b[?1002l\x1b[?1006l")) // disable mouse (no-op if off)
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
		bindsPtr.Store(newBinds)
		compMu.Lock()
		if comp != nil {
			os.Stdout.Write(comp.reload(newCfg))
		}
		compMu.Unlock()
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

	go func() {
		defer guardPanic(restoreTerm) // a crash here must not leave the pane wedged
		buf := make([]byte, 4096)
		var mp mouseParser
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
				} else if len(op.Action) > 0 {
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
							finishEsc(csiKeyName[seq], seq, escPrefixed, escRaw)
						}
					case 3: // SS3: single final byte after "ESC O"
						escStage = 0
						finishEsc(ss3KeyName[string(b)], "", escPrefixed, escRaw)
					}
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
					consumed, flushed := mp.feed(b, func(cb, x, y int, press bool) {
						mouseEvents = append(mouseEvents, proto.MouseEvent{Cb: cb, X: x, Y: y, Press: press})
					})
					if consumed {
						pass = append(pass, flushed...)
						continue
					}
					pass = append(pass, b)
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
						os.Stdout.Write(out)
						compMu.Unlock()
						if res.yank != "" {
							if comp.cfg.SetClipboard != "off" {
								os.Stdout.Write(encodeOSC52(res.yank))
							}
							send(&proto.ClientMsg{SetPaste: &proto.SetPasteBuffer{Text: res.yank, Pipe: true}})
						}
					case comp != nil && (comp.prompt != nil || comp.picker != nil):
						compMu.Unlock() // overlay swallows mouse
					default:
						var mr mouseResult
						offset := 0
						if comp != nil {
							mr = comp.mouseAction(me)
							offset = comp.contentOffset()
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
							// The server places panes in window-row space; with the
							// status bar at the top the client's rows are shifted down,
							// so undo that before forwarding the event.
							me.Y -= offset
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
					case comp != nil && comp.copy != nil:
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
			send(&proto.ClientMsg{Resize: &proto.Resize{Cols: cols, Rows: rows, StatusLines: cliCfg.StatusLines}})
			compMu.Lock()
			if comp != nil {
				comp.setPhysical(cols, rows)
				os.Stdout.Write(comp.redraw())
			}
			compMu.Unlock()
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
		if err := send(&proto.ClientMsg{Attach: &proto.Attach{Session: target, Cols: cols, Rows: rows, Cwd: cwd, Create: create, GroupTarget: groupTarget, ReadOnly: readOnly, StatusCmds: serverCmds, StatusInterval: cliCfg.StatusInterval, StatusLines: cliCfg.StatusLines, Env: env}}); err != nil {
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
		compMu.Unlock()
		switchTo := ""
		var decodeErr error
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
			if msg.ClientAction != nil {
				// A hook fired a client-owned command (command-prompt / display-menu
				// / confirm-before): open the overlay locally. dispatch handles its
				// own locking; it never routes these back to the server.
				dispatch(msg.ClientAction)
				continue
			}
			compMu.Lock()
			out := comp.apply(&msg)
			compMu.Unlock()
			os.Stdout.Write(out)
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
