// Package config loads gtmux's Lua configuration files: server.lua (prefix
// key, keybindings) and client.lua (chrome colors, mouse on/off). Both are
// optional — a missing or broken user file just means defaults apply.
package config

import (
	_ "embed"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	lua "github.com/yuin/gopher-lua"
)

//go:embed default_server.lua
var defaultServerLua string

// WriteDefaultServer writes the embedded default server.lua to the user's
// config path. See writeConfig for the overwrite/force semantics.
func WriteDefaultServer(force bool) (string, error) {
	return writeConfig(ServerConfigPath(), defaultServerLua, force)
}

// writeConfig writes content to path (creating the directory), returning that
// path. It refuses to overwrite an existing file so a user's own edits are
// never clobbered — unless force is set.
func writeConfig(path, content string, force bool) (string, error) {
	if path == "" {
		return "", fmt.Errorf("cannot determine config directory")
	}
	if _, err := os.Stat(path); err == nil && !force {
		return path, fmt.Errorf("config already exists: %s (--force to overwrite)", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, []byte(content), 0644)
}

// ServerConfig is the server's loaded options. Input (prefix + keybinds) and
// the status bar (formats + colors) are client-side now; the only server-side
// option left is the session auto-name template.
//
// SessionName is the fmt template the server uses to auto-name a session
// created by a bare `attach` (no name given). Must contain %d; defaults to
// tmux's numeric "%d" (sessions 0, 1, 2, …).
type ServerConfig struct {
	SessionName string
	// MainPaneWidth/Height size the main pane in the main-vertical /
	// main-horizontal layouts (tmux's options of the same name); the rest
	// share the leftover space. Default to tmux's 80/24.
	MainPaneWidth  int
	MainPaneHeight int
	// BaseIndex/PaneBaseIndex offset the displayed (and targeted) window/pane
	// numbers. tmux's compiled-in default is 0; the shipped default_server.lua
	// sets 1 (gtmux's opinionated default, like a starter .tmux.conf).
	BaseIndex     int
	PaneBaseIndex int
	// HistoryLimit is the scrollback line cap per pane (tmux's history-limit),
	// applied at pane creation. Read once at startup (config-time only, no
	// runtime set-option). Default is tmux's 2000; shipped default_server.lua
	// bumps it to 5000. DisplayTime is how long a transient status message
	// (run-shell output, command errors) stays up, in ms; tmux's default is
	// 750, the shipped lua sets 3000.
	HistoryLimit int
	DisplayTime  int
	// MessageLimit caps the per-session message log show-messages prints
	// (tmux's message-limit); default 1000.
	MessageLimit int
	// AutomaticRename recomputes a window's name from AutomaticRenameFormat on
	// each status build (tmux's automatic-rename, default on). AllowRename lets a
	// pane's OSC 0/2 title escape rename the window (tmux's allow-rename, default
	// off). A manual rename-window always overrides both.
	AutomaticRename       bool
	AutomaticRenameFormat string
	AllowRename           bool
	// DestroyUnattached destroys a session as soon as its last client detaches
	// (default off). DetachOnDestroy detaches a client when its session is killed
	// (default on); off switches it to another session instead.
	DestroyUnattached bool
	DetachOnDestroy   bool
	// VisualActivity/VisualBell show a status-line message on an activity/bell
	// alert (tmux's visual-activity / visual-bell; default off).
	VisualActivity bool
	VisualBell     bool
	// PaneBorderStatus (off/top/bottom) reserves a row per pane for a label drawn
	// from PaneBorderFormat (tmux's pane-border-status / pane-border-format).
	PaneBorderStatus string
	PaneBorderFormat string
	// WindowSize picks the grid size when clients of different sizes share a
	// session: "latest" (default — follow the most recent/acting client, tmux's
	// default), "smallest", or "largest".
	WindowSize string
	// AggressiveResize (tmux's aggressive-resize, default off) sizes a shared
	// window only to the sessions where it is the CURRENT window, rather than
	// every session it's linked into.
	AggressiveResize bool
	// SynchronizePanes (tmux's synchronize-panes, default off) mirrors client
	// input to every pane in a window at once. A window option; this is the
	// session-wide default, overridable per window via setw.
	SynchronizePanes bool
	// RemainOnExit (tmux's remain-on-exit): "off" (default) closes a pane when
	// its process exits; "on" keeps it as a dead pane; "failed" keeps it only on
	// a non-zero exit. A window option; session-wide default, per-window setw.
	RemainOnExit string
	// CopyCommand (tmux's copy-command): shell command a copy-mode yank pipes the
	// selection to (stdin). Empty (default) = no pipe; the client still sets the
	// system clipboard via OSC 52 and the server's paste buffer regardless.
	CopyCommand string
	// FocusEvents (tmux's focus-events, default off): when on, gtmux sends
	// focus-in/out escapes to a pane that requested them (DECSET 1004) as it
	// gains/loses the active-pane focus, so apps like vim see focus changes.
	FocusEvents bool
	// AllowPassthrough (tmux's allow-passthrough, default off): when on, an app in
	// a pane may emit an ESC Ptmux;<payload> ESC \ DCS whose un-doubled payload is
	// forwarded raw to the attached client's real terminal (e.g. to reach the
	// outer terminal's clipboard). Off strips the wrapper and drops the payload.
	AllowPassthrough bool
	// ExitEmpty (tmux's exit-empty, default on): the server process exits once
	// its last session closes. Off keeps the daemon alive with zero sessions.
	ExitEmpty bool
	// DefaultShell (tmux's default-shell): the shell binary new panes run. Empty
	// (default) uses $SHELL, then /bin/sh. DefaultCommand (tmux's default-command):
	// run via `shell -c` for a new pane with no explicit command; empty (default)
	// runs the shell as a login shell. Config-time, like history-limit.
	DefaultShell, DefaultCommand string
	// BellAction/ActivityAction (tmux's bell-action/activity-action) scope which
	// windows raise an alert: "any", "none", "current", or "other". tmux defaults
	// are bell-action "any" and activity-action "other".
	BellAction, ActivityAction string
	// UpdateEnvironment (tmux's update-environment) is the list of variables
	// refreshed from an attaching client's environment into the session env on
	// attach/reattach. A listed var absent from the client is removed from the
	// session env (tmux's unset semantics). Defaults to tmux's built-in list.
	UpdateEnvironment []string
	// Hooks maps a hook name (e.g. "after-new-window") to the commands to run
	// when that event fires. Seeded globally here; each session gets its own
	// copy so a runtime set-hook stays session-local (no shared mutable state).
	Hooks map[string][]string
}

// DefaultUpdateEnvironment is tmux's built-in update-environment list.
func DefaultUpdateEnvironment() []string {
	return []string{
		"DISPLAY", "KRB5CCNAME", "SSH_ASKPASS", "SSH_AUTH_SOCK",
		"SSH_AGENT_PID", "SSH_CONNECTION", "WINDOWID", "XAUTHORITY",
	}
}

// ServerConfigPath returns ~/.config/gtmux/server.lua, or "" if the user's
// config directory can't be determined.
func ServerConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "gtmux", "server.lua")
}

