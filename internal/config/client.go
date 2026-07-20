package config

import (
	_ "embed"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	lua "github.com/yuin/gopher-lua"

	"github.com/FyrmForge/gtmux/internal/emu"
	"github.com/FyrmForge/gtmux/internal/proto"
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
	// window-status-style: style for inactive window-list entries. Inherits the
	// status style unless WindowStatusStyleSet. (window-status-current-style is
	// deferred: it would alias active_window_fg/bg — the loader-provenance trap.)
	WindowStatusFG, WindowStatusBG emu.Color
	WindowStatusAttr               int16
	WindowStatusStyleSet           bool
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

	// Pane border rendering (gtmux extension over tmux's single-line dividers):
	//   "simple" (default) — straight │/─, crossings overwrite (today's look).
	//   "joined"           — box-drawing junctions (┼├┤┬┴) on the shared dividers,
	//                        computed client-side; no geometry change.
	//   "framed"           — every pane fully enclosed in its own frame + outer
	//                        window frame; the server reserves the border cells.
	PaneBorders string
	// PaneBorderRounded rounds outer frame corners (╭╮╰╯); framed mode only.
	PaneBorderRounded bool
	// PaneBorderTitle anchors a title on the frame: "" (off) or one of
	// top-left|top-centre|top-right|bottom-left|bottom-centre|bottom-right.
	// PaneBorderOffset nudges it by N cells along that edge.
	PaneBorderTitle  string
	PaneBorderOffset int

	// Widgets are user-defined overlay elements the client composites on top of
	// pane content (gtmux.widget in client config). First slice: static text at
	// fixed coords; sources/anchors come later (see TODO Widget system).
	Widgets []WidgetSpec
}

// WidgetSpec is one config-defined widget. Text is a status-style format string
// (expanded client-side each tick, "\n" = new line); FG/BG/Attr its style.
//
// Dock "" = a floating overlay at window-space Row/Col. Dock "left"/"right"
// reserves Size columns on that edge (content flows in the middle); each line of
// Text draws on a successive row of the strip.
type WidgetSpec struct {
	Row, Col int
	Text     string
	FG, BG   emu.Color
	Attr     int16
	Dock     string // "" | "left" | "right" | "top" | "bottom"
	Size     int    // reserved columns/rows when docked
	// TextFn, if set, is a Lua function that returns the widget's text (run
	// client-side each refresh in place of the Text format string). It reads
	// gtmux.sessions()/panes()/etc. and returns a string ("\n" = new line).
	TextFn *lua.LFunction
	// OnClick, if set, runs when the widget is clicked (like a keybind: it records
	// actions the client dispatches). Receives a hit table {line, line_text, col}.
	OnClick *lua.LFunction
	// Draw, if set, is a Lua function given a Canvas to paint into (boxes, borders,
	// separators, positioned text) — the full 2D drawing path. Takes precedence
	// over Text/TextFn. Width/Height size a floating draw widget (docked ones use
	// the dock size × content extent).
	Draw          *lua.LFunction
	Width, Height int
	// Component, if set, is a Lua function fn(props, ui) — the component runtime.
	// It paints through the same verbs as Draw but coords are local, it can nest
	// sub-components (ui:child) and emit clickable regions (ui:on_click). Takes
	// precedence over Draw. Draw stays as the one-arg fn(ui) form for back-compat.
	Component *lua.LFunction
	// Interval throttles TextFn/Draw re-runs to at most once per Interval seconds
	// (0 = every status tick). A clock wants 1; a session list can be lazier.
	Interval int
}

// Region is a widget-local clickable rectangle a component emits via ui:on_click
// during a draw. Coords are relative to the widget's top-left (X=col, Y=row) —
// the same space clickWidget hands the hit-test, so no host/physical offset is
// folded in. Emission order is click priority: a component emits its own
// on_click before descending into children, and the hit-test takes the
// last-emitted region that contains the point (deepest child wins).
type Region struct {
	X, Y, W, H int
	OnClick    *lua.LFunction
}

