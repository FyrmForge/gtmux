// Package proto defines the messages exchanged between gtmux client and server over gob.
package proto

import (
	"fmt"
	"os"

	"github.com/FyrmForge/gtmux/internal/emu"
)

// SockPath returns the Unix socket path for the gtmux daemon. One daemon
// serves every session; Attach.Session picks which one a client wants.
// GTMUX_SOCK overrides the default (mirrors tmux's -S), which the e2e harness
// uses to give each test its own isolated daemon.
func SockPath() string {
	if s := os.Getenv("GTMUX_SOCK"); s != "" {
		return s
	}
	return fmt.Sprintf("/tmp/gtmux-%d/server", os.Getuid())
}

// Attach is sent by the client immediately after connecting.
type Attach struct {
	Session string
	Cols    int
	Rows    int
	Cwd     string // only used to seed a brand-new session's first pane
	Create  bool   // `gtmux new`: create the session (error if it exists);
	// else `gtmux attach`: the session must already exist.
	// GroupTarget is `new-session -t <session>`: the new session joins that
	// session's group, displaying its current windows (a snapshot at creation).
	GroupTarget string

	// ReadOnly is `attach -r`: the client observes and can still drive prefix
	// commands, but its raw keystrokes never reach a pane.
	ReadOnly bool

	// StatusCmds are the #server(cmd) bodies this client's status formats use;
	// the server runs them each tick and streams the output back (the client
	// owns the formats, so it's the only one that knows which commands exist).
	// StatusInterval is the client's cache cadence for them, in seconds.
	StatusCmds     []string
	StatusInterval int

	// WantSnapshot: this client has widgets that query gtmux state, so the server
	// should build and stamp the cross-session StateSnapshot on its status ticks.
	// Off = the server skips that work entirely (no-widget clients pay nothing).
	WantSnapshot bool

	// Env is the client's environment, sent so the server can refresh the
	// update-environment vars (SSH_AUTH_SOCK, DISPLAY, …) into the session env
	// on attach/reattach — tmux's update-environment.
	Env map[string]string

	// Rows is the window (content) height: the client already subtracts the rows
	// its status bar reserves, so the server sizes the grid to Rows directly.
	// Status-bar reservation is entirely client-side.
}

// Input carries raw client key bytes to the server. Never contains mouse
// escape sequences — the client parses those out of its own stdin and
// sends a MouseEvent instead (see below).
type Input struct {
	Data []byte
}

// MouseEvent is a structured mouse report, decoded by the client from its
// terminal's raw SGR escape sequence rather than forwarded as bytes for the
// server to reparse. Cb keeps the xterm mouse protocol's own button/modifier/
// motion/wheel bit encoding (button in bits 0-1, motion 0x20, wheel 0x40,
// modifiers 0x04/0x08/0x10) since the server needs those bits verbatim to
// translate the event for a pane's own requested mouse protocol.
type MouseEvent struct {
	Cb    int
	X, Y  int // 1-based column/row, client-wide coordinates
	Press bool
}

// Resize notifies the server the client's terminal size changed. Rows is the
// window (content) height — the client has already subtracted its status rows.
type Resize struct {
	Cols int
	Rows int
}

// PaneRect is one pane's position, size, and focus state within its
// window's grid — enough for a client to draw its own dividers and know
// which pane's cursor to show.
type PaneRect struct {
	ID                   int
	Number               int // base-index-adjusted display number (display-panes overlay)
	Row, Col, Rows, Cols int
	Active               bool
	WantsMouse           bool   // the pane's app has requested mouse tracking (emu ModeMouseMask); the client forwards mouse here instead of owning the gesture
	KeyFlags             int    // the pane's app kitty-keyboard flags (0 = legacy); the client negotiates the same with its outer terminal (extended-keys)
	Marked               bool   // tagged by prefix+m, the join-pane source
	BorderRow            int    // window-row of this pane's pane-border-status label, or -1
	BorderLabel          string // expanded pane-border-format for that row
}

// BorderSeg is one divider line between panes. The client colors it per-cell
// against the active/marked pane rects it already has in Layout.Panes, so no
// active-adjacent flag rides along here.
type BorderSeg struct {
	Vertical   bool
	Fixed      int
	Start, End int
}

