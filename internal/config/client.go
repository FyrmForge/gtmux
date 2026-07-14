package config

import (
	_ "embed"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	lua "github.com/yuin/gopher-lua"

	"github.com/FyrmForge/gtmux/internal/emu"
)

//go:embed default_client.lua
var defaultClientLua string

// WriteDefaultClient writes the embedded default client.lua to the user's
// config path. See writeConfig for the overwrite/force semantics.
func WriteDefaultClient(force bool) (string, error) {
	return writeConfig(ClientConfigPath(), defaultClientLua, force)
}

// ClientConfig is static chrome options resolved once at attach — no
// callbacks, just colors and toggles for what the compositor draws, plus the
// status_left/status_right format strings (the client owns and expands them).
type ClientConfig struct {
	Mouse                            bool
	ExtendedKeys                     bool // negotiate the kitty keyboard protocol with the outer terminal when a pane app requests it (tmux's extended-keys)
	StatusFG, StatusBG               emu.Color
	ActiveWindowFG, ActiveWindowBG   emu.Color
	ActiveBorderFG                   emu.Color
	MarkedBorderFG                   emu.Color
	InactiveBorderFG                 emu.Color // pane-border-style: inactive pane dividers (fg)
	InactiveBorderBG                 emu.Color // pane-border-style: inactive pane dividers (bg)
	FillFG                           emu.Color
	CopyCursorFG, CopyCursorBG       emu.Color
	CopySelectionFG, CopySelectionBG emu.Color
	MessageFG, MessageBG             emu.Color // message-style: transient status messages + command prompt

	StatusAttr         int16 // status-style attributes (bold/reverse/italics) for the bar
	MessageAttr        int16 // message-style attributes
	InactiveBorderAttr int16 // pane-border-style attributes for inactive dividers
	StatusLeftLength   int   // status-left-length: max cells for status-left (0 = unlimited)
	StatusRightLength  int   // status-right-length: max cells for status-right (0 = unlimited)
	StatusLines        int   // tmux `status` 1..5: number of status rows (the client reserves them)
	// ExtraStatusFormats are the formats for status lines 2..5 (index 0 = line 2),
	// each expanded and drawn full-width. status-format[0] (line 1) is the normal
	// bar (status-left + window list + status-right); these are the extra lines.
	ExtraStatusFormats [4]string
	StatusLeft         string
	StatusRight        string
	StatusInterval     int    // #client()/#server() cache cadence, seconds
	ModeKeys           string // copy-mode keytable: "vi" (default) or "emacs"
	StatusKeys         string // command-prompt editing: "emacs" (default; C-u/C-w kill keys) or "vi" (plain — no modal editing, ESC cancels)
	SetClipboard       string // tmux set-clipboard: "external"/"on" (default; copy-mode yank emits OSC 52 to the outer clipboard) or "off"
	// Copy-mode mouse behavior. In tmux these are copy-mode-vi keybinds
	// (WheelUp/Down `send -N<n> -X scroll`, MouseDragEnd `copy-selection-and-cancel`);
	// gtmux has no copy-mode keytable, so they're options.
	CopyWheelLines  int  // lines scrolled per wheel notch in copy-mode (default 3)
	CopyDragFinish  bool // release after a drag yanks + exits (tmux default true); false = selection persists (tmux's `unbind MouseDragEnd1Pane`)
	WordSeparators     string // tmux word-separators: chars (besides whitespace) that bound copy-mode w/b/e words
	LockPassword       string // lock overlay: typed password to unlock; "" = any key dismisses
	RepeatTime         int    // tmux repeat-time: bare-key repeat window after a -r bind, ms
	SetTitles          bool   // tmux set-titles: emit OSC 0/2 to set the outer terminal's title
	SetTitlesString    string // format for the outer-terminal title when SetTitles is on
	// Status-bar layout (tmux status-position / status-justify) and the
	// per-window-entry formats (window-status-format / -current-format, joined
	// by window-status-separator). The entry formats are expanded client-side
	// against each window's vars, replacing a hardcoded label.
	StatusJustify             string // "left" (default) | "centre" | "right"
	StatusPosition            string // "bottom" (default) | "top"
	WindowStatusSeparator     string
	WindowStatusFormat        string
	WindowStatusCurrentFormat string
}

