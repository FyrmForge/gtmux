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
	defer term.Restore(int(os.Stdin.Fd()), oldState)

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
		os.Stdout.Write([]byte("\x1b[?1002h\x1b[?1006h"))
		defer os.Stdout.Write([]byte("\x1b[?1002l\x1b[?1006l"))
	}

	go func() {
		buf := make([]byte, 4096)
		var mp mouseParser
		// Prefix state machine (client owns input). Persist across reads: a
		// prefix key and its follow byte can land in separate Stdin.Read chunks.
		prefixPending := false
		curTable := "" // active custom key table (tmux switch-client -T); one key, then reverts
		escStage := 0  // 0 none, 1 saw ESC after prefix, 2 collecting CSI
		var csiBuf []byte
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
			for i := 0; i < len(pass); i++ {
				b := pass[i]
				if escStage == 1 {
					if b == '[' {
						escStage, csiBuf = 2, nil
					} else {
						escStage = 0
					}
					continue
				}
				if escStage == 2 {
					if b < 0x40 || b > 0x7e {
						csiBuf = append(csiBuf, b)
						continue
					}
					escStage = 0
					seq := string(append(csiBuf, b))
					csiBuf = nil
					switch {
					case len(seq) == 1 && arrowFlag[seq[0]] != "":
						runOps([]config.BindOp{{Action: []string{"select-pane", arrowFlag[seq[0]]}}})
					case len(seq) == 4 && seq[:3] == "1;5" && arrowFlag[seq[3]] != "":
						runOps([]config.BindOp{{Action: []string{"resize-pane", arrowFlag[seq[3]], "1"}}})
					case len(seq) == 4 && seq[:3] == "1;3" && arrowFlag[seq[3]] != "":
						runOps([]config.BindOp{{Action: []string{"resize-pane", arrowFlag[seq[3]], "5"}}})
					case seq == "5~": // prefix+PgUp: copy-mode a page up
						runOps([]config.BindOp{{Action: []string{"copy-mode", "-u"}}})
					}
					continue
				}
				// A custom key table (switch-client -T) claims the next key: look it
				// up there, revert to root first so the bind can chain into another
				// table. An unbound key in the table is simply swallowed (tmux does
				// the same — the table consumed the key).
				if curTable != "" {
					t := curTable
					curTable = ""
					runOps(bd.ResolveTable(t, b))
					continue
				}
				// Repeat window: a bare key resolves as if the prefix were held.
				// A repeatable bind extends the window; anything else ends it
				// and is reprocessed normally below.
				if repeatActive {
					if time.Now().Before(repeatDeadline) {
						if ops := bd.Resolve(b); ops != nil {
							runOps(ops)
							if bd.Repeat[b] {
								repeatDeadline = time.Now().Add(repeatWindow)
							} else {
								repeatActive = false
							}
							continue
						}
					}
					repeatActive = false
				}
				if prefixPending {
					prefixPending = false
					switch {
					case b == 0x1b:
						escStage = 1
					case b == bd.Prefix:
						fwd = append(fwd, b) // prefix twice = literal prefix
					default:
						runOps(bd.Resolve(b))
						if bd.Repeat[b] {
							repeatActive, repeatDeadline = true, time.Now().Add(repeatWindow)
						}
					}
					continue
				}
				if b == bd.Prefix {
					prefixPending = true
					continue
				}
				if ops := bd.ResolveRoot(b); ops != nil { // tmux bind -n
					runOps(ops)
					continue
				}
				fwd = append(fwd, b)
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
