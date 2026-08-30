package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/FyrmForge/gtmux/internal/emu"
	"github.com/FyrmForge/gtmux/internal/proto"
)

// lastWidget returns the widget a test's own lua registered. The default config
// now ships a status component (widget index 0), and user widgets append after
// it, so a single-widget test's widget is the last one.
func lastWidget(cfg ClientConfig) WidgetSpec { return cfg.Widgets[len(cfg.Widgets)-1] }

func TestLoadClientMissingFileUsesDefaults(t *testing.T) {
	cfg, binds := LoadClient("/nonexistent/gtmux/client.lua")
	defer binds.Close()
	// default_client.lua ships a status component (a Lua fn) that can't live in
	// the Go DefaultClientConfig(); compare the option defaults only.
	cfg.Widgets = nil
	if !reflect.DeepEqual(cfg, DefaultClientConfig()) {
		t.Errorf("missing file: got %+v, want defaults %+v", cfg, DefaultClientConfig())
	}
}

// The embedded default_client.lua must set every option to exactly its Go
// default, so `init-config` produces a file that round-trips to the same
// config — a guard against the template drifting from DefaultClientConfig.
func TestDefaultClientLuaMatchesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	if err := os.WriteFile(path, []byte(defaultClientLua), 0o644); err != nil {
		t.Fatal(err)
	}
	got, binds := LoadClient(path)
	defer binds.Close()
	got.Widgets = nil // the status component is a Lua fn, not a Go option default
	if !reflect.DeepEqual(got, DefaultClientConfig()) {
		t.Errorf("default_client.lua loads as %+v, want defaults %+v", got, DefaultClientConfig())
	}
}