func DefaultClientConfig() ClientConfig {
	return ClientConfig{
		Mouse:            true,
		StatusFG:         emu.White,
		StatusBG:         emu.DarkGrey,
		ActiveWindowFG:   emu.Black,
		ActiveWindowBG:   emu.Green,
		ActiveBorderFG:   emu.Green,
		MarkedBorderFG:   emu.Magenta,
		InactiveBorderFG: emu.DarkGrey, // matches the previously-hardcoded inactive border color
		InactiveBorderBG: emu.DefaultBG,
		FillFG:           emu.DarkGrey,
		CopyCursorFG:     emu.Black,
		CopyCursorBG:     emu.Yellow,
		CopySelectionFG:  emu.Black,
		CopySelectionBG:  emu.LightCyan,
		MessageFG:        emu.Black, // tmux message-style default: black on yellow
		MessageBG:        emu.Yellow,
		// status-left/right-length: 0 = unlimited. Diverges from tmux (10/40) on
		// purpose — gtmux's default status-left is longer than 10, so tmux's cap
		// would truncate the default bar. The knob is still exposed.
		StatusLeft:     "[#{host}][#{session}]",
		StatusRight:    "#{?git_branch,[git:#{git_branch}] ,}#{clock}",
		StatusInterval: 15,
		StatusLines:    1,
		ModeKeys:       "vi",
		StatusKeys:     "emacs",    // tmux default
		SetClipboard:   "external", // tmux default
		CopyWheelLines: 3,          // tmux copy-mode default
		CopyDragFinish: true,       // tmux default: drag-release yanks + cancels
		WordSeparators: "!\"#$%&'()*+,-./:;<=>?@[\\]^\x60{|}~", // tmux default: all ASCII punctuation

		RepeatTime:      500,
		SetTitlesString: "#{session}:#{window_index}:#{window_name}",

		StatusJustify:             "left",
		StatusPosition:            "bottom",
		WindowStatusSeparator:     " ",
		WindowStatusFormat:        "#{window_index}:#{window_name}#{window_flags}",
		WindowStatusCurrentFormat: "#{window_index}:#{window_name}#{window_flags}",
	}
}

