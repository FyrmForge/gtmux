package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/FyrmForge/gtmux/internal/emu"
)

func TestLoadClientMissingFileUsesDefaults(t *testing.T) {
	cfg, binds := LoadClient("/nonexistent/gtmux/client.lua")
	defer binds.Close()
	if cfg != DefaultClientConfig() {
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
	if got != DefaultClientConfig() {
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
		"s": {Action: []string{"choose-session"}},
		"[": {Action: []string{"copy-mode"}},
		"]": {Action: []string{"paste"}},
		"$": {Local: "rename-session"},
		",": {Local: "rename-window"},
		":": {Action: []string{"command-prompt"}},
		"w": {Action: []string{"choose-tree"}},
		"W": {Local: "choose-window"},
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

// TestDisplayMenuEncoding pins the display_menu primitive's Action encoding.
func TestDisplayMenuEncoding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	if err := os.WriteFile(path, []byte(`
gtmux.bind("M", function() gtmux.display_menu("go", "new", "new-window", "kill", "kill-pane") end)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, binds := LoadClient(path)
	defer binds.Close()
	want := []string{"display-menu", "-T", "go", "--", "new", "new-window", "kill", "kill-pane"}
	if ops := binds.Resolve("M"); len(ops) != 1 || !reflect.DeepEqual(ops[0].Action, want) {
		t.Errorf("display_menu encoded as %v, want %v", ops, want)
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