// Layout describes a window's pane arrangement: rects, dividers, and
// whether the display-panes number overlay (prefix+q) should be drawn.
// Sent whenever the arrangement changes (split, close, resize, window
// switch, attach) — a client uses it to compose its own chrome instead of
// receiving pre-drawn border/number glyphs mixed into pane content.
type Layout struct {
	Cols, Rows  int
	Panes       []PaneRect
	Borders     []BorderSeg
	ShowNumbers bool
}

// PaneContent is one pane's content diff, in that pane's own local
// coordinates (row 0 is the pane's first row, not the window's). A client
// places it according to the PaneRect it already has from Layout.
type PaneContent struct {
	PaneID        int
	Lines         map[int]emu.Line
	Cursor        emu.Cursor
	CursorVisible bool
}

// WindowInfo describes one window for the status bar's window list.
type WindowInfo struct {
	Index    int // 1-based, matching base-index 1 display
	Name     string
	Active   bool
	Zoomed   bool // active pane is zoomed (prefix+z) — shown as a Z flag
	Activity bool // monitor-activity: output seen while not current (# flag)
	Bell     bool // monitor-bell: a BEL seen while not current (! flag)
	Silence  bool // monitor-silence: no output for the interval while not current (~ flag)
	Panes    int  // pane count, for the client's choose-window picker
}

// StatusInfo carries the status bar's raw data — not pre-rendered, and not
// even pre-expanded: the client owns the status_left/status_right format
// strings and expands them itself. Vars is the per-tick variable map (host,
// session, window_name, git_branch, clock, pane_path, pane_command); ServerShell
// is the output of each #server(cmd) the client asked for, keyed by command.
// PromptLabel is "" outside a transient server message (run-shell output,
// command errors); when set, PromptText is the message to show.
type StatusInfo struct {
	Vars        map[string]string
	ServerShell map[string]string
	Windows     []WindowInfo
	PromptLabel string
	PromptText  string
	// Snapshot is the whole-server state the client caches each tick and exposes
	// to Lua widgets (gtmux.sessions()/windows()/panes()/clients()). Nil until a
	// client that uses widgets is attached (the server skips building it
	// otherwise — see wantSnapshot). ponytail: full tree every tick; fine for
	// tens of panes, move to on-demand query if it grows.
	Snapshot *StateSnapshot
}

// StateSnapshot is the cross-session view assembled by the server: every live
// session, its windows, and their panes. Each session self-reports its own
// summary into the registry on its 1s tick (so detached sessions stay fresh);
// the stamp reads the whole registry map. The client turns this into the Lua
// query tables — no separate data bus, just a fatter StatusInfo.
type StateSnapshot struct {
	Sessions []SnapSession
}

// SnapSession is one session's summary in a StateSnapshot.
type SnapSession struct {
	Name     string
	Attached bool
	Windows  []SnapWindow
	Clients  []SnapClient
	Buffers  []SnapBuffer
}

// SnapBuffer is one paste buffer exposed to gtmux.buffers() (for choose-buffer).
type SnapBuffer struct {
	Name    string
	Preview string // first line, truncated — for the list label
}

// SnapWindow is one window inside a SnapSession.
type SnapWindow struct {
	Index    int // base-index-adjusted display number
	Name     string
	Active   bool
	Zoomed   bool
	Activity bool // # flag (monitor-activity)
	Bell     bool // ! flag (monitor-bell)
	Silence  bool // ~ flag (monitor-silence)
	Panes    []PaneInfo
}

// PaneInfo is one pane exposed to gtmux.panes()/find_panes() in Lua.
type PaneInfo struct {
	Number        int
	ID            int
	Command       string
	Path          string
	Title         string
	PID           int
	Active        bool
	Marked        bool
	Width, Height int
}

// SnapClient is one attached client exposed to gtmux.clients().
type SnapClient struct {
	Name    string
	Session string
	Width   int
	Height  int
}