// IsClientOption reports whether name is a client-owned option (so a runtime
// set-option handles it locally rather than forwarding to the server). It's the
// name→owner test the client routes on; applyOption returns true for any known
// name regardless of the value's validity, so a throwaway apply is enough.
func IsClientOption(name string) bool {
	var cfg ClientConfig
	binds := &ClientBinds{}
	return applyOption(&cfg, binds, name, "")
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// splitFields splits a command-prompt template like a shell: whitespace
// separates, '…'/"…" group (spaces inside stay in one field), \x escapes the
// next byte. Unlike strings.Fields it keeps a quoted nested command whole.
func splitFields(s string) []string {
	var out []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(s); {
		switch c := s[i]; c {
		case ' ', '\t':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
			i++
		case '\'':
			inWord = true
			for i++; i < len(s) && s[i] != '\''; i++ {
				cur.WriteByte(s[i])
			}
			i++
		case '"':
			inWord = true
			for i++; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				cur.WriteByte(s[i])
			}
			i++
		case '\\':
			inWord = true
			if i++; i < len(s) {
				cur.WriteByte(s[i])
				i++
			}
		default:
			inWord = true
			cur.WriteByte(c)
			i++
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out
}

// applyOption sets one client option by name, returning false if the name isn't
// a known client option (so a caller can route it elsewhere — e.g. a server
// option). This is the single registry of client options: the Lua loader
// (set_option + the gtmux.options.* readout) and runtime set-option both go
// through it, so a new option is added in exactly one place.
func applyOption(cfg *ClientConfig, binds *ClientBinds, name, value string) bool {
	setColor := func(dst *emu.Color) {
		if c, ok := colorNames[value]; ok {
			*dst = c
		}
	}
	switch name {
	case "prefix":
		if b, ok := parseKey(value); ok {
			binds.Prefix = b
		}
	case "status_left":
		cfg.StatusLeft = value
	case "status_right":
		cfg.StatusRight = value
	case "status_interval":
		if n, err := strconv.Atoi(value); err == nil && n > 0 {
			cfg.StatusInterval = n
		}
	case "mouse":
		cfg.Mouse = value == "true" || value == "1"
	case "extended_keys":
		// tmux accepts off/on/always; gtmux only needs on-vs-off (it negotiates
		// with the outer terminal only while a pane app requests kitty).
		cfg.ExtendedKeys = value == "on" || value == "always" || value == "true" || value == "1"
	case "mode_keys":
		if value == "vi" || value == "emacs" {
			cfg.ModeKeys = value
		}
	case "status_keys":
		if value == "vi" || value == "emacs" {
			cfg.StatusKeys = value
		}
	case "set_clipboard":
		// tmux off/external/on; gtmux only distinguishes off (no OSC 52 on yank)
		// from on (external/on both emit). "1"/"true" = on for lua-bool configs.
		switch value {
		case "off", "false", "0":
			cfg.SetClipboard = "off"
		default:
			cfg.SetClipboard = value
		}
	case "copy_wheel_lines":
		if n, err := strconv.Atoi(value); err == nil && n > 0 {
			cfg.CopyWheelLines = n
		}
	case "copy_drag_finish":
		cfg.CopyDragFinish = value == "true" || value == "1" || value == "on"
	case "word_separators":
		cfg.WordSeparators = value
	case "lock_password":
		cfg.LockPassword = value
	case "status_style":
		applyStyle(value, &cfg.StatusFG, &cfg.StatusBG, &cfg.StatusAttr)
	case "message_style":
		applyStyle(value, &cfg.MessageFG, &cfg.MessageBG, &cfg.MessageAttr)
	case "status_left_length":
		if n, err := strconv.Atoi(value); err == nil && n >= 0 {
			cfg.StatusLeftLength = n
		}
	case "status_right_length":
		if n, err := strconv.Atoi(value); err == nil && n >= 0 {
			cfg.StatusRightLength = n
		}
	case "status":
		// tmux `status`: on/off/1..5 status lines. ponytail: `off` (hide the bar)
		// isn't modeled — gtmux has no hide-bar today — so it clamps to 1 line.
		switch value {
		case "", "on", "off", "0", "1":
			cfg.StatusLines = 1
		default:
			if n, err := strconv.Atoi(value); err == nil && n >= 1 && n <= 5 {
				cfg.StatusLines = n
			}
		}
	case "status_format_2", "status_format_3", "status_format_4", "status_format_5":
		cfg.ExtraStatusFormats[name[len(name)-1]-'2'] = value
	case "status_justify":
		if value == "left" || value == "centre" || value == "center" || value == "right" {
			cfg.StatusJustify = value
		}
	case "status_position":
		if value == "top" || value == "bottom" {
			cfg.StatusPosition = value
		}
	case "window_status_separator":
		cfg.WindowStatusSeparator = value
	case "window_status_format":
		cfg.WindowStatusFormat = value
	case "window_status_current_format":
		cfg.WindowStatusCurrentFormat = value
	case "repeat_time":
		if n, err := strconv.Atoi(value); err == nil && n > 0 {
			cfg.RepeatTime = n
		}
	case "set_titles":
		cfg.SetTitles = value == "true" || value == "on" || value == "1"
	case "set_titles_string":
		cfg.SetTitlesString = value
	case "active_window_fg":
		setColor(&cfg.ActiveWindowFG)
	case "active_window_bg":
		setColor(&cfg.ActiveWindowBG)
	case "active_border_fg":
		setColor(&cfg.ActiveBorderFG)
	case "marked_border_fg":
		setColor(&cfg.MarkedBorderFG)
	case "pane_border_style":
		// tmux pane-border-style: the inactive pane dividers' fg/bg/attr. The
		// active/marked borders keep their own fg-only options (active_border_fg /
		// marked_border_fg) — a full pane-active-border-style alias is deferred
		// (it would collide with active_border_fg under the loader's opts replay).
		applyStyle(value, &cfg.InactiveBorderFG, &cfg.InactiveBorderBG, &cfg.InactiveBorderAttr)
	case "fill_fg":
		setColor(&cfg.FillFG)
	case "copy_cursor_fg":
		setColor(&cfg.CopyCursorFG)
	case "copy_cursor_bg":
		setColor(&cfg.CopyCursorBG)
	case "copy_selection_fg":
		setColor(&cfg.CopySelectionFG)
	case "copy_selection_bg":
		setColor(&cfg.CopySelectionBG)
	default:
		return false
	}
	return true
}

// applyStyle parses a tmux style string (comma-separated fg=/bg=/attr tokens,
// e.g. "fg=white,bg=dark_grey,bold") into the given fg/bg/attr fields. Only the
// components present are changed (partial, cumulative — like tmux);
// "none"/"default"/"noattr" clears attributes.
func applyStyle(value string, fg, bg *emu.Color, attr *int16) {
	for _, tok := range strings.Split(value, ",") {
		tok = strings.TrimSpace(tok)
		switch {
		case strings.HasPrefix(tok, "fg="):
			if c, ok := colorNames[strings.TrimPrefix(tok, "fg=")]; ok {
				*fg = c
			}
		case strings.HasPrefix(tok, "bg="):
			if c, ok := colorNames[strings.TrimPrefix(tok, "bg=")]; ok {
				*bg = c
			}
		case tok == "bold":
			*attr |= emu.AttrBold
		case tok == "reverse":
			*attr |= emu.AttrReverse
		case tok == "italics" || tok == "italic":
			*attr |= emu.AttrItalic
		case tok == "none" || tok == "default" || tok == "noattr" || tok == "":
			*attr = 0
		}
	}
}

var colorNames = map[string]emu.Color{
	"black": emu.Black, "red": emu.Red, "green": emu.Green, "yellow": emu.Yellow,
	"blue": emu.Blue, "magenta": emu.Magenta, "cyan": emu.Cyan, "white": emu.White,
	"light_grey": emu.LightGrey, "dark_grey": emu.DarkGrey,
	"light_red": emu.LightRed, "light_green": emu.LightGreen, "light_yellow": emu.LightYellow,
	"light_blue": emu.LightBlue, "light_magenta": emu.LightMagenta, "light_cyan": emu.LightCyan,
}

// ClientConfigPath returns ~/.config/gtmux/client.lua, or "" if the user's
// config directory can't be determined.
func ClientConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "gtmux", "client.lua")
}