// parseKeyByte turns a prefix key string into its single input byte: "C-x" for
// a control code or a lone character. Prefix keys are always single bytes
// (C-b, C-a, backtick, ...), so this stays byte-valued while binds went
// canonical-string (parseKeyName).
func parseKeyByte(s string) (byte, bool) {
	if len(s) == 3 && s[0] == 'C' && s[1] == '-' {
		return s[2] & 0x1f, true
	}
	if len(s) == 1 {
		return s[0], true
	}
	return 0, false
}

// namedKeys maps a config key name (and common aliases) to the canonical token
// the client's input reader also produces from raw bytes, so a bind and a
// keystroke compare equal. Kept here (not the client package) because parsing
// is config-side; the reader must produce the same right-hand values.
var namedKeys = map[string]string{
	"Up": "Up", "Down": "Down", "Left": "Left", "Right": "Right",
	"Home": "Home", "End": "End",
	"Insert": "Insert", "Ins": "Insert", "Delete": "Delete", "Del": "Delete",
	"PgUp": "PgUp", "PageUp": "PgUp", "PPage": "PgUp",
	"PgDn": "PgDn", "PageDown": "PgDn", "NPage": "PgDn",
	"F1": "F1", "F2": "F2", "F3": "F3", "F4": "F4", "F5": "F5", "F6": "F6",
	"F7": "F7", "F8": "F8", "F9": "F9", "F10": "F10", "F11": "F11", "F12": "F12",
}