// windowFlags builds tmux's window_flags string for a snapshot window, in the
// status bar's order: * current, # activity, ! bell, ~ silence, Z zoomed. Given
// to gtmux.windows() as `flags` so a status component reaches renderBar parity
// without re-deriving the order.
func windowFlags(w proto.SnapWindow) string {
	f := ""
	if w.Active {
		f += "*"
	}
	if w.Activity {
		f += "#"
	}
	if w.Bell {
		f += "!"
	}
	if w.Silence {
		f += "~"
	}
	if w.Zoomed {
		f += "Z"
	}
	return f
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

		PaneBorders: "simple", // tmux-faithful default: straight dividers
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
		if b, ok := parseKeyByte(value); ok {
			binds.Prefix = b
		}
	case "prefix2":
		if b, ok := parseKeyByte(value); ok {
			binds.Prefix2 = b // secondary prefix; 0 = unset
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
	case "window_status_style":
		applyStyle(value, &cfg.WindowStatusFG, &cfg.WindowStatusBG, &cfg.WindowStatusAttr)
		cfg.WindowStatusStyleSet = true
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
		// tmux `status`: off (hide the bar), on, or 1..5 status lines.
		switch value {
		case "off", "0":
			cfg.StatusLines = 0
		case "", "on", "1":
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
	case "pane_borders":
		cfg.PaneBorders = value // "simple" | "joined" | "framed"
	case "pane_border_rounded":
		cfg.PaneBorderRounded = value == "true" || value == "on" || value == "1"
	case "pane_border_title":
		cfg.PaneBorderTitle = value // anchor: top-left..bottom-right, or "" = off
	case "pane_border_offset":
		if n, err := strconv.Atoi(value); err == nil {
			cfg.PaneBorderOffset = n
		}
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

// ColorByName resolves a tmux color name ("red", "light_blue", …) to an
// emu.Color. ok is false for an unknown name (caller keeps its default).
func ColorByName(name string) (emu.Color, bool) {
	c, ok := colorNames[name]
	return c, ok
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
	Action  []string   // send as proto.Action
	Local   string     // "command-prompt" | "rename-window" | "rename-session" | "choose-window"
	Table   string     // switch the client into this key table for the next key (tmux switch-client -T)
	Modal   *ModalOpen // open a modal keyboard widget (gtmux.open{...})
	Command string     // a raw command line the client tokenizes + dispatches (gtmux.run_command)
	Border  *PaneBorder // set a pane's border override color (pane:set_border)
}

// PaneBorder is a per-pane border color override (pane:set_border("red")): the
// compositor paints that pane's outline in Color until the pane is focused.
// Empty Color clears the override.
type PaneBorder struct {
	PaneID int
	Color  string
}

// ModalOpen is a request to open a modal component overlay: it paints via
// Component and receives every key through OnKey until OnKey calls ui:close().
// Width/Height size the (centered) box. The Lua fns live in the client's VM.
type ModalOpen struct {
	Component, OnKey *lua.LFunction
	Width, Height    int
	Position         string // "center" (default) | "status" (the status/message line)
}

// ClientBinds is the client's keybind table and prefix key, held in a Lua VM
// that stays alive to own the bound function values. Close it when the client
// exits.
type ClientBinds struct {
	l         *lua.LState
	Binds     map[string]*lua.LFunction
	RootBinds map[string]*lua.LFunction            // no-prefix binds (tmux bind -n)
	Repeat    map[string]bool                      // prefix keys that repeat (tmux bind -r)
	Tables    map[string]map[string]*lua.LFunction // custom key tables (tmux bind -T <table>)
	Prefix    byte
	Prefix2   byte // tmux prefix2: optional secondary prefix key; 0 = unset
	ops       []BindOp // accumulated by the primitives while one bind runs
	// oBinds/oRoot are runtime overrides (tmux bind-key/unbind-key at runtime):
	// a key present here shadows the Lua bind — non-nil ops run instead, nil ops
	// mean unbound. Mutated from the server-message goroutine (SetOverride) and
	// read from the input goroutine (Resolve), so guarded by mu.
	oBinds, oRoot map[string][]BindOp
	mu            sync.Mutex
	// vmMu serializes every call INTO the Lua VM. Binds resolve on the input
	// goroutine, but widget text/click functions now also run from the render
	// (decode) goroutine — gopher-lua's LState is not goroutine-safe, so all
	// CallByParam/PCall go through this.
	vmMu sync.Mutex
	// Hooks are the live client-state accessors the widget query primitives read
	// (gtmux.sessions/panes/context/expand/...). The client installs them after
	// load; nil-safe (unset returns empty).
	Hooks WidgetHooks
	// Alerts maps an alert event name ("alert-bell"/"alert-activity"/
	// "alert-silence", tmux's alert-* hooks) to the Lua callbacks registered via
	// gtmux.on. The client fires them on a window's false→true flag edge.
	Alerts map[string][]*lua.LFunction
}

// AlertEvent describes a window flag transition the client detected in a
// pushed snapshot, passed to gtmux.on callbacks so config can react (e.g.
// notify when "Claude is done"). Command/Title come from the window's active
// pane, letting a callback filter for agent processes.
type AlertEvent struct {
	Event   string // "alert-bell" | "alert-activity" | "alert-silence"
	Session string
	Window  int    // display index
	Name    string // window name
	Command string // active pane's current command
	Title   string // active pane's title
}

// WidgetHooks are the client-provided accessors the Lua widget query primitives
// call at runtime. Set by the client after LoadClient; each is nil until then.
type WidgetHooks struct {
	Snapshot func() *proto.StateSnapshot // gtmux.sessions/windows/panes/clients/find_panes
	Context  func() map[string]string   // gtmux.context(): session/window/pane/prefix/width/height
	Expand   func(string) string        // gtmux.expand()
	Option   func(string) string        // gtmux.get_option()
}

func (c *ClientBinds) Close() { c.l.Close() }

// RunText runs a widget's text function and returns its string result. Distinct
// from run(): NRet=1, no ops recorded. Empty string on error or non-string.
func (c *ClientBinds) RunText(fn *lua.LFunction) string {
	if fn == nil {
		return ""
	}
	c.vmMu.Lock()
	defer c.vmMu.Unlock()
	if err := c.l.CallByParam(lua.P{Fn: fn, NRet: 1, Protect: true}); err != nil {
		log.Printf("gtmux: widget text error: %v", err)
		return ""
	}
	ret := c.l.Get(-1)
	c.l.Pop(1)
	if s, ok := ret.(lua.LString); ok {
		return string(s)
	}
	return ""
}

// Canvas is a widget's 2D drawing surface: a W×H glyph grid a draw function
// paints into (gtmux.widget{ draw = function(c) ... end }). The compositor blits
// it into the widget's region. Cells start as a space in the widget's base style;
// the draw primitives overwrite them.
type Canvas struct {
	W, H  int
	Cells []emu.Glyph
	base  emu.Glyph
}

// hasVertical/hasHorizontal report whether a box-drawing rune already carries a
// stroke in that axis, so hline/vline can merge into a proper junction instead
// of overwriting a border. mergeH/mergeV pick the tee/cross for a crossing.
func hasVertical(r rune) bool {
	switch r {
	case '│', '┌', '┐', '└', '┘', '├', '┤', '┬', '┴', '┼', '╭', '╮', '╰', '╯':
		return true
	}
	return false
}

func hasHorizontal(r rune) bool {
	switch r {
	case '─', '┌', '┐', '└', '┘', '├', '┤', '┬', '┴', '┼', '╭', '╮', '╰', '╯':
		return true
	}
	return false
}

func mergeH(leftEnd, rightEnd bool) rune {
	switch {
	case leftEnd:
		return '├'
	case rightEnd:
		return '┤'
	default:
		return '┼'
	}
}

func mergeV(topEnd, botEnd bool) rune {
	switch {
	case topEnd:
		return '┬'
	case botEnd:
		return '┴'
	default:
		return '┼'
	}
}

// drawFrameTitle embeds a title on a box's top or bottom border line at an
// anchor (top-left..bottom-right), offset cells along the edge, clipped between
// the corners. The 6-anchor vocabulary is shared with pane frames (phase B).
func drawFrameTitle(cv *Canvas, x, y, bw, bh int, label, at string, offset int, g emu.Glyph) {
	rs := []rune(label)
	if bw < 3 || len(rs) == 0 {
		return
	}
	row := y
	if strings.HasPrefix(at, "bottom") {
		row = y + bh - 1
	}
	inner := bw - 2 // cells between the two corners
	start := x + 1  // left (default)
	switch {
	case strings.HasSuffix(at, "centre"), strings.HasSuffix(at, "center"):
		start = x + 1 + (inner-len(rs))/2
	case strings.HasSuffix(at, "right"):
		start = x + bw - 1 - len(rs)
	}
	start += offset
	for i, r := range rs {
		if col := start + i; col > x && col < x+bw-1 {
			cv.put(col, row, r, g)
		}
	}
}

func newCanvas(w, h int, fg, bg emu.Color, attr int16) *Canvas {
	if w < 0 {
		w = 0
	}
	if h < 0 {
		h = 0
	}
	base := emu.Glyph{Char: ' ', FG: fg, BG: bg, Mode: attr}
	cells := make([]emu.Glyph, w*h)
	for i := range cells {
		cells[i] = base
	}
	return &Canvas{W: w, H: h, Cells: cells, base: base}
}

// At returns the glyph at (x,y), or a zero glyph if out of bounds (ok=false).
func (cv *Canvas) At(x, y int) (emu.Glyph, bool) {
	if x < 0 || y < 0 || x >= cv.W || y >= cv.H {
		return emu.Glyph{}, false
	}
	return cv.Cells[y*cv.W+x], true
}

func (cv *Canvas) put(x, y int, ch rune, g emu.Glyph) {
	if x < 0 || y < 0 || x >= cv.W || y >= cv.H {
		return
	}
	g.Char = ch
	cv.Cells[y*cv.W+x] = g
}

// style resolves an optional per-primitive style string over the canvas base.
func (cv *Canvas) style(s string) emu.Glyph {
	g := cv.base
	if s != "" {
		applyStyle(s, &g.FG, &g.BG, &g.Mode)
	}
	return g
}

// buildUI returns the factory for the `ui` object a draw/component fn paints
// through. Each ui covers a sub-rect of cv: origin (ox,oy) in canvas cells, size
// w×h, and a clip window [cx0,cy0)–[cx1,cy1) (the intersection of ancestor
// boxes). Draw verbs take LOCAL coords (0,0 = this ui's top-left), translated by
// the origin and clipped — so a component can't scribble outside its box. It
// also exposes on_click (register this ui's box as a clickable Region, in
// widget-local coords) and child (run a sub-component in a nested rect).
//
// All ui methods run on the decode goroutine under vmMu, already held by the
// caller (runPaint) — they must NOT re-lock it. Nested CallByParam for a child
// fn is fine (gopher-lua allows reentrant calls on one goroutine). regions
// accumulates emitted click rects in emission order.
func (c *ClientBinds) buildUI(cv *Canvas, regions *[]Region, state *lua.LTable) func(ox, oy, w, h, cx0, cy0, cx1, cy1 int) *lua.LTable {
	L := c.l
	firstRune := func(s string) rune {
		for _, r := range s {
			return r
		}
		return ' '
	}
	var mk func(ox, oy, w, h, cx0, cy0, cx1, cy1 int) *lua.LTable
	mk = func(ox, oy, w, h, cx0, cy0, cx1, cy1 int) *lua.LTable {
		// put/at translate a ui-local (lx,ly) by the origin and clip to the box.
		put := func(lx, ly int, ch rune, g emu.Glyph) {
			gx, gy := ox+lx, oy+ly
			if gx < cx0 || gx >= cx1 || gy < cy0 || gy >= cy1 {
				return
			}
			cv.put(gx, gy, ch, g)
		}
		at := func(lx, ly int) (emu.Glyph, bool) { return cv.At(ox+lx, oy+ly) }
		// drawTitle is drawFrameTitle in local coords (put clips for us).
		drawTitle := func(x, y, bw, bh int, label, atStr string, offset int, g emu.Glyph) {
			rs := []rune(label)
			if bw < 3 || len(rs) == 0 {
				return
			}
			row := y
			if strings.HasPrefix(atStr, "bottom") {
				row = y + bh - 1
			}
			inner := bw - 2
			start := x + 1
			switch {
			case strings.HasSuffix(atStr, "centre"), strings.HasSuffix(atStr, "center"):
				start = x + 1 + (inner-len(rs))/2
			case strings.HasSuffix(atStr, "right"):
				start = x + bw - 1 - len(rs)
			}
			start += offset
			for i, r := range rs {
				if col := start + i; col > x && col < x+bw-1 {
					put(col, row, r, g)
				}
			}
		}

		t := L.NewTable()
		t.RawSetString("w", lua.LNumber(w))
		t.RawSetString("h", lua.LNumber(h))
		// ui:set(x, y, char, style?)
		L.SetField(t, "set", L.NewFunction(func(l *lua.LState) int {
			put(l.CheckInt(2), l.CheckInt(3), firstRune(l.CheckString(4)), cv.style(l.OptString(5, "")))
			return 0
		}))
		// ui:text(x, y, str, style?)
		L.SetField(t, "text", L.NewFunction(func(l *lua.LState) int {
			x, y := l.CheckInt(2), l.CheckInt(3)
			g := cv.style(l.OptString(5, ""))
			for i, r := range l.CheckString(4) {
				put(x+i, y, r, g)
			}
			return 0
		}))
		// ui:box(x, y, w, h, style | {style=,title=,title_at=}?)
		L.SetField(t, "box", L.NewFunction(func(l *lua.LState) int {
			x, y, bw, bh := l.CheckInt(2), l.CheckInt(3), l.CheckInt(4), l.CheckInt(5)
			s, title, titleAt := "", "", "top-left"
			switch a := l.Get(6).(type) {
			case lua.LString:
				s = string(a)
			case *lua.LTable:
				if v, ok := a.RawGetString("style").(lua.LString); ok {
					s = string(v)
				}
				if v, ok := a.RawGetString("title").(lua.LString); ok {
					title = string(v)
				}
				if v, ok := a.RawGetString("title_at").(lua.LString); ok {
					titleAt = string(v)
				}
			}
			g := cv.style(s)
			if bw < 2 || bh < 2 {
				return 0
			}
			tl, tr, bl, br := '┌', '┐', '└', '┘'
			if strings.Contains(s, "rounded") {
				tl, tr, bl, br = '╭', '╮', '╰', '╯'
			}
			for i := 1; i < bw-1; i++ {
				put(x+i, y, '─', g)
				put(x+i, y+bh-1, '─', g)
			}
			for j := 1; j < bh-1; j++ {
				put(x, y+j, '│', g)
				put(x+bw-1, y+j, '│', g)
			}
			put(x, y, tl, g)
			put(x+bw-1, y, tr, g)
			put(x, y+bh-1, bl, g)
			put(x+bw-1, y+bh-1, br, g)
			if title != "" {
				drawTitle(x, y, bw, bh, " "+title+" ", titleAt, 0, g)
			}
			return 0
		}))
		// ui:hline(y, style?) — separator across THIS ui's width, junction-aware.
		L.SetField(t, "hline", L.NewFunction(func(l *lua.LState) int {
			y := l.CheckInt(2)
			g := cv.style(l.OptString(3, ""))
			for x := 0; x < w; x++ {
				if cur, ok := at(x, y); ok && hasVertical(cur.Char) {
					put(x, y, mergeH(x == 0, x == w-1), cur)
				} else {
					put(x, y, '─', g)
				}
			}
			return 0
		}))
		// ui:vline(x, style?) — separator down THIS ui's height, junction-aware.
		L.SetField(t, "vline", L.NewFunction(func(l *lua.LState) int {
			x := l.CheckInt(2)
			g := cv.style(l.OptString(3, ""))
			for y := 0; y < h; y++ {
				if cur, ok := at(x, y); ok && hasHorizontal(cur.Char) {
					put(x, y, mergeV(y == 0, y == h-1), cur)
				} else {
					put(x, y, '│', g)
				}
			}
			return 0
		}))
		// ui:fill(style?) — flood THIS ui's box with a style's space.
		L.SetField(t, "fill", L.NewFunction(func(l *lua.LState) int {
			g := cv.style(l.OptString(2, ""))
			for y := 0; y < h; y++ {
				for x := 0; x < w; x++ {
					put(x, y, ' ', g)
				}
			}
			return 0
		}))
		// ui:state(key?) — a persistent table that survives across redraws (the
		// component is re-run each refresh; state is not). No key = the widget's
		// root state table (shared by the whole component tree — namespace with
		// your own fields); a key = a persistent sub-table under it, for a child
		// that wants isolation. Mutate it in an on_click; the click re-renders.
		L.SetField(t, "state", L.NewFunction(func(l *lua.LState) int {
			if l.GetTop() >= 2 {
				key := l.CheckString(2)
				sub, ok := state.RawGetString(key).(*lua.LTable)
				if !ok {
					sub = L.NewTable()
					state.RawSetString(key, sub)
				}
				l.Push(sub)
			} else {
				l.Push(state)
			}
			return 1
		}))
		// ui:on_click(fn) — register this ui's whole box as clickable. Emit before
		// descending into children so a child's region (emitted later) wins.
		L.SetField(t, "on_click", L.NewFunction(func(l *lua.LState) int {
			if fn, ok := l.Get(2).(*lua.LFunction); ok {
				*regions = append(*regions, Region{X: ox, Y: oy, W: w, H: h, OnClick: fn})
			}
			return 0
		}))
		// ui:child(x, y, w, h, fn, props?) — run a sub-component in a nested rect
		// (local coords); its draws/regions are offset+clipped automatically.
		L.SetField(t, "child", L.NewFunction(func(l *lua.LState) int {
			cx, cy, cw, ch := l.CheckInt(2), l.CheckInt(3), l.CheckInt(4), l.CheckInt(5)
			fn, ok := l.Get(6).(*lua.LFunction)
			if !ok {
				return 0
			}
			var props lua.LValue = lua.LNil
			if l.GetTop() >= 7 {
				props = l.Get(7)
			}
			nx0, ny0 := max(cx0, ox+cx), max(cy0, oy+cy)
			nx1, ny1 := min(cx1, ox+cx+cw), min(cy1, oy+cy+ch)
			childUI := mk(ox+cx, oy+cy, cw, ch, nx0, ny0, nx1, ny1)
			if err := l.CallByParam(lua.P{Fn: fn, NRet: 0, Protect: true}, props, childUI); err != nil {
				log.Printf("gtmux: ui:child error: %v", err)
			}
			return 0
		}))
		return t
	}
	return mk
}

// runPaint runs a draw (one-arg fn(ui)) or component (two-arg fn(props, ui))
// against a fresh w×h canvas and returns the painted grid plus the click regions
// it emitted. Base style = the widget's fg/bg/attr so undrawn cells match the
// background. twoArg picks the calling convention — a draw=function(c) fn must
// keep being called fn(ui), never fn(props, ui), or its `c` binds to props.
func (c *ClientBinds) runPaint(fn *lua.LFunction, twoArg bool, state *lua.LTable, w, h int, fg, bg emu.Color, attr int16) (*Canvas, []Region, *lua.LTable) {
	cv := newCanvas(w, h, fg, bg, attr)
	if fn == nil {
		return cv, nil, state
	}
	c.vmMu.Lock()
	defer c.vmMu.Unlock()
	if state == nil {
		state = c.l.NewTable() // first render: create the widget's persistent state
	}
	var regions []Region
	root := c.buildUI(cv, &regions, state)(0, 0, w, h, 0, 0, w, h)
	var err error
	if twoArg {
		err = c.l.CallByParam(lua.P{Fn: fn, NRet: 0, Protect: true}, c.l.NewTable(), root)
	} else {
		err = c.l.CallByParam(lua.P{Fn: fn, NRet: 0, Protect: true}, root)
	}
	if err != nil {
		log.Printf("gtmux: widget paint error: %v", err)
	}
	return cv, regions, state
}

// RunDraw runs a widget's one-arg draw function fn(ui). Back-compat: the ui it
// gets is a root component (offset 0, full canvas), so existing c:set/text/box…
// fns work unchanged, and they may now also call ui:on_click. Draw fns get a
// throwaway state table (they're the stateless legacy form); use component for
// persistent state.
func (c *ClientBinds) RunDraw(fn *lua.LFunction, w, h int, fg, bg emu.Color, attr int16) (*Canvas, []Region) {
	cv, regions, _ := c.runPaint(fn, false, nil, w, h, fg, bg, attr)
	return cv, regions
}

// RunComponent runs a two-arg component fn(props, ui) as a widget's root. state
// is the widget's persistent store (nil on first render); the returned table is
// the same one, to be passed back next render so ui:state() survives redraws.
func (c *ClientBinds) RunComponent(fn *lua.LFunction, state *lua.LTable, w, h int, fg, bg emu.Color, attr int16) (*Canvas, []Region, *lua.LTable) {
	return c.runPaint(fn, true, state, w, h, fg, bg, attr)
}

// RunClick runs a widget's on_click function (like a bind: records BindOps the
// client dispatches). The client passes what was clicked — line index within the
// widget, that line's text, and the column — built into the {line, line_text,
// col} hit table under the VM lock.
func (c *ClientBinds) RunClick(fn *lua.LFunction, line int, lineText string, col int) []BindOp {
	if fn == nil {
		return nil
	}
	c.vmMu.Lock()
	defer c.vmMu.Unlock()
	hit := c.l.NewTable()
	hit.RawSetString("line", lua.LNumber(line))
	hit.RawSetString("line_text", lua.LString(lineText))
	hit.RawSetString("col", lua.LNumber(col))
	c.ops = nil
	if err := c.l.CallByParam(lua.P{Fn: fn, NRet: 0, Protect: true}, hit); err != nil {
		log.Printf("gtmux: widget on_click error: %v", err)
	}
	ops := c.ops
	c.ops = nil
	return ops
}

// RunKey runs a modal widget's on_key(key, ui). key is a key-name string
// ("Up", "Enter", "C-c", or a printable char). ui exposes only state() (the
// modal's persistent table, shared with its component) and close() — it is an
// event handler, not a paint surface. Returns the BindOps the handler recorded
// (e.g. switch_session) plus whether it asked to close. Under vmMu, like
// RunClick; the caller re-renders once after feeding a key chunk.
func (c *ClientBinds) RunKey(onKey *lua.LFunction, key string, state *lua.LTable) ([]BindOp, bool) {
	if onKey == nil {
		return nil, false
	}
	c.vmMu.Lock()
	defer c.vmMu.Unlock()
	if state == nil {
		state = c.l.NewTable()
	}
	closed := false
	ui := c.l.NewTable()
	c.l.SetField(ui, "state", c.l.NewFunction(func(l *lua.LState) int { l.Push(state); return 1 }))
	c.l.SetField(ui, "close", c.l.NewFunction(func(l *lua.LState) int { closed = true; return 0 }))
	c.ops = nil
	if err := c.l.CallByParam(lua.P{Fn: onKey, NRet: 0, Protect: true}, lua.LString(key), ui); err != nil {
		log.Printf("gtmux: widget on_key error: %v", err)
	}
	ops := c.ops
	c.ops = nil
	return ops, closed
}

// RunAlert fires the callbacks registered for ev.Event, each with a table
// describing the window, and returns the BindOps their primitives recorded (so
// a callback can select-window, run-command, etc.). Under vmMu, like RunKey.
func (c *ClientBinds) RunAlert(ev AlertEvent) []BindOp {
	fns := c.Alerts[ev.Event]
	if len(fns) == 0 {
		return nil
	}
	c.vmMu.Lock()
	defer c.vmMu.Unlock()
	arg := c.l.NewTable()
	c.l.SetField(arg, "event", lua.LString(ev.Event))
	c.l.SetField(arg, "session", lua.LString(ev.Session))
	c.l.SetField(arg, "window", lua.LNumber(ev.Window))
	c.l.SetField(arg, "name", lua.LString(ev.Name))
	c.l.SetField(arg, "command", lua.LString(ev.Command))
	c.l.SetField(arg, "title", lua.LString(ev.Title))
	c.ops = nil
	for _, fn := range fns {
		if err := c.l.CallByParam(lua.P{Fn: fn, NRet: 0, Protect: true}, arg); err != nil {
			log.Printf("gtmux: alert callback error: %v", err)
		}
	}
	ops := c.ops
	c.ops = nil
	return ops
}

// CommandExitEvent is an OSC 133 command-finished the client received, fed to
// gtmux.on("command-exited") callbacks as a pane object.
type CommandExitEvent struct {
	Session                  string
	Window, PaneID, ExitCode int
}

// RunPaneEvent fires the callbacks registered for event, each with a pane table
// {session, window, id, …extra} plus a :set_border(color) method that records a
// PaneBorder op. extra holds the event-specific fields (exit_code, command, …).
// Returns the ops the callbacks recorded. Under vmMu, like RunKey.
func (c *ClientBinds) RunPaneEvent(event, session string, window, paneID int, extra map[string]lua.LValue) []BindOp {
	fns := c.Alerts[event]
	if len(fns) == 0 {
		return nil
	}
	c.vmMu.Lock()
	defer c.vmMu.Unlock()
	pane := c.l.NewTable()
	c.l.SetField(pane, "session", lua.LString(session))
	c.l.SetField(pane, "window", lua.LNumber(window))
	c.l.SetField(pane, "id", lua.LNumber(paneID))
	for k, v := range extra {
		c.l.SetField(pane, k, v)
	}
	c.l.SetField(pane, "set_border", c.l.NewFunction(func(l *lua.LState) int {
		c.ops = append(c.ops, BindOp{Border: &PaneBorder{PaneID: paneID, Color: l.CheckString(2)}})
		return 0
	}))
	c.ops = nil
	for _, fn := range fns {
		if err := c.l.CallByParam(lua.P{Fn: fn, NRet: 0, Protect: true}, pane); err != nil {
			log.Printf("gtmux: %s callback error: %v", event, err)
		}
	}
	ops := c.ops
	c.ops = nil
	return ops
}

// RunCommandExit fires "command-exited" with a pane object carrying exit_code.
func (c *ClientBinds) RunCommandExit(ev CommandExitEvent) []BindOp {
	return c.RunPaneEvent("command-exited", ev.Session, ev.Window, ev.PaneID,
		map[string]lua.LValue{"exit_code": lua.LNumber(ev.ExitCode)})
}

// RunProgramChanged fires "program-changed" with a pane object carrying the new
// foreground command and the one it replaced (from).
func (c *ClientBinds) RunProgramChanged(session string, window, paneID int, command, from string) []BindOp {
	return c.RunPaneEvent("program-changed", session, window, paneID,
		map[string]lua.LValue{"command": lua.LString(command), "from": lua.LString(from)})
}

// SetOverride installs a runtime bind for key (root = tmux bind -n table); ops
// nil marks it unbound, shadowing any Lua bind. tmux bind-key / unbind-key.
func (c *ClientBinds) SetOverride(key string, root bool, ops []BindOp) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if root {
		if c.oRoot == nil {
			c.oRoot = map[string][]BindOp{}
		}
		c.oRoot[key] = ops
		return
	}
	if c.oBinds == nil {
		c.oBinds = map[string][]BindOp{}
	}
	c.oBinds[key] = ops
}

// Resolve runs the Lua function bound to key and returns the BindOps its
// primitives recorded. A runtime override wins (nil = unbound). Nil if unbound.
func (c *ClientBinds) Resolve(key string) []BindOp {
	c.mu.Lock()
	ops, ok := c.oBinds[key]
	c.mu.Unlock()
	if ok {
		return ops
	}
	return c.run(c.Binds[key])
}

// ResolveRoot is Resolve for the no-prefix (bind -n) table.
func (c *ClientBinds) ResolveRoot(key string) []BindOp {
	c.mu.Lock()
	ops, ok := c.oRoot[key]
	c.mu.Unlock()
	if ok {
		return ops
	}
	return c.run(c.RootBinds[key])
}

// ParseKey exposes the canonical bind-key parser to the client package for
// runtime bind-key: single char, "C-x", "M-x", or a named/function key.
func ParseKey(s string) (string, bool) { return parseKeyName(s) }

// ResolveTable runs the bind for key in a custom key table (tmux bind -T), or
// nil if the table or key is unbound.
func (c *ClientBinds) ResolveTable(table string, key string) []BindOp {
	if t := c.Tables[table]; t != nil {
		return c.run(t[key])
	}
	return nil
}

func (c *ClientBinds) run(fn *lua.LFunction) []BindOp {
	if fn == nil {
		return nil
	}
	c.vmMu.Lock()
	defer c.vmMu.Unlock()
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
		Binds:     map[string]*lua.LFunction{},
		RootBinds: map[string]*lua.LFunction{},
		Repeat:    map[string]bool{},
		Tables:    map[string]map[string]*lua.LFunction{},
		Alerts:    map[string][]*lua.LFunction{},
		Prefix:    0x02,
	}

	tbl := L.NewTable()
	defOpts := L.NewTable() // gtmux.options while the bundled default config runs
	L.SetField(tbl, "options", defOpts)

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
	// widget{row=, col=, text=, fg=, bg=, bold=}: register a static overlay
	// widget composited on top of window content at window-space (row,col).
	// text may contain \n for a multi-line block. First slice of the widget
	// system — static/floating only.
	L.SetField(tbl, "widget", L.NewFunction(func(l *lua.LState) int {
		t := l.CheckTable(1)
		ws := WidgetSpec{FG: emu.DefaultFG, BG: emu.DefaultBG}
		if v, ok := t.RawGetString("row").(lua.LNumber); ok {
			ws.Row = int(v)
		}
		if v, ok := t.RawGetString("col").(lua.LNumber); ok {
			ws.Col = int(v)
		}
		switch v := t.RawGetString("text").(type) {
		case lua.LString:
			ws.Text = string(v)
		case *lua.LFunction:
			ws.TextFn = v // dynamic widget: run each refresh, use its return value
		}
		if fn, ok := t.RawGetString("on_click").(*lua.LFunction); ok {
			ws.OnClick = fn
		}
		if fn, ok := t.RawGetString("draw").(*lua.LFunction); ok {
			ws.Draw = fn
		}
		if fn, ok := t.RawGetString("component").(*lua.LFunction); ok {
			ws.Component = fn
		}
		if v, ok := t.RawGetString("width").(lua.LNumber); ok {
			ws.Width = int(v)
		}
		if v, ok := t.RawGetString("height").(lua.LNumber); ok {
			ws.Height = int(v)
		}
		if v, ok := t.RawGetString("interval").(lua.LNumber); ok {
			ws.Interval = int(v)
		}
		if v, ok := t.RawGetString("fg").(lua.LString); ok {
			if c, ok := colorNames[string(v)]; ok {
				ws.FG = c
			}
		}
		if v, ok := t.RawGetString("bg").(lua.LString); ok {
			if c, ok := colorNames[string(v)]; ok {
				ws.BG = c
			}
		}
		if v, ok := t.RawGetString("bold").(lua.LBool); ok && bool(v) {
			ws.Attr |= emu.AttrBold
		}
		if v, ok := t.RawGetString("dock").(lua.LString); ok {
			ws.Dock = string(v)
		}
		if v, ok := t.RawGetString("size").(lua.LNumber); ok {
			ws.Size = int(v)
		}
		cfg.Widgets = append(cfg.Widgets, ws)
		return 0
	}))

	// --- Widget query primitives ------------------------------------------------
	// These read the live gtmux state the client installs into binds.Hooks (the
	// cross-session snapshot + this client's context) and hand it to widget
	// text/on_click functions as Lua tables. No server round-trip at call time:
	// the snapshot is push-fed on the status tick and cached client-side.
	snap := func() *proto.StateSnapshot {
		if binds.Hooks.Snapshot != nil {
			if s := binds.Hooks.Snapshot(); s != nil {
				return s
			}
		}
		return &proto.StateSnapshot{}
	}
	ctxSession := func() string {
		if binds.Hooks.Context != nil {
			return binds.Hooks.Context()["session"]
		}
		return ""
	}
	findSession := func(s *proto.StateSnapshot, name string) *proto.SnapSession {
		if name == "" {
			name = ctxSession()
		}
		for i := range s.Sessions {
			if s.Sessions[i].Name == name {
				return &s.Sessions[i]
			}
		}
		return nil
	}
	pushPane := func(l *lua.LState, p proto.PaneInfo, sess string, win int) *lua.LTable {
		t := l.NewTable()
		t.RawSetString("number", lua.LNumber(p.Number))
		t.RawSetString("id", lua.LNumber(p.ID))
		t.RawSetString("command", lua.LString(p.Command))
		t.RawSetString("path", lua.LString(p.Path))
		t.RawSetString("title", lua.LString(p.Title))
		t.RawSetString("pid", lua.LNumber(p.PID))
		t.RawSetString("active", lua.LBool(p.Active))
		t.RawSetString("marked", lua.LBool(p.Marked))
		t.RawSetString("width", lua.LNumber(p.Width))
		t.RawSetString("height", lua.LNumber(p.Height))
		t.RawSetString("session", lua.LString(sess))
		t.RawSetString("window", lua.LNumber(win))
		return t
	}
	optString := func(l *lua.LState, key string) string {
		if l.GetTop() >= 1 {
			if t, ok := l.Get(1).(*lua.LTable); ok {
				if v, ok := t.RawGetString(key).(lua.LString); ok {
					return string(v)
				}
			}
		}
		return ""
	}

	// gtmux.sessions(): every session {name, windows, attached}.
	L.SetField(tbl, "sessions", L.NewFunction(func(l *lua.LState) int {
		arr := l.NewTable()
		for _, s := range snap().Sessions {
			t := l.NewTable()
			t.RawSetString("name", lua.LString(s.Name))
			t.RawSetString("windows", lua.LNumber(len(s.Windows)))
			t.RawSetString("attached", lua.LBool(s.Attached))
			arr.Append(t)
		}
		l.Push(arr)
		return 1
	}))

	// gtmux.windows({session=}): windows of a session (default: attached session).
	L.SetField(tbl, "windows", L.NewFunction(func(l *lua.LState) int {
		s := snap()
		arr := l.NewTable()
		if sess := findSession(s, optString(l, "session")); sess != nil {
			for _, w := range sess.Windows {
				t := l.NewTable()
				t.RawSetString("index", lua.LNumber(w.Index))
				t.RawSetString("name", lua.LString(w.Name))
				t.RawSetString("active", lua.LBool(w.Active))
				t.RawSetString("zoomed", lua.LBool(w.Zoomed))
				t.RawSetString("activity", lua.LBool(w.Activity))
				t.RawSetString("bell", lua.LBool(w.Bell))
				t.RawSetString("silence", lua.LBool(w.Silence))
				t.RawSetString("flags", lua.LString(windowFlags(w)))
				t.RawSetString("panes", lua.LNumber(len(w.Panes)))
				arr.Append(t)
			}
		}
		l.Push(arr)
		return 1
	}))

	// gtmux.panes({session=, window=}): panes of a window (default: the active
	// window of the attached session).
	L.SetField(tbl, "panes", L.NewFunction(func(l *lua.LState) int {
		winIdx, haveWin := 0, false
		if l.GetTop() >= 1 {
			if t, ok := l.Get(1).(*lua.LTable); ok {
				if v, ok := t.RawGetString("window").(lua.LNumber); ok {
					winIdx, haveWin = int(v), true
				}
			}
		}
		s := snap()
		arr := l.NewTable()
		if sess := findSession(s, optString(l, "session")); sess != nil {
			for _, w := range sess.Windows {
				if haveWin {
					if w.Index != winIdx {
						continue
					}
				} else if !w.Active {
					continue
				}
				for _, p := range w.Panes {
					arr.Append(pushPane(l, p, sess.Name, w.Index))
				}
			}
		}
		l.Push(arr)
		return 1
	}))

	// gtmux.find_panes({command=}): panes across ALL sessions whose command
	// contains the filter (empty = all). Backbone of the program-aware bar.
	L.SetField(tbl, "find_panes", L.NewFunction(func(l *lua.LState) int {
		filter := optString(l, "command")
		arr := l.NewTable()
		for _, sess := range snap().Sessions {
			for _, w := range sess.Windows {
				for _, p := range w.Panes {
					if filter != "" && !strings.Contains(p.Command, filter) {
						continue
					}
					arr.Append(pushPane(l, p, sess.Name, w.Index))
				}
			}
		}
		l.Push(arr)
		return 1
	}))

	// gtmux.clients(): every attached client {name, session, width, height}.
	L.SetField(tbl, "clients", L.NewFunction(func(l *lua.LState) int {
		arr := l.NewTable()
		for _, sess := range snap().Sessions {
			for _, c := range sess.Clients {
				t := l.NewTable()
				t.RawSetString("name", lua.LString(c.Name))
				t.RawSetString("session", lua.LString(c.Session))
				t.RawSetString("width", lua.LNumber(c.Width))
				t.RawSetString("height", lua.LNumber(c.Height))
				arr.Append(t)
			}
		}
		l.Push(arr)
		return 1
	}))

	// gtmux.context(): this client's {session, window, pane, prefix, width, height}.
	L.SetField(tbl, "context", L.NewFunction(func(l *lua.LState) int {
		t := l.NewTable()
		if binds.Hooks.Context != nil {
			for k, v := range binds.Hooks.Context() {
				t.RawSetString(k, lua.LString(v))
			}
		}
		l.Push(t)
		return 1
	}))

	// gtmux.expand(fmt): run the status-format expander (#{vars}/#client()/#server()).
	L.SetField(tbl, "expand", L.NewFunction(func(l *lua.LState) int {
		out := l.CheckString(1)
		if binds.Hooks.Expand != nil {
			out = binds.Hooks.Expand(out)
		}
		l.Push(lua.LString(out))
		return 1
	}))

	// gtmux.get_option(name): read a client option's current value as a string.
	L.SetField(tbl, "get_option", L.NewFunction(func(l *lua.LState) int {
		out := ""
		if binds.Hooks.Option != nil {
			out = binds.Hooks.Option(l.CheckString(1))
		}
		l.Push(lua.LString(out))
		return 1
	}))

	// --- Targeted write verbs (for on_click) -----------------------------------
	// gtmux.switch_session(name): attach this client to another session.
	L.SetField(tbl, "switch_session", L.NewFunction(func(l *lua.LState) int {
		name := l.CheckString(1)
		if l.GetTop() >= 2 { // optional window index (choose-tree): switch AND focus
			binds.ops = append(binds.ops, BindOp{Action: []string{"switch-session", name, fmt.Sprintf("%d", l.CheckInt(2))}})
		} else {
			binds.ops = append(binds.ops, BindOp{Action: []string{"switch-client", "-t", name}})
		}
		return 0
	}))
	// gtmux.kill_pane([id]): no arg kills the active pane; an id targets %id.
	L.SetField(tbl, "kill_pane", L.NewFunction(func(l *lua.LState) int {
		act := []string{"kill-pane"}
		if l.GetTop() >= 1 {
			act = append(act, "-t", fmt.Sprintf("%%%d", l.CheckInt(1)))
		}
		binds.ops = append(binds.ops, BindOp{Action: act})
		return 0
	}))
	// gtmux.send_keys(paneID, text): send text to pane %paneID.
	L.SetField(tbl, "send_keys", L.NewFunction(func(l *lua.LState) int {
		binds.ops = append(binds.ops, BindOp{Action: []string{"send-keys", "-t", fmt.Sprintf("%%%d", l.CheckInt(1)), l.CheckString(2)}})
		return 0
	}))

	// gtmux.open{component=, on_key=, width=, height=}: open a modal keyboard
	// widget — a centered component overlay that receives every key via on_key
	// until on_key calls ui:close(). The keyboard analog of a dock/float widget;
	// the basis for pickers/prompts/menus as components.
	L.SetField(tbl, "open", L.NewFunction(func(l *lua.LState) int {
		t := l.CheckTable(1)
		m := &ModalOpen{Width: 40, Height: 10}
		if fn, ok := t.RawGetString("component").(*lua.LFunction); ok {
			m.Component = fn
		}
		if fn, ok := t.RawGetString("on_key").(*lua.LFunction); ok {
			m.OnKey = fn
		}
		if v, ok := t.RawGetString("width").(lua.LNumber); ok {
			m.Width = int(v)
		}
		if v, ok := t.RawGetString("height").(lua.LNumber); ok {
			m.Height = int(v)
		}
		if v, ok := t.RawGetString("position").(lua.LString); ok {
			m.Position = string(v)
		}
		binds.ops = append(binds.ops, BindOp{Modal: m})
		return 0
	}))

	// gtmux.rename_window(name) / rename_session(name): the argv dispatch behind a
	// rename prompt component (the typed text is one arg, spaces preserved).
	L.SetField(tbl, "rename_window", L.NewFunction(func(l *lua.LState) int {
		binds.ops = append(binds.ops, BindOp{Action: []string{"rename-window", l.CheckString(1)}})
		return 0
	}))
	L.SetField(tbl, "rename_session", L.NewFunction(func(l *lua.LState) int {
		binds.ops = append(binds.ops, BindOp{Action: []string{"rename-session", l.CheckString(1)}})
		return 0
	}))

	// gtmux.buffers(): the attached session's paste buffers {name, preview}
	// (newest first), for a choose-buffer picker.
	L.SetField(tbl, "buffers", L.NewFunction(func(l *lua.LState) int {
		arr := l.NewTable()
		if sess := findSession(snap(), ctxSession()); sess != nil {
			for _, b := range sess.Buffers {
				t := l.NewTable()
				t.RawSetString("name", lua.LString(b.Name))
				t.RawSetString("preview", lua.LString(b.Preview))
				arr.Append(t)
			}
		}
		l.Push(arr)
		return 1
	}))
	// gtmux.paste_buffer(name): paste the named buffer into the active pane.
	L.SetField(tbl, "paste_buffer", L.NewFunction(func(l *lua.LState) int {
		binds.ops = append(binds.ops, BindOp{Action: []string{"paste-buffer", "-b", l.CheckString(1)}})
		return 0
	}))

	// gtmux.run_command(line): run a tmux command line — the client tokenizes it
	// (shell-like quoting) and dispatches. The dispatch behind a command-prompt
	// component; an empty line is a no-op.
	L.SetField(tbl, "run_command", L.NewFunction(func(l *lua.LState) int {
		binds.ops = append(binds.ops, BindOp{Command: l.CheckString(1)})
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
		if b, ok := parseKeyName(l.CheckString(1)); ok {
			binds.Binds[b] = l.CheckFunction(2)
		}
		return 0
	}))
	// bind_repeat is bind + tmux's -r: after firing, the key repeats without
	// re-pressing the prefix until the repeat window (client-side) lapses.
	L.SetField(tbl, "bind_repeat", L.NewFunction(func(l *lua.LState) int {
		if b, ok := parseKeyName(l.CheckString(1)); ok {
			binds.Binds[b] = l.CheckFunction(2)
			binds.Repeat[b] = true
		}
		return 0
	}))
	// on(event, fn) registers an alert callback (tmux's alert-bell/alert-activity/
	// alert-silence hooks). The client fires fn on a window's flag edge with a
	// table {event, session, window, name, command, title}.
	L.SetField(tbl, "on", L.NewFunction(func(l *lua.LState) int {
		event := l.CheckString(1)
		binds.Alerts[event] = append(binds.Alerts[event], l.CheckFunction(2))
		return 0
	}))
	// bind_root is tmux's bind -n: no prefix, the bare key fires it.
	L.SetField(tbl, "bind_root", L.NewFunction(func(l *lua.LState) int {
		if b, ok := parseKeyName(l.CheckString(1)); ok {
			binds.RootBinds[b] = l.CheckFunction(2)
		}
		return 0
	}))
	// bind_table(table, key, fn) is tmux's bind -T <table>: a key in a custom
	// table, reachable by first switching into it (key_table below).
	L.SetField(tbl, "bind_table", L.NewFunction(func(l *lua.LState) int {
		table := l.CheckString(1)
		if b, ok := parseKeyName(l.CheckString(2)); ok {
			if binds.Tables[table] == nil {
				binds.Tables[table] = map[string]*lua.LFunction{}
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
	// Swap gtmux.options to a fresh table so the user file's entries are tracked
	// separately from the defaults (provenance). Applied after the defaults
	// below, a user option deterministically wins over a default option that
	// writes the same config field (e.g. a future mode-style aliasing
	// copy_selection_fg) — the ForEach order within one table is otherwise
	// random, which made same-field aliases flaky.
	userOpts := L.NewTable()
	L.SetField(tbl, "options", userOpts)
	if data, err := os.ReadFile(path); err == nil {
		if err := L.DoString(string(data)); err != nil {
			log.Printf("gtmux: %s: %v (ignoring, using defaults)", path, err)
		}
	}

	// gtmux.options.X = Y entries feed the same registry: read each as a string
	// and applyOption it (unknown names are ignored). Default-file opts first,
	// then user-file opts, so a user option wins any same-field alias.
	applyOpts := func(t *lua.LTable) {
		t.ForEach(func(k, v lua.LValue) {
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
	}
	applyOpts(defOpts)
	applyOpts(userOpts)

	// Runtime set-option overrides, applied last so they win over the file.
	for _, o := range overrides {
		applyOption(&cfg, binds, o[0], o[1])
	}
	return cfg, binds
}