// BindOp is one effect of a client keybind: either an Action to send to the
// server (routed through its runCommand), or a Local overlay the client opens
// from its own state (prompts/pickers whose data it already mirrors — so no
// server round-trip, no type-before-prompt-opens race). Exactly one is set.
type BindOp struct {
	Action []string // send as proto.Action
	Local  string   // "command-prompt" | "rename-window" | "rename-session" | "choose-window"
	Table  string   // switch the client into this key table for the next key (tmux switch-client -T)
}

// ClientBinds is the client's keybind table and prefix key, held in a Lua VM
// that stays alive to own the bound function values. Close it when the client
// exits.
type ClientBinds struct {
	l         *lua.LState
	Binds     map[byte]*lua.LFunction
	RootBinds map[byte]*lua.LFunction            // no-prefix binds (tmux bind -n)
	Repeat    map[byte]bool                      // prefix keys that repeat (tmux bind -r)
	Tables    map[string]map[byte]*lua.LFunction // custom key tables (tmux bind -T <table>)
	Prefix    byte
	ops       []BindOp // accumulated by the primitives while one bind runs
	// oBinds/oRoot are runtime overrides (tmux bind-key/unbind-key at runtime):
	// a key present here shadows the Lua bind — non-nil ops run instead, nil ops
	// mean unbound. Mutated from the server-message goroutine (SetOverride) and
	// read from the input goroutine (Resolve), so guarded by mu.
	oBinds, oRoot map[byte][]BindOp
	mu            sync.Mutex
}

func (c *ClientBinds) Close() { c.l.Close() }