// OpenPrompt is the client-local descriptor for a text-entry prompt
// (rename-window, rename-session, or the command prompt). The client opens it
// from its own state on a keybind (no server round-trip); on Enter it sends the
// committed text back as an Action (see the Kind→verb mapping in the client).
// Prefill seeds the line so the user edits, not retypes.
type OpenPrompt struct {
	Kind    string // "session" | "window" | "command"
	Prefill string
}

// OpenPicker tells a client to open a selection list (choose-window /
// choose-session). Items are the display strings; Targets[i] is what selecting
// item i acts on (a window index, or a session name), sent back as {Verb,
// Target} in an Action. The client owns navigation and rendering.
type OpenPicker struct {
	Title   string
	Verb    string // "select-window" | "switch-session" | "run"
	Items   []string
	Targets []string
	// Filter enables type-to-filter: printable keys narrow the list live and
	// arrows navigate (choose-tree). Off for the plain j/k-navigated pickers.
	Filter bool
	// Previews is an optional per-item preview: the highlighted item's pane
	// content as styled lines (tmux choose-tree's pane preview), shown beside the
	// list. Static — captured when the picker opens, not live. nil = no preview.
	Previews [][]emu.Line
}

// Action is a client-invoked command run mid-session (prompt/picker commit,
// and — after the input refactor — every keybind). Args routes straight to the
// server's runCommand, the same surface `gtmux run` uses.
type Action struct {
	Args []string
}

// CopyModeEnter tells one client to enter copy-mode over a frozen snapshot of
// a pane's scrollback + current screen, captured atomically by the server (so
// there's no seam between history and live screen). The client owns copy-mode
// from here: movement, search, selection, and yank all run locally over Lines.
// CursorY/CursorX are the initial cursor, as an index into Lines.
type CopyModeEnter struct {
	PaneID           int
	Lines            []emu.Line
	CursorY, CursorX int
	Select           bool // start a selection anchored at the cursor (drag-to-copy)
}

// SetPasteBuffer sets the server's paste buffer — sent by a client after a
// copy-mode yank so prefix+] paste still works across clients. The client also
// writes OSC 52 to its own terminal for the system clipboard.
type SetPasteBuffer struct {
	Text string
	// Pipe marks a copy-mode yank (vs. a plain buffer set): the server also pipes
	// Text through the copy-command option, if set.
	Pipe bool
}

// ListRequest asks the server for the sessions it currently holds. Sent
// instead of Attach; the server replies once with SessionList and closes
// the connection.
type ListRequest struct{}

// SessionInfo describes one live session for `gtmux list`.
type SessionInfo struct {
	Name     string
	Windows  int
	Attached bool
}

// SessionList is the server's reply to a ListRequest.
type SessionList struct {
	Sessions []SessionInfo
}

// KillServerRequest asks the server to shut down entirely. Sent instead of
// Attach; the server replies once with Ack and then exits.
type KillServerRequest struct{}

// KillSessionRequest asks the server to tear down a named session. Sent
// instead of Attach; the server replies once with Ack and closes the
// connection.
type KillSessionRequest struct {
	Name string
}

// HasSessionRequest checks whether a session exists (tmux has-session); the
// server replies with an Ack whose Ok reports existence, then closes.
type HasSessionRequest struct {
	Name string
}

// NewSessionRequest creates a session WITHOUT attaching (`gtmux new -d`, tmux's
// new-session -d): the session's owner goroutine runs independently, so a client
// can build it via `run` and attach later. Cwd seeds its first pane; GroupTarget
// joins a group. The server Acks and closes; Ok=false if the name is taken.
type NewSessionRequest struct {
	Name        string
	Cwd         string
	GroupTarget string
}

// RenameSessionRequest asks the server to rename a session. Sent instead of
// Attach; the server replies once with Ack and closes the connection.
type RenameSessionRequest struct {
	Old, New string
}

// CommandRequest asks the server to run one command-mode command (`gtmux run
// <session> <command...>`) in the named session. Args stay an array (not a
// joined line) so shell quoting survives: send-keys "make test" Enter. Sent
// instead of Attach; the server replies once with Ack and closes the
// connection.
type CommandRequest struct {
	Session string
	Args    []string
}