// TestDefaultBindsResolve pins every default keybind to the op it emits — the
// Stage-4 bug class ("key X emits the wrong action") that e2e can't afford to
// cover exhaustively. prefix is C-b (0x02).
func TestDefaultBindsResolve(t *testing.T) {
	_, binds := LoadClient("/nonexistent/gtmux/client.lua")
	defer binds.Close()

	if binds.Prefix != 0x02 {
		t.Fatalf("prefix = %#x, want C-b (0x02)", binds.Prefix)
	}

	want := map[string]BindOp{
		"c": {Action: []string{"new-window"}},
		"n": {Action: []string{"next-window"}},
		"p": {Action: []string{"previous-window"}},
		"%": {Action: []string{"split-window", "-h"}},
		"\"": {Action: []string{"split-window"}},
		"x": {Action: []string{"kill-pane"}},
		"d": {Action: []string{"detach"}},
		"q": {Action: []string{"display-panes"}},
		"z": {Action: []string{"resize-pane", "-Z"}},
		"{": {Action: []string{"swap-pane", "-U"}},
		"}": {Action: []string{"swap-pane"}},
		"<": {Action: []string{"swap-window", "-L"}},
		">": {Action: []string{"swap-window"}},
		"!": {Action: []string{"break-pane"}},
		"m": {Action: []string{"mark-pane"}},
		"J": {Action: []string{"join-marked"}},
		// "s" is now a modal picker component (gtmux.open), asserted separately below.
		"[": {Action: []string{"copy-mode"}},
		"]": {Action: []string{"paste"}},
		// "$" / "," are now modal prompt components (gtmux.open), asserted below.
		// ":" is now a modal command-prompt component (gtmux.open), asserted below.
		// "w" is now a modal choose-tree component (gtmux.open), asserted below.
		// "W" is now a modal picker component (gtmux.open), asserted below.
	}
	for key, exp := range want {
		ops := binds.Resolve(key)
		if len(ops) != 1 {
			t.Errorf("key %q: got %d ops, want 1 (%+v)", key, len(ops), ops)
			continue
		}
		got := ops[0]
		if got.Local != exp.Local || !equalStrs(got.Action, exp.Action) {
			t.Errorf("key %q: got %+v, want %+v", key, got, exp)
		}
	}
	// prefix+s / prefix+W open modal picker components (gtmux.open), not server
	// actions.
	for _, key := range []string{"s", "W", "$", ",", ":", "w"} {
		if ops := binds.Resolve(key); len(ops) != 1 || ops[0].Modal == nil {
			t.Errorf("key %s: got %+v, want a Modal open", key, ops)
		}
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestLoadClientOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	if err := os.WriteFile(path, []byte(`
gtmux.options.mouse = false
gtmux.options.status_style = "fg=red"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, binds := LoadClient(path)
	defer binds.Close()
	if cfg.Mouse {
		t.Error("mouse should be overridden to false")
	}
	if cfg.StatusFG != emu.Red {
		t.Errorf("status_style fg = %v, want emu.Red", cfg.StatusFG)
	}
	if cfg.StatusBG != DefaultClientConfig().StatusBG {
		t.Errorf("status_style bg should stay default when only fg set, got %v", cfg.StatusBG)
	}
}

// TestFlowPromptEncoding pins how the command_prompt/confirm_before primitives
// encode their args into the Action the client's dispatch intercepts.
func TestFlowPromptEncoding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	if err := os.WriteFile(path, []byte(`
gtmux.bind("W", function() gtmux.command_prompt("new name:", "old", "rename-window %1") end)
gtmux.bind("K", function() gtmux.confirm_before("kill-window", "kill? (y/n)") end)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, binds := LoadClient(path)
	defer binds.Close()

	wantW := []string{"command-prompt", "-p", "new name:", "-I", "old", "--", "rename-window", "%1"}
	if ops := binds.Resolve("W"); len(ops) != 1 || !reflect.DeepEqual(ops[0].Action, wantW) {
		t.Errorf("command_prompt encoded as %v, want %v", ops, wantW)
	}
	wantK := []string{"confirm-before", "-p", "kill? (y/n)", "--", "kill-window"}
	if ops := binds.Resolve("K"); len(ops) != 1 || !reflect.DeepEqual(ops[0].Action, wantK) {
		t.Errorf("confirm_before encoded as %v, want %v", ops, wantK)
	}
}

// display_menu is now a component (default_client.lua overrides the verb): it
// opens a modal menu, not a server display-menu action.
func TestDisplayMenuOpensModal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	if err := os.WriteFile(path, []byte(`
gtmux.bind("M", function() gtmux.display_menu("go", "new", "new-window", "kill", "kill-pane") end)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, binds := LoadClient(path)
	defer binds.Close()
	if ops := binds.Resolve("M"); len(ops) != 1 || ops[0].Modal == nil {
		t.Errorf("display_menu should open a Modal, got %+v", ops)
	}
}

// TestApplyOptionRegistry checks the single client-option registry: known names
// mutate config, unknown names report false (so a caller can route elsewhere).
func TestApplyOptionRegistry(t *testing.T) {
	cfg := DefaultClientConfig()
	binds := &ClientBinds{Prefix: 0x02}
	if !applyOption(&cfg, binds, "status_style", "bg=red") || cfg.StatusBG != emu.Red {
		t.Errorf("status_style bg=red not applied: %v", cfg.StatusBG)
	}
	if !applyOption(&cfg, binds, "status_left", "[X]") || cfg.StatusLeft != "[X]" {
		t.Errorf("status_left not applied: %q", cfg.StatusLeft)
	}
	if !applyOption(&cfg, binds, "prefix", "C-a") || binds.Prefix != 0x01 {
		t.Errorf("prefix C-a not applied: %#x", binds.Prefix)
	}
	if applyOption(&cfg, binds, "main_pane_width", "80") {
		t.Errorf("server option main_pane_width should not be a known client option")
	}
}

// TestLoadClientWithOverrides checks a runtime set-option override wins over the
// file's value and is re-derived from the file (not merged into stale state).
func TestLoadClientWithOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client.lua")
	if err := os.WriteFile(path, []byte(`gtmux.options.status_style = "bg=green"`), 0o644); err != nil {
		t.Fatal(err)
	}
	base, b1 := LoadClientWith(path, nil)
	b1.Close()
	if base.StatusBG != emu.Green {
		t.Fatalf("file value not loaded: %v", base.StatusBG)
	}
	over, b2 := LoadClientWith(path, [][2]string{{"status_style", "bg=red"}})
	b2.Close()
	if over.StatusBG != emu.Red {
		t.Fatalf("override did not win over file: %v", over.StatusBG)
	}
}

// message_style / mode_style / status-*-length round-trip through the loader.
func TestLoadClientStyleOptions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	if err := os.WriteFile(path, []byte(`
gtmux.options.message_style = "fg=red,bg=blue"
gtmux.options.status_left_length = "12"
gtmux.options.status_right_length = "8"
gtmux.options.pane_border_style = "fg=cyan,bg=magenta"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, binds := LoadClient(path)
	defer binds.Close()
	if cfg.MessageFG != emu.Red || cfg.MessageBG != emu.Blue {
		t.Errorf("message_style = %v/%v, want red/blue", cfg.MessageFG, cfg.MessageBG)
	}
	if cfg.StatusLeftLength != 12 || cfg.StatusRightLength != 8 {
		t.Errorf("status length = %d/%d, want 12/8", cfg.StatusLeftLength, cfg.StatusRightLength)
	}
	if cfg.InactiveBorderFG != emu.Cyan || cfg.InactiveBorderBG != emu.Magenta {
		t.Errorf("pane_border_style = %v/%v, want cyan/magenta", cfg.InactiveBorderFG, cfg.InactiveBorderBG)
	}
}

// TestLoadClientMultiLineStatus: the `status` count and per-extra-line formats
// round-trip through the loader.
func TestLoadClientMultiLineStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	if err := os.WriteFile(path, []byte(`
gtmux.options.status = "3"
gtmux.options.status_format_2 = "L2"
gtmux.options.status_format_5 = "L5"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, binds := LoadClient(path)
	defer binds.Close()
	if cfg.StatusLines != 3 {
		t.Errorf("status = %d, want 3", cfg.StatusLines)
	}
	if cfg.ExtraStatusFormats[0] != "L2" || cfg.ExtraStatusFormats[3] != "L5" {
		t.Errorf("extra formats = %q, want [L2,,,L5]", cfg.ExtraStatusFormats)
	}
	if cfg.ExtraStatusFormats[1] != "" {
		t.Errorf("status_format_3 unset should be empty, got %q", cfg.ExtraStatusFormats[1])
	}
}

// #3: splitFields keeps a quoted nested command as one template field (the
// command-prompt quoting fix; strings.Fields would have split it).
func TestSplitFieldsQuoting(t *testing.T) {
	got := splitFields(`new-window 'workspacer -W=current %1 %2 ; read'`)
	want := []string{"new-window", "workspacer -W=current %1 %2 ; read"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("splitFields = %q, want %q", got, want)
	}
}

// #4/#5/#6: the new client options parse and default correctly.
func TestNewClientOptions(t *testing.T) {
	cfg := DefaultClientConfig()
	if cfg.StatusKeys != "emacs" || cfg.SetClipboard != "external" || cfg.CopyWheelLines != 3 || !cfg.CopyDragFinish {
		t.Fatalf("defaults: %+v", cfg)
	}
	binds := &ClientBinds{}
	applyOption(&cfg, binds, "status_keys", "vi")
	applyOption(&cfg, binds, "set_clipboard", "off")
	applyOption(&cfg, binds, "copy_wheel_lines", "5")
	applyOption(&cfg, binds, "copy_drag_finish", "false")
	if cfg.StatusKeys != "vi" || cfg.SetClipboard != "off" || cfg.CopyWheelLines != 5 || cfg.CopyDragFinish {
		t.Errorf("after set: %+v", cfg)
	}
}

// gtmux.widget registers a static overlay widget onto the config, styled from
// color names, text split on newlines — the first slice of the widget system.
// gtmux.on registers an alert callback; RunAlert fires it with the window
// table and returns any BindOps the callback recorded (here via run_command).
func TestAlertOnCallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	src := `
seen = nil
gtmux.on("alert-bell", function(w)
  seen = w.session .. "/" .. w.window .. "/" .. w.command
  if w.command == "claude" then gtmux.run_command("display-message done") end
end)
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, binds := LoadClient(path)
	_ = cfg
	defer binds.Close()

	ops := binds.RunAlert(AlertEvent{Event: "alert-bell", Session: "work", Window: 2, Command: "claude"})
	if got := binds.l.GetGlobal("seen").String(); got != "work/2/claude" {
		t.Errorf("callback saw %q, want work/2/claude", got)
	}
	if len(ops) != 1 || ops[0].Command != "display-message done" {
		t.Errorf("ops = %+v, want one display-message command", ops)
	}
	// An event with no registered callback returns nil ops and doesn't panic.
	if ops := binds.RunAlert(AlertEvent{Event: "alert-silence"}); ops != nil {
		t.Errorf("unregistered event returned %+v, want nil", ops)
	}
}

// gtmux.on("command-exited", …) gets a pane object; pane:set_border records a
// PaneBorder op the client applies to the compositor.
func TestCommandExitedSetBorder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	src := `
seen = nil
gtmux.on("command-exited", function(p)
  seen = p.session .. "/" .. p.window .. "/" .. p.id .. "/" .. p.exit_code
  if p.exit_code ~= 0 then p:set_border("red") end
end)
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	_, binds := LoadClient(path)
	defer binds.Close()

	// Failure → set_border op.
	ops := binds.RunCommandExit(CommandExitEvent{Session: "work", Window: 3, PaneID: 42, ExitCode: 1})
	if got := binds.l.GetGlobal("seen").String(); got != "work/3/42/1" {
		t.Errorf("callback saw %q, want work/3/42/1", got)
	}
	if len(ops) != 1 || ops[0].Border == nil || ops[0].Border.PaneID != 42 || ops[0].Border.Color != "red" {
		t.Fatalf("ops = %+v, want one set_border(42,red)", ops)
	}
	// Success → no op.
	if ops := binds.RunCommandExit(CommandExitEvent{PaneID: 42, ExitCode: 0}); ops != nil {
		t.Errorf("exit 0 recorded ops %+v, want none", ops)
	}
}

// gtmux.on("program-changed", …) gets a pane object with command + from, and
// the shared set_border method still records a PaneBorder op.
func TestProgramChangedCallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	src := `
seen = nil
gtmux.on("program-changed", function(p)
  seen = p.session .. "/" .. p.id .. ":" .. p.from .. "->" .. p.command
  if p.command == "vim" then p:set_border("green") end
end)
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	_, binds := LoadClient(path)
	defer binds.Close()

	ops := binds.RunProgramChanged("work", 1, 9, "vim", "zsh")
	if got := binds.l.GetGlobal("seen").String(); got != "work/9:zsh->vim" {
		t.Errorf("callback saw %q, want work/9:zsh->vim", got)
	}
	if len(ops) != 1 || ops[0].Border == nil || ops[0].Border.PaneID != 9 || ops[0].Border.Color != "green" {
		t.Fatalf("ops = %+v, want one set_border(9,green)", ops)
	}
}

func TestLoadClientWidget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	if err := os.WriteFile(path, []byte(`
gtmux.widget{ row = 2, col = 5, text = "hi\nthere", fg = "black", bg = "green", bold = true }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, binds := LoadClient(path)
	defer binds.Close()
	w := lastWidget(cfg) // the default status component is Widgets[0]; ours is last
	if w.Row != 2 || w.Col != 5 {
		t.Errorf("pos = (%d,%d), want (2,5)", w.Row, w.Col)
	}
	if w.Text != "hi\nthere" {
		t.Errorf("text = %q, want %q", w.Text, "hi\nthere")
	}
	if w.FG != emu.Black || w.BG != emu.Green || w.Attr&emu.AttrBold == 0 {
		t.Errorf("style = fg %v/bg %v/attr %d, want black/green/bold", w.FG, w.BG, w.Attr)
	}
}

// A function-widget's text fn runs on the bind VM (RunText) and reads the live
// snapshot the client installs into Hooks — this is the whole query pipeline:
// gtmux.sessions()/panes() turn the pushed StateSnapshot into Lua tables. The
// widget formats them and returns a string.
func TestWidgetTextFnReadsSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	src := `
gtmux.widget{ dock = "left", size = 12, text = function()
  local out = {}
  for _, s in ipairs(gtmux.sessions()) do
    out[#out+1] = s.name .. ":" .. s.windows
  end
  for _, p in ipairs(gtmux.panes()) do
    out[#out+1] = "p" .. p.number .. "=" .. p.command
  end
  return table.concat(out, "\n")
end }
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, binds := LoadClient(path)
	defer binds.Close()
	if lastWidget(cfg).TextFn == nil {
		t.Fatalf("expected a function-widget, got %+v", cfg.Widgets)
	}
	// Install a fake snapshot + context, as the client does at runtime.
	binds.Hooks = WidgetHooks{
		Snapshot: func() *proto.StateSnapshot {
			return &proto.StateSnapshot{Sessions: []proto.SnapSession{
				{Name: "work", Windows: []proto.SnapWindow{
					{Index: 1, Active: true, Panes: []proto.PaneInfo{
						{Number: 1, Command: "zsh"}, {Number: 2, Command: "vim"},
					}},
				}},
				{Name: "play", Windows: []proto.SnapWindow{{Index: 1}}},
			}}
		},
		Context: func() map[string]string { return map[string]string{"session": "work"} },
	}
	got := binds.RunText(lastWidget(cfg).TextFn)
	want := "work:1\nplay:1\np1=zsh\np2=vim"
	if got != want {
		t.Fatalf("widget text = %q, want %q", got, want)
	}
}

// The remaining query primitives (find_panes/clients/windows/context/expand/
// get_option) each turn the installed hooks into Lua values. One widget fn
// exercises all of them against a fake snapshot so a typo in any registration
// fails loudly.
func TestWidgetQueryPrimitives(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	src := `
gtmux.widget{ text = function()
  local parts = {}
  -- find_panes filters by command across all sessions
  for _, p in ipairs(gtmux.find_panes({ command = "claude" })) do
    parts[#parts+1] = "find=" .. p.session .. ":" .. p.command
  end
  -- clients() flattens across sessions
  parts[#parts+1] = "clients=" .. #gtmux.clients()
  -- windows() of a named session
  parts[#parts+1] = "wins=" .. #gtmux.windows({ session = "work" })
  -- context / expand / get_option
  parts[#parts+1] = "ctx=" .. gtmux.context().session
  parts[#parts+1] = "exp=" .. gtmux.expand("#{x}")
  parts[#parts+1] = "opt=" .. gtmux.get_option("status_interval")
  return table.concat(parts, " ")
end }
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, binds := LoadClient(path)
	defer binds.Close()
	binds.Hooks = WidgetHooks{
		Snapshot: func() *proto.StateSnapshot {
			return &proto.StateSnapshot{Sessions: []proto.SnapSession{
				{Name: "work", Windows: []proto.SnapWindow{{Index: 1, Active: true, Panes: []proto.PaneInfo{
					{Number: 1, Command: "zsh"}, {Number: 2, Command: "claude"},
				}}}, Clients: []proto.SnapClient{{Name: "work:1"}}},
				{Name: "srv", Windows: []proto.SnapWindow{{Index: 1, Panes: []proto.PaneInfo{
					{Number: 1, Command: "claude"},
				}}}},
			}}
		},
		Context: func() map[string]string { return map[string]string{"session": "work"} },
		Expand:  func(s string) string { return "EXP" },
		Option:  func(name string) string { return "42" },
	}
	got := binds.RunText(lastWidget(cfg).TextFn)
	want := "find=work:claude find=srv:claude clients=1 wins=1 ctx=work exp=EXP opt=42"
	if got != want {
		t.Fatalf("query primitives = %q, want %q", got, want)
	}
}

// gtmux.windows() exposes per-window flags for a status component to reach
// renderBar parity: the raw bools plus a `flags` string in tmux's order
// (* current, # activity, ! bell, ~ silence, Z zoomed).
func TestWindowsExposesFlags(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	src := `
gtmux.widget{ text = function()
  local out = {}
  for _, w in ipairs(gtmux.windows({ session = "work" })) do
    out[#out+1] = w.name .. "=" .. w.flags
  end
  return table.concat(out, " ")
end }
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, binds := LoadClient(path)
	defer binds.Close()
	binds.Hooks = WidgetHooks{
		Snapshot: func() *proto.StateSnapshot {
			return &proto.StateSnapshot{Sessions: []proto.SnapSession{
				{Name: "work", Windows: []proto.SnapWindow{
					{Index: 1, Name: "a", Active: true, Zoomed: true}, // *Z
					{Index: 2, Name: "b", Bell: true, Silence: true},  // !~
					{Index: 3, Name: "c", Activity: true},             // #
				}},
			}}
		},
	}
	got := binds.RunText(lastWidget(cfg).TextFn)
	want := "a=*Z b=!~ c=#"
	if got != want {
		t.Fatalf("window flags = %q, want %q", got, want)
	}
}

// on_click records the targeted action the client dispatches, and the hit table
// carries what was clicked (the session name on that dock line).
func TestWidgetOnClickRecordsAction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	src := `
gtmux.widget{ dock = "left", size = 12,
  text = "x",
  on_click = function(hit) gtmux.switch_session(hit.line_text) end }
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, binds := LoadClient(path)
	defer binds.Close()
	if lastWidget(cfg).OnClick == nil {
		t.Fatal("expected on_click function")
	}
	ops := binds.RunClick(lastWidget(cfg).OnClick, 0, "play", 3)
	if len(ops) != 1 || len(ops[0].Action) != 3 ||
		ops[0].Action[0] != "switch-client" || ops[0].Action[2] != "play" {
		t.Fatalf("on_click ops = %+v, want switch-client -t play", ops)
	}
}

// A draw-widget paints into a Canvas via c:box/c:text/c:hline — verify the grid
// gets the border corners, a positioned label, and a separator row, with the
// per-primitive style applied.
func TestWidgetDrawCanvas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	src := `
gtmux.widget{ dock = "top", size = 4, draw = function(c)
  c:box(0, 0, c.w, c.h, "fg=cyan")
  c:text(2, 0, "HI", "fg=red")
  c:hline(2)
end }
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, binds := LoadClient(path)
	defer binds.Close()
	if lastWidget(cfg).Draw == nil {
		t.Fatal("expected a draw function")
	}
	cv, _, _ := binds.RunDraw(lastWidget(cfg).Draw, 6, 4, emu.White, emu.Black, 0)
	// Corners of the box.
	if g, _ := cv.At(0, 0); g.Char != '┌' {
		t.Errorf("top-left = %q, want ┌", g.Char)
	}
	if g, _ := cv.At(5, 3); g.Char != '┘' {
		t.Errorf("bottom-right = %q, want ┘", g.Char)
	}
	// Positioned label with its own style.
	g, _ := cv.At(2, 0)
	if g.Char != 'H' || g.FG != emu.Red {
		t.Errorf("label cell = %q fg %v, want H/red", g.Char, g.FG)
	}
	// Separator row filled with ─.
	if g, _ := cv.At(3, 2); g.Char != '─' {
		t.Errorf("hline cell = %q, want ─", g.Char)
	}
	// Undrawn interior keeps the base style (space, white/black).
	if g, _ := cv.At(1, 1); g.Char != ' ' || g.FG != emu.White || g.BG != emu.Black {
		t.Errorf("interior = %q fg %v bg %v, want space white/black", g.Char, g.FG, g.BG)
	}
}

// A component (two-arg fn(props, ui)) can nest a child in a sub-rect: the
// child's draws land at the child's offset, its text is clipped to the child's
// box, and its on_click registers a Region in widget-LOCAL coords (offset
// accumulated from the component root, never a physical/host offset).
func TestComponentChildOffsetRegion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	src := `
gtmux.widget{ dock = "left", size = 10, component = function(props, ui)
  ui:child(2, 1, 3, 1, function(p, c)
    c:text(0, 0, "BTNZZ")                 -- 5 chars into a width-3 child: clipped
    c:on_click(function() gtmux.switch_session("x") end)
  end)
end }
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, binds := LoadClient(path)
	defer binds.Close()
	if lastWidget(cfg).Component == nil {
		t.Fatal("expected a component function")
	}
	cv, regions, _, _ := binds.RunComponent(lastWidget(cfg).Component, nil, 10, 4, emu.White, emu.Black, 0)
	// Child text painted at the child's offset (2,1)..(4,1).
	if g, _ := cv.At(2, 1); g.Char != 'B' {
		t.Errorf("child (2,1) = %q, want B", g.Char)
	}
	if g, _ := cv.At(4, 1); g.Char != 'N' {
		t.Errorf("child (4,1) = %q, want N", g.Char)
	}
	// Clipped: the 4th/5th chars fall outside the width-3 child box.
	if g, _ := cv.At(5, 1); g.Char != ' ' {
		t.Errorf("child (5,1) = %q, want blank (clipped to child box)", g.Char)
	}
	// Nothing painted outside the child.
	if g, _ := cv.At(0, 0); g.Char != ' ' {
		t.Errorf("(0,0) = %q, want blank", g.Char)
	}
	// Exactly one region, at the child's widget-local rect.
	if len(regions) != 1 {
		t.Fatalf("regions = %d, want 1", len(regions))
	}
	if r := regions[0]; r.X != 2 || r.Y != 1 || r.W != 3 || r.H != 1 {
		t.Errorf("region = {X:%d Y:%d W:%d H:%d}, want {2 1 3 1}", r.X, r.Y, r.W, r.H)
	}
}

// ui:state() is a per-widget store that survives redraws: a component reads it
// each render, an on_click mutates it, and re-rendering with the SAME state
// table (as the click path does) reflects the change. This is the reactive
// loop — no dirty-tracking, just state change -> re-render.
func TestComponentStatePersistsAndReacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	src := `
gtmux.widget{ dock = "left", size = 8, component = function(props, ui)
  local st = ui:state()
  st.n = st.n or 0
  ui:text(0, 0, "n=" .. st.n)
  ui:on_click(function() st.n = st.n + 1 end)
end }
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, binds := LoadClient(path)
	defer binds.Close()
	fn := lastWidget(cfg).Component

	rowText := func(cv *Canvas, n int) string {
		rs := make([]rune, n)
		for x := 0; x < n; x++ {
			g, _ := cv.At(x, 0)
			rs[x] = g.Char
		}
		return string(rs)
	}

	// First render: fresh state, n=0. Keep the returned state table.
	cv, regions, state, _ := binds.RunComponent(fn, nil, 8, 1, emu.White, emu.Black, 0)
	if got := rowText(cv, 3); got != "n=0" {
		t.Fatalf("first render = %q, want n=0", got)
	}
	if len(regions) != 1 {
		t.Fatalf("regions = %d, want 1", len(regions))
	}
	// Click twice, re-rendering with the persisted state each time.
	for want := 1; want <= 2; want++ {
		binds.RunClick(regions[0].OnClick, 0, "", 0)
		var cv2 *Canvas
		cv2, regions, state, _ = binds.RunComponent(fn, state, 8, 1, emu.White, emu.Black, 0)
		if got := rowText(cv2, 3); got != "n="+string(rune('0'+want)) {
			t.Fatalf("after %d clicks = %q, want n=%d (state must persist)", want, got, want)
		}
	}
}

// hline crossing a box border merges into tees instead of breaking it.
func TestWidgetDrawHlineJunction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	src := `
gtmux.widget{ draw = function(c)
  c:box(0, 0, c.w, c.h, "fg=cyan")
  c:hline(2, "fg=grey")
end }
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, binds := LoadClient(path)
	defer binds.Close()
	cv, _, _ := binds.RunDraw(lastWidget(cfg).Draw, 6, 5, emu.White, emu.Black, 0)
	if g, _ := cv.At(0, 2); g.Char != '├' {
		t.Errorf("left border at hline = %q, want ├", g.Char)
	}
	if g, _ := cv.At(5, 2); g.Char != '┤' {
		t.Errorf("right border at hline = %q, want ┤", g.Char)
	}
	if g, _ := cv.At(3, 2); g.Char != '─' {
		t.Errorf("interior hline = %q, want ─", g.Char)
	}
	// The junction cell keeps the BORDER's color (cyan from box), not the
	// divider's — a one-color cell should read as part of the frame.
	if g, _ := cv.At(0, 2); g.FG != emu.Cyan {
		t.Errorf("junction ├ fg = %v, want cyan (border color kept)", g.FG)
	}
}

// A "rounded" token in the box style swaps the corners for arcs.
func TestWidgetDrawRoundedBox(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	src := `gtmux.widget{ draw = function(c) c:box(0,0,c.w,c.h,"fg=cyan,rounded") end }`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, binds := LoadClient(path)
	defer binds.Close()
	cv, _, _ := binds.RunDraw(lastWidget(cfg).Draw, 4, 4, emu.White, emu.Black, 0)
	for _, tc := range []struct {
		x, y int
		want rune
	}{{0, 0, '╭'}, {3, 0, '╮'}, {0, 3, '╰'}, {3, 3, '╯'}} {
		if g, _ := cv.At(tc.x, tc.y); g.Char != tc.want {
			t.Errorf("corner (%d,%d) = %q, want %q", tc.x, tc.y, g.Char, tc.want)
		}
	}
}

// c:box with a {title, title_at} table embeds the title on the frame line.
func TestWidgetDrawBoxTitle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	src := `gtmux.widget{ draw = function(c)
	  c:box(0, 0, c.w, c.h, { style = "fg=cyan", title = "hi", title_at = "top-centre" })
	end }`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, binds := LoadClient(path)
	defer binds.Close()
	cv, _, _ := binds.RunDraw(lastWidget(cfg).Draw, 10, 3, emu.White, emu.Black, 0)
	// top border row 0, " hi " centred in the 8-wide interior (cols 1..8): the
	// 4-char label starts at col 1 + (8-4)/2 = 3 → cells 3.." ",4 'h',5 'i',6 ' '.
	row := make([]rune, cv.W)
	for x := 0; x < cv.W; x++ {
		g, _ := cv.At(x, 0)
		row[x] = g.Char
	}
	if got := string(row); got != "┌── hi ──┐" {
		t.Fatalf("titled top border = %q, want %q", got, "┌── hi ──┐")
	}
}

// A draw fn may emit ops (gtmux.run_command) and read server-global @options
// from the snapshot (gtmux.global_option): the cross-client widget state path.
func TestDrawEmitsOpsAndReadsGlobalOption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	os.WriteFile(path, []byte(`gtmux.widget{ dock = "left", size = 5, draw = function(c)
  if gtmux.global_option("@seen_1") == "" then gtmux.run_command("set -g @seen_1 1") end
  c:text(0, 0, gtmux.global_option("@x"))
end }`), 0o644)
	cfg, binds := LoadClient(path)
	defer binds.Close()
	binds.Hooks.Snapshot = func() *proto.StateSnapshot {
		return &proto.StateSnapshot{Options: map[string]string{"@x": "hi"}}
	}
	cv, _, ops := binds.RunDraw(lastWidget(cfg).Draw, 5, 1, emu.White, emu.Black, 0)
	if len(ops) != 1 || ops[0].Command != "set -g @seen_1 1" {
		t.Fatalf("ops = %+v, want one set -g command", ops)
	}
	if g, _ := cv.At(0, 0); g.Char != 'h' {
		t.Errorf("cell = %q, want h (global_option read)", g.Char)
	}
	binds.Hooks.Snapshot = func() *proto.StateSnapshot {
		return &proto.StateSnapshot{Options: map[string]string{"@seen_1": "1"}}
	}
	if _, _, ops := binds.RunDraw(lastWidget(cfg).Draw, 5, 1, emu.White, emu.Black, 0); len(ops) != 0 {
		t.Errorf("ops after seen = %+v, want none", ops)
	}
}

// c:text advances one cell per rune: a multibyte glyph must not leave a gap
// (a range over the string gave byte offsets, so "⠋x" put x two cells late).
func TestWidgetDrawTextRuneCells(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	src := `
gtmux.widget{ dock = "top", size = 1, draw = function(c)
  c:text(0, 0, "⠋x")
end }
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, binds := LoadClient(path)
	defer binds.Close()
	cv, _, _ := binds.RunDraw(lastWidget(cfg).Draw, 4, 1, emu.White, emu.Black, 0)
	if g, _ := cv.At(1, 0); g.Char != 'x' {
		t.Errorf("cell 1 = %q, want x right after the spinner", g.Char)
	}
}

// require("gtmux.sidebar") is a bundled module: calling it registers the
// sidebar dock widget with the given options; a config that never requires
// it registers no widget for it.
func TestBundledSidebarModule(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	if err := os.WriteFile(path, []byte(`require("gtmux.sidebar"){ size = 30, name = "sb" }`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, binds := LoadClient(path)
	defer binds.Close()
	w := lastWidget(cfg)
	if w.Dock != "left" || w.Size != 30 || w.Name != "sb" || w.Draw == nil || w.OnClick == nil {
		t.Fatalf("sidebar widget = %+v, want left dock, size 30, name sb, draw+on_click", w)
	}
	cv, _, _ := binds.RunDraw(w.Draw, 30, 5, emu.White, emu.Black, 0)
	if g, _ := cv.At(2, 1); g.Char != 'S' { // "SESSIONS" header inside the box
		t.Errorf("cell (2,1) = %q, want S of SESSIONS", g.Char)
	}
	if err := os.WriteFile(path, []byte(`gtmux.options.status_bg = "red"`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg2, binds2 := LoadClient(path)
	defer binds2.Close()
	for _, w := range cfg2.Widgets {
		if w.Name == "sidebar" || w.Name == "sb" {
			t.Errorf("sidebar registered without require: %+v", w)
		}
	}
}