// SetOverride installs a runtime bind for key (root = tmux bind -n table); ops
// nil marks it unbound, shadowing any Lua bind. tmux bind-key / unbind-key.
func (c *ClientBinds) SetOverride(key byte, root bool, ops []BindOp) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if root {
		if c.oRoot == nil {
			c.oRoot = map[byte][]BindOp{}
		}
		c.oRoot[key] = ops
		return
	}
	if c.oBinds == nil {
		c.oBinds = map[byte][]BindOp{}
	}
	c.oBinds[key] = ops
}

// Resolve runs the Lua function bound to key and returns the BindOps its
// primitives recorded. A runtime override wins (nil = unbound). Nil if unbound.
func (c *ClientBinds) Resolve(key byte) []BindOp {
	c.mu.Lock()
	ops, ok := c.oBinds[key]
	c.mu.Unlock()
	if ok {
		return ops
	}
	return c.run(c.Binds[key])
}

// ResolveRoot is Resolve for the no-prefix (bind -n) table.
func (c *ClientBinds) ResolveRoot(key byte) []BindOp {
	c.mu.Lock()
	ops, ok := c.oRoot[key]
	c.mu.Unlock()
	if ok {
		return ops
	}
	return c.run(c.RootBinds[key])
}

// ParseKey exposes the bind-key parser (single char or "C-x") to the client
// package for runtime bind-key.
func ParseKey(s string) (byte, bool) { return parseKey(s) }

// ResolveTable runs the bind for key in a custom key table (tmux bind -T), or
// nil if the table or key is unbound.
func (c *ClientBinds) ResolveTable(table string, key byte) []BindOp {
	if t := c.Tables[table]; t != nil {
		return c.run(t[key])
	}
	return nil
}

func (c *ClientBinds) run(fn *lua.LFunction) []BindOp {
	if fn == nil {
		return nil
	}
	c.ops = nil
	if err := c.l.CallByParam(lua.P{Fn: fn, NRet: 0, Protect: true}); err != nil {
		log.Printf("gtmux: keybind error: %v", err)
	}
	ops := c.ops
	c.ops = nil
	return ops
}

// dirFlag maps a select_pane direction to tmux's -L/-R/-U/-D flag.
var dirFlag = map[string]string{"left": "-L", "right": "-R", "up": "-U", "down": "-D"}

// LoadClient runs client.lua (embedded defaults, then the user's file layered
// on top) in one Lua VM that yields both the static chrome (ClientConfig) and
// the keybind table (ClientBinds). The VM stays alive inside ClientBinds; the
// caller Closes it. A broken user file just means the loaded-so-far state wins.
func LoadClient(path string) (ClientConfig, *ClientBinds) {
	return LoadClientWith(path, nil)
}