// parseKeyName canonicalizes a bind key string into the token the client's
// input reader also produces from raw bytes, so binds match. Forms: a lone
// char ("a"), a control key ("C-a", folded like the reader), Meta ("M-h",
// case-sensitive), a function/named key (see namedKeys), or the folding
// aliases Space/Tab/Enter/BSpace. "" if unrecognized.
func parseKeyName(s string) (string, bool) {
	switch s {
	case "Space":
		return " ", true
	case "Tab":
		return "C-i", true // Tab folds onto C-i, matching the 0x09 byte
	case "Enter", "Return":
		return "C-m", true // Enter folds onto C-m (0x0d)
	case "BSpace", "Backspace":
		return "BSpace", true
	}
	if c, ok := namedKeys[s]; ok {
		return c, true
	}
	// C-x: fold to the control byte like the reader (0x01-0x1a -> "C-a".."C-z",
	// 0x1c-0x1f -> "C-\\","C-]","C-^","C-_"). C-[ folds to ESC (0x1b), which is
	// the escape-sequence lead and never matches a bind — unbindable, as it
	// effectively was before too.
	if len(s) == 3 && s[0] == 'C' && s[1] == '-' {
		b := s[2] & 0x1f
		if b >= 0x01 && b <= 0x1a {
			return "C-" + string(rune('a'+b-1)), true
		}
		if b >= 0x1c && b <= 0x1f {
			return "C-" + string(rune(b|0x40)), true
		}
	}
	// M-x: any single following char, case preserved (M-h vs M-H = Shift).
	if len(s) == 3 && s[0] == 'M' && s[1] == '-' {
		return "M-" + string(s[2]), true
	}
	if len(s) == 1 {
		return s, true
	}
	return "", false
}