// Ack is the server's reply to any one-shot, non-Attach request
// (KillSessionRequest, RenameSessionRequest, ...).
type Ack struct {
	Ok  bool
	Err string
	Out string // stdout text from info commands (display-message, list-panes)
}

// ClientMsg is the envelope for everything a client sends to the server.
// ponytail: one envelope with optional fields, simplest way to multiplex a few
// message types over a single gob stream. Revisit if the type list grows large.
type ClientMsg struct {
	Attach        *Attach
	Input         *Input
	Mouse         *MouseEvent
	Resize        *Resize
	List          *ListRequest
	KillSession   *KillSessionRequest
	HasSession    *HasSessionRequest
	NewSession    *NewSessionRequest
	KillServer    *KillServerRequest
	RenameSession *RenameSessionRequest
	Command       *CommandRequest
	SetPaste      *SetPasteBuffer
	Action        *Action
	ResizeBorder  *ResizeBorder
	CopyDrag      *CopyDrag
}

// ResizeBorder drags a pane divider to an absolute position. The client
// recognizes the border-drag gesture from the Layout.Borders it already holds
// (client-owned mouse); Index selects that border, Pos is the target column
// (vertical divider) or row (horizontal), in window coordinates. The server
// maps the index to the split node and sets its fraction — it never inspects
// raw mouse coordinates to discover the gesture.
type ResizeBorder struct {
	Index int
	Pos   int
}

// CopyDrag asks the server to enter copy-mode on a pane with a selection
// anchored at (Row,Col) in window coordinates — tmux's drag-to-copy over a
// pane whose app isn't tracking the mouse. Only the server can build the
// scrollback snapshot, so the client (which recognized the drag) requests it
// here and the server replies with a CopyModeEnter carrying Select=true.
type CopyDrag struct {
	PaneID   int
	Row, Col int
}

// ServerMsg is the envelope for everything the server sends to a client.
// Layout/PaneContent/Status are independently optional: a message might
// carry just a Status update (the clock ticking), just one pane's content
// (a keystroke), or a full Layout+PaneContent set (attach, split, resize).
type ServerMsg struct {
	Layout        *Layout
	PaneContent   []PaneContent
	Status        *StatusInfo
	SessionList   *SessionList
	Ack           *Ack
	CopyModeEnter *CopyModeEnter
	OpenPicker    *OpenPicker
	// SwitchSession asks the client to reconnect and attach to this session
	// instead; the server closes the connection right after sending it.
	SwitchSession string
	// SetOption pushes a client-owned option (set via `gtmux run … set-option`
	// or another client) for the client to apply live to its own config.
	SetOption *SetOption
	// Popup drives the display-popup overlay (open/content/close).
	Popup *PopupMsg
	// ClientAction asks the client to dispatch a client-owned command locally
	// (command-prompt / confirm-before / display-menu) — used when a hook fires
	// one of them server-side, where it would otherwise no-op.
	ClientAction []string
	// Passthrough is raw bytes (an app's un-doubled allow-passthrough DCS payload)
	// the client writes straight to its terminal, bypassing the compositor.
	Passthrough []byte
	// CommandExits reports OSC 133 command-finished events (a command run in a
	// pane exited) so the client can fire gtmux.on("command-exited", …).
	CommandExits []CommandExit
}

// CommandExit is one OSC 133 D (command-finished) event: a command run in a
// pane exited with ExitCode. Window is the base-index-adjusted display number.
type CommandExit struct {
	Session  string
	Window   int
	PaneID   int
	ExitCode int
}

// PopupMsg drives the client's display-popup overlay — a floating terminal the
// server runs a command in. Open carries the content dimensions (the client
// centers a bordered box of that size); Content is a diff in popup-local coords
// (reusing the pane-diff shape); Close tears it down. Session-scoped: broadcast
// to every attached client.
type PopupMsg struct {
	Open    bool
	Close   bool
	Cols    int // content width (on Open)
	Rows    int // content height (on Open)
	X       int // box left column, or -1 to center horizontally (on Open)
	Y       int // box top row, or -1 to center vertically (on Open)
	Content *PaneContent
}

// SetOption is one live client-option change pushed from the server.
type SetOption struct {
	Name, Value string
}