// LoadClientWith is LoadClient plus a list of runtime set-option overrides
// (name/value pairs) applied after the file, so a live `set-option` re-derives
// the config from the same source. Fresh VM every call: reset-then-eval, so a
// bind or option the file dropped really goes away.
func LoadClientWith(path string, overrides [][2]string) (ClientConfig, *ClientBinds) {
	cfg := DefaultClientConfig()
	L := lua.NewState()
	binds := &ClientBinds{
		l:         L,
		Binds:     map[byte]*lua.LFunction{},
		RootBinds: map[byte]*lua.LFunction{},
		Repeat:    map[byte]bool{},
		Tables:    map[string]map[byte]*lua.LFunction{},
		Prefix:    0x02,
	}

	tbl := L.NewTable()
	opts := L.NewTable()
	L.SetField(tbl, "options", opts)

	// Each primitive records a fixed BindOp; the key→action mapping lives here,
	// not on the server, so binds are configured entirely client-side.
	reg := func(name string, op BindOp) {
		L.SetField(tbl, name, L.NewFunction(func(l *lua.LState) int {
			binds.ops = append(binds.ops, op)
			return 0
		}))
	}
	reg("new_window", BindOp{Action: []string{"new-window"}})
	reg("next_window", BindOp{Action: []string{"next-window"}})
	reg("prev_window", BindOp{Action: []string{"previous-window"}})
	reg("last_window", BindOp{Action: []string{"last-window"}})
	reg("next_layout", BindOp{Action: []string{"next-layout"}})
	reg("previous_layout", BindOp{Action: []string{"previous-layout"}})
	reg("rotate_window", BindOp{Action: []string{"rotate-window"}})
	reg("source_file", BindOp{Action: []string{"source-file"}}) // reload this client's config live
	reg("split_v", BindOp{Action: []string{"split-window", "-h"}})
	reg("split_h", BindOp{Action: []string{"split-window"}})
	reg("kill_pane", BindOp{Action: []string{"kill-pane"}})
	reg("detach", BindOp{Action: []string{"detach"}})
	reg("show_pane_numbers", BindOp{Action: []string{"display-panes"}})
	reg("enter_copy_mode", BindOp{Action: []string{"copy-mode"}})
	reg("paste", BindOp{Action: []string{"paste"}})
	reg("zoom", BindOp{Action: []string{"resize-pane", "-Z"}})
	reg("break_pane", BindOp{Action: []string{"break-pane"}})
	reg("mark_pane", BindOp{Action: []string{"mark-pane"}})
	reg("join_marked", BindOp{Action: []string{"join-marked"}})
	reg("choose_session", BindOp{Action: []string{"choose-session"}})
	reg("rename_session_prompt", BindOp{Local: "rename-session"})
	reg("rename_window_prompt", BindOp{Local: "rename-window"})
	reg("choose_window", BindOp{Local: "choose-window"})
	reg("send_prefix", BindOp{Action: []string{"send-prefix"}})
	reg("choose_buffer", BindOp{Action: []string{"choose-buffer"}})
	reg("respawn_pane", BindOp{Action: []string{"respawn-pane", "-k"}})
	reg("respawn_window", BindOp{Action: []string{"respawn-window", "-k"}})
	reg("clock_mode", BindOp{Action: []string{"clock-mode"}})
	reg("lock", BindOp{Action: []string{"lock"}})

	// find_window(pattern): select the first window whose name contains pattern.
	L.SetField(tbl, "find_window", L.NewFunction(func(l *lua.LState) int {
		binds.ops = append(binds.ops, BindOp{Action: []string{"find-window", l.CheckString(1)}})
		return 0
	}))

	// choose_tree([flag...]): session→window tree picker. Extra args pass through
	// to the choose-tree command, e.g. gtmux.choose_tree("-f", "#{m:pfx-*,#{session_name}}")
	// keeps only matching sessions (tmux's `-f` filter).
	L.SetField(tbl, "choose_tree", L.NewFunction(func(l *lua.LState) int {
		args := []string{"choose-tree"}
		for i := 1; i <= l.GetTop(); i++ {
			args = append(args, l.CheckString(i))
		}
		binds.ops = append(binds.ops, BindOp{Action: args})
		return 0
	}))

	// run_shell(cmd): run a shell command server-side (tmux's `run-shell`); its
	// first output line shows as a status message. cmd is one string (spaces and
	// quotes preserved — the Action reaches runCommand as a single arg).
	L.SetField(tbl, "run_shell", L.NewFunction(func(l *lua.LState) int {
		binds.ops = append(binds.ops, BindOp{Action: []string{"run-shell", l.CheckString(1)}})
		return 0
	}))

	// switch_client(flag...): retarget this client to another session —
	// gtmux.switch_client("-n"|"-p"|"-l") or ("-t", name).
	L.SetField(tbl, "switch_client", L.NewFunction(func(l *lua.LState) int {
		args := []string{"switch-client"}
		for i := 1; i <= l.GetTop(); i++ {
			args = append(args, l.CheckString(i))
		}
		binds.ops = append(binds.ops, BindOp{Action: args})
		return 0
	}))

	// command_prompt([prompt], [initial], [template]): with no template it's the
	// plain ":" command line (run the typed text as a command). With a template,
	// the typed text substitutes for %1/%% in the template, which then runs — so
	// `command_prompt("new name:", "", "rename-window %1")`. The template is
	// pre-split into fields here; %1 as its own field takes the whole typed text
	// (spaces and all). Encoded as an Action the client's dispatch intercepts.
	L.SetField(tbl, "command_prompt", L.NewFunction(func(l *lua.LState) int {
		args := []string{"command-prompt"}
		if p := l.OptString(1, ""); p != "" {
			args = append(args, "-p", p)
		}
		if i := l.OptString(2, ""); i != "" {
			args = append(args, "-I", i)
		}
		if t := l.OptString(3, ""); t != "" {
			args = append(args, "--")
			// Quote-aware split (not strings.Fields) so a quoted nested command —
			// `new-window 'workspacer %1 %2 ; read'` — stays ONE template field;
			// per-field %N substitution then preserves it (spaces/`;` and all).
			args = append(args, splitFields(t)...)
		}
		binds.ops = append(binds.ops, BindOp{Action: args})
		return 0
	}))
	// display_popup([command]): open a floating terminal running command (shell
	// if none). Runs server-side; the client renders the overlay. One arg, so
	// multi-word commands survive (quoting doesn't).
	L.SetField(tbl, "display_popup", L.NewFunction(func(l *lua.LState) int {
		args := []string{"display-popup"}
		if cmd := l.OptString(1, ""); cmd != "" {
			args = append(args, "--", cmd)
		}
		binds.ops = append(binds.ops, BindOp{Action: args})
		return 0
	}))
	// display_menu(title, name1, cmd1, name2, cmd2, ...): open a menu overlay;
	// selecting an item runs its command. Encoded as an Action the client
	// intercepts. Each cmd is one arg (multi-word survives, quoting doesn't).
	L.SetField(tbl, "display_menu", L.NewFunction(func(l *lua.LState) int {
		args := []string{"display-menu", "-T", l.CheckString(1), "--"}
		for i := 2; i <= l.GetTop(); i++ {
			args = append(args, l.CheckString(i))
		}
		binds.ops = append(binds.ops, BindOp{Action: args})
		return 0
	}))
	// if_shell(shell, then, [else]): run `shell` on keypress; on exit 0 run the
	// `then` command, else the `else` command. Server-side (it has the shell +
	// command runner). Each command is one arg — multi-word parts survive; inner
	// quoting doesn't (deferred). For load-time conditionals, use Lua's own `if`.
	L.SetField(tbl, "if_shell", L.NewFunction(func(l *lua.LState) int {
		args := []string{"if-shell", l.CheckString(1), l.CheckString(2)}
		if e := l.OptString(3, ""); e != "" {
			args = append(args, e)
		}
		binds.ops = append(binds.ops, BindOp{Action: args})
		return 0
	}))
	// confirm_before(command, [prompt]): opens a y/n prompt; only y/Y runs the
	// command (Enter and everything else cancel, like tmux).
	L.SetField(tbl, "confirm_before", L.NewFunction(func(l *lua.LState) int {
		cmd := l.CheckString(1)
		prompt := l.OptString(2, "confirm? (y/n)")
		args := []string{"confirm-before", "-p", prompt, "--"}
		args = append(args, strings.Fields(cmd)...)
		binds.ops = append(binds.ops, BindOp{Action: args})
		return 0
	}))

	L.SetField(tbl, "select_pane", L.NewFunction(func(l *lua.LState) int {
		if f := dirFlag[l.CheckString(1)]; f != "" {
			binds.ops = append(binds.ops, BindOp{Action: []string{"select-pane", f}})
		}
		return 0
	}))
	// Vim-aware directional nav (tmux's vim-split pattern). "last" -> select
	// the previously-active pane. The server decides vim-vs-switch live.
	L.SetField(tbl, "select_pane_vim", L.NewFunction(func(l *lua.LState) int {
		d := l.CheckString(1)
		flag := dirFlag[d]
		if d == "last" {
			flag = "-l"
		}
		if flag != "" {
			binds.ops = append(binds.ops, BindOp{Action: []string{"select-pane-vim", flag}})
		}
		return 0
	}))
	L.SetField(tbl, "resize_pane", L.NewFunction(func(l *lua.LState) int {
		if f := dirFlag[l.CheckString(1)]; f != "" {
			binds.ops = append(binds.ops, BindOp{Action: []string{"resize-pane", f, strconv.Itoa(l.CheckInt(2))}})
		}
		return 0
	}))
	L.SetField(tbl, "select_window", L.NewFunction(func(l *lua.LState) int {
		binds.ops = append(binds.ops, BindOp{Action: []string{"select-window", strconv.Itoa(l.CheckInt(1))}})
		return 0
	}))
	L.SetField(tbl, "select_layout", L.NewFunction(func(l *lua.LState) int {
		binds.ops = append(binds.ops, BindOp{Action: []string{"select-layout", l.CheckString(1)}})
		return 0
	}))
	L.SetField(tbl, "swap_pane", L.NewFunction(func(l *lua.LState) int {
		args := []string{"swap-pane"}
		if l.CheckString(1) == "prev" {
			args = append(args, "-U")
		}
		binds.ops = append(binds.ops, BindOp{Action: args})
		return 0
	}))
	L.SetField(tbl, "swap_window", L.NewFunction(func(l *lua.LState) int {
		args := []string{"swap-window"}
		if l.CheckString(1) == "prev" {
			args = append(args, "-L")
		}
		binds.ops = append(binds.ops, BindOp{Action: args})
		return 0
	}))
	L.SetField(tbl, "bind", L.NewFunction(func(l *lua.LState) int {
		if b, ok := parseKey(l.CheckString(1)); ok {
			binds.Binds[b] = l.CheckFunction(2)
		}
		return 0
	}))
	// bind_repeat is bind + tmux's -r: after firing, the key repeats without
	// re-pressing the prefix until the repeat window (client-side) lapses.
	L.SetField(tbl, "bind_repeat", L.NewFunction(func(l *lua.LState) int {
		if b, ok := parseKey(l.CheckString(1)); ok {
			binds.Binds[b] = l.CheckFunction(2)
			binds.Repeat[b] = true
		}
		return 0
	}))
	// bind_root is tmux's bind -n: no prefix, the bare key fires it.
	L.SetField(tbl, "bind_root", L.NewFunction(func(l *lua.LState) int {
		if b, ok := parseKey(l.CheckString(1)); ok {
			binds.RootBinds[b] = l.CheckFunction(2)
		}
		return 0
	}))
	// bind_table(table, key, fn) is tmux's bind -T <table>: a key in a custom
	// table, reachable by first switching into it (key_table below).
	L.SetField(tbl, "bind_table", L.NewFunction(func(l *lua.LState) int {
		table := l.CheckString(1)
		if b, ok := parseKey(l.CheckString(2)); ok {
			if binds.Tables[table] == nil {
				binds.Tables[table] = map[byte]*lua.LFunction{}
			}
			binds.Tables[table][b] = l.CheckFunction(3)
		}
		return 0
	}))
	// key_table(name) is tmux's switch-client -T: the next key is looked up in
	// the named table (one-shot; reverts to root after), so multi-key sequences
	// work. Recorded as a BindOp the client's input machine acts on.
	L.SetField(tbl, "key_table", L.NewFunction(func(l *lua.LState) int {
		binds.ops = append(binds.ops, BindOp{Table: l.CheckString(1)})
		return 0
	}))
	// set_option and gtmux.options.* both funnel through applyOption (the single
	// client-option registry), so runtime set-option reaches exactly what the
	// config file can.
	L.SetField(tbl, "set_option", L.NewFunction(func(l *lua.LState) int {
		applyOption(&cfg, binds, l.CheckString(1), l.CheckString(2))
		return 0
	}))
	L.SetGlobal("gtmux", tbl)

	if err := L.DoString(defaultClientLua); err != nil {
		log.Fatalf("gtmux: embedded default client config is broken: %v", err)
	}
	if data, err := os.ReadFile(path); err == nil {
		if err := L.DoString(string(data)); err != nil {
			log.Printf("gtmux: %s: %v (ignoring, using defaults)", path, err)
		}
	}

	// gtmux.options.X = Y entries feed the same registry: read each as a string
	// and applyOption it (unknown names are ignored).
	opts.ForEach(func(k, v lua.LValue) {
		name, ok := k.(lua.LString)
		if !ok {
			return
		}
		switch val := v.(type) {
		case lua.LString:
			applyOption(&cfg, binds, string(name), string(val))
		case lua.LBool:
			applyOption(&cfg, binds, string(name), boolStr(bool(val)))
		}
	})

	// Runtime set-option overrides, applied last so they win over the file.
	for _, o := range overrides {
		applyOption(&cfg, binds, o[0], o[1])
	}
	return cfg, binds
}