// LoadServer reads the server's options from the bundled defaults, then the
// user's server.lua (if any) layered on top. A throwaway VM: no bound
// functions outlive the load.
func LoadServer(userPath string) ServerConfig {
	L := lua.NewState()
	defer L.Close()
	cfg := ServerConfig{SessionName: "%d", MainPaneWidth: 80, MainPaneHeight: 24, HistoryLimit: 2000, DisplayTime: 750, MessageLimit: 1000, AutomaticRename: true, AutomaticRenameFormat: "#{pane_command}", DetachOnDestroy: true, PaneBorderStatus: "off", PaneBorderFormat: "#{pane_index} #{pane_command}", WindowSize: "latest", RemainOnExit: "off", UpdateEnvironment: DefaultUpdateEnvironment(), ExitEmpty: true, BellAction: "any", ActivityAction: "other", Hooks: map[string][]string{}}

	tbl := L.NewTable()
	L.SetField(tbl, "set_option", L.NewFunction(func(l *lua.LState) int {
		// The server-side options; status_* / prefix are the client's and are
		// ignored here.
		switch l.CheckString(1) {
		case "session_name":
			value := l.CheckString(2)
			// Must contain %d so auto-naming produces distinct names and the
			// registry's lowest-unused loop is guaranteed to terminate.
			if strings.Contains(value, "%d") {
				cfg.SessionName = value
			} else {
				log.Printf("gtmux: session_name %q must contain %%d; ignoring", value)
			}
		case "main_pane_width":
			cfg.MainPaneWidth = l.CheckInt(2)
		case "main_pane_height":
			cfg.MainPaneHeight = l.CheckInt(2)
		case "window_size":
			if v := l.CheckString(2); v == "latest" || v == "smallest" || v == "largest" || v == "manual" {
				cfg.WindowSize = v
			}
		case "aggressive_resize":
			cfg.AggressiveResize = l.CheckBool(2)
		case "synchronize_panes":
			cfg.SynchronizePanes = l.CheckBool(2)
		case "remain_on_exit":
			cfg.RemainOnExit = l.CheckString(2)
		case "copy_command":
			cfg.CopyCommand = l.CheckString(2)
		case "focus_events":
			cfg.FocusEvents = l.CheckBool(2)
		case "allow_passthrough":
			cfg.AllowPassthrough = l.CheckBool(2)
		case "exit_empty":
			cfg.ExitEmpty = l.CheckBool(2)
		case "default_shell":
			cfg.DefaultShell = l.CheckString(2)
		case "default_command":
			cfg.DefaultCommand = l.CheckString(2)
		case "bell_action":
			cfg.BellAction = l.CheckString(2)
		case "activity_action":
			cfg.ActivityAction = l.CheckString(2)
		case "update_environment":
			// Whitespace-separated list (tmux stores an array; a string is
			// simpler for Lua config). Empty string clears the list.
			cfg.UpdateEnvironment = strings.Fields(l.CheckString(2))
		case "base_index":
			if n := l.CheckInt(2); n >= 0 {
				cfg.BaseIndex = n
			}
		case "pane_base_index":
			if n := l.CheckInt(2); n >= 0 {
				cfg.PaneBaseIndex = n
			}
		case "history_limit":
			if n := l.CheckInt(2); n >= 0 {
				cfg.HistoryLimit = n
			}
		case "display_time":
			if n := l.CheckInt(2); n > 0 {
				cfg.DisplayTime = n
			}
		case "message_limit":
			if n := l.CheckInt(2); n >= 0 {
				cfg.MessageLimit = n
			}
		case "automatic_rename":
			cfg.AutomaticRename = l.CheckBool(2)
		case "automatic_rename_format":
			cfg.AutomaticRenameFormat = l.CheckString(2)
		case "allow_rename":
			cfg.AllowRename = l.CheckBool(2)
		case "destroy_unattached":
			cfg.DestroyUnattached = l.CheckBool(2)
		case "detach_on_destroy":
			cfg.DetachOnDestroy = l.CheckBool(2)
		case "visual_activity":
			cfg.VisualActivity = l.CheckBool(2)
		case "visual_bell":
			cfg.VisualBell = l.CheckBool(2)
		case "pane_border_status":
			if v := l.CheckString(2); v == "off" || v == "top" || v == "bottom" {
				cfg.PaneBorderStatus = v
			}
		case "pane_border_format":
			cfg.PaneBorderFormat = l.CheckString(2)
		}
		return 0
	}))
	// set_hook(name, cmd): register a command to run when the event fires.
	// Replaces any previous command for that name (tmux set-hook default).
	L.SetField(tbl, "set_hook", L.NewFunction(func(l *lua.LState) int {
		cfg.Hooks[l.CheckString(1)] = []string{l.CheckString(2)}
		return 0
	}))
	L.SetGlobal("gtmux", tbl)

	if err := L.DoString(defaultServerLua); err != nil {
		log.Fatalf("gtmux: embedded default server config is broken: %v", err)
	}
	if data, err := os.ReadFile(userPath); err == nil {
		if err := L.DoString(string(data)); err != nil {
			log.Printf("gtmux: %s: %v (ignoring, using defaults)", userPath, err)
		}
	}
	return cfg
}
