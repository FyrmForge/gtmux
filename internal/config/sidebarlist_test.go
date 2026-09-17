package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	lua "github.com/yuin/gopher-lua"

	"github.com/FyrmForge/gtmux/internal/emu"
	"github.com/FyrmForge/gtmux/internal/proto"
)

// canvasText reads a rendered canvas back as lines of text, right-trimmed.
func canvasText(cv *Canvas) []string {
	out := make([]string, cv.H)
	for r := 0; r < cv.H; r++ {
		var b strings.Builder
		for c := 0; c < cv.W; c++ {
			g, _ := cv.At(c, r)
			if g.Char == 0 {
				b.WriteByte(' ')
				continue
			}
			b.WriteRune(g.Char)
		}
		out[r] = strings.TrimRight(b.String(), " ")
	}
	return out
}

// Unfocused, the caret IS the you-are-here marker: it follows whatever session
// the client is attached to, including a switch made outside the dock (a click,
// another keybind), not the last row the keyboard left it on.
func TestSidebarCaretTracksCurrentSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	if err := os.WriteFile(path, []byte(`require("gtmux.sidebar"){ size = 24, title = false, spinner = false }`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, binds := LoadClient(path)
	defer binds.Close()
	w := lastWidget(cfg)

	attached := "three"
	binds.Hooks = WidgetHooks{
		Snapshot: func() *proto.StateSnapshot {
			return &proto.StateSnapshot{Sessions: []proto.SnapSession{
				{Name: "one", Windows: []proto.SnapWindow{{Index: 1, Active: true}}},
				{Name: "two", Windows: []proto.SnapWindow{{Index: 1, Active: true}}},
				{Name: "three", Windows: []proto.SnapWindow{{Index: 1, Active: true}}},
			}}
		},
		Context: func() map[string]string { return map[string]string{"session": attached, "pane": "%1"} },
	}

	caretRow := func(state *lua.LTable) (int, *lua.LTable) {
		cv, _, st, _ := binds.RunComponent(w.Component, state, 24, 14, emu.White, emu.Black, 0)
		for r, l := range canvasText(cv) {
			if strings.Contains(l, ">") {
				return r, st
			}
		}
		return -1, st
	}

	// First ever draw, keyboard never used: the caret is already on "three".
	row, state := caretRow(nil)
	cv, _, _, _ := binds.RunComponent(w.Component, state, 24, 14, emu.White, emu.Black, 0)
	if row < 0 || !strings.Contains(canvasText(cv)[row], "three") {
		t.Fatalf("caret row %d is not the attached session:\n%s", row, strings.Join(canvasText(cv), "\n"))
	}

	// Switched elsewhere (mouse click on a row, another bind): the caret follows.
	attached = "one"
	row, state = caretRow(state)
	cv, _, _, _ = binds.RunComponent(w.Component, state, 14, 14, emu.White, emu.Black, 0)
	if row < 0 || !strings.Contains(canvasText(cv)[row], "one") {
		t.Fatalf("caret stayed put after an outside switch; rows:\n%s", strings.Join(canvasText(cv), "\n"))
	}
}

// The two rules bracketing the attached session are lit; every other rule stays
// dim, so the active block reads as one band.
func TestSidebarActiveRulesLit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	if err := os.WriteFile(path, []byte(`require("gtmux.sidebar"){ size = 24, title = false, spinner = false }`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, binds := LoadClient(path)
	defer binds.Close()
	binds.Hooks = WidgetHooks{
		Snapshot: func() *proto.StateSnapshot {
			return &proto.StateSnapshot{Sessions: []proto.SnapSession{
				{Name: "one", Windows: []proto.SnapWindow{{Index: 1, Active: true}}},
				{Name: "two", Windows: []proto.SnapWindow{{Index: 1, Active: true}}},
				{Name: "three", Windows: []proto.SnapWindow{{Index: 1, Active: true}}},
				{Name: "four", Windows: []proto.SnapWindow{{Index: 1, Active: true}}},
			}}
		},
		// Attached to "two": the rules above and below it are the lit pair.
		Context: func() map[string]string { return map[string]string{"session": "two", "pane": "%1"} },
	}
	cv, _, _, _ := binds.RunComponent(lastWidget(cfg).Component, nil, 24, 14, emu.White, emu.Black, 0)

	// The attached session's own name is the reference "lit" colour.
	var green emu.Color
	for r, l := range canvasText(cv) {
		if strings.Contains(l, " two") {
			g, _ := cv.At(4, r) // first letter of the session name
			green = g.FG
		}
	}
	var lit, dim int
	for r, l := range canvasText(cv) {
		if !strings.HasPrefix(l, "│ ──") { // the inset rules, not the box's own borders
			continue
		}
		g, _ := cv.At(2, r) // a dash cell inside the inset rule
		if g.FG == green {
			lit++
		} else {
			dim++
		}
	}
	if lit != 2 {
		t.Fatalf("lit rules = %d, want the 2 bracketing the attached session", lit)
	}
	if dim != 1 {
		t.Fatalf("dim rules = %d, want 1 (between three and four)", dim)
	}
}

// The sidebar is ONE list: every session, its agent panes nested beneath, a
// blank line between groups. The cursor ">" only appears once the dock is
// focused, and it lands on items — never on a spacer.
func TestSidebarUnifiedList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	if err := os.WriteFile(path, []byte(`require("gtmux.sidebar"){ size = 24, title = false, spinner = false }`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, binds := LoadClient(path)
	defer binds.Close()
	w := lastWidget(cfg)

	binds.Hooks = WidgetHooks{
		Snapshot: func() *proto.StateSnapshot {
			return &proto.StateSnapshot{Sessions: []proto.SnapSession{
				{Name: "one", Windows: []proto.SnapWindow{{Index: 1, Active: true, Panes: []proto.PaneInfo{
					{Number: 1, ID: 11, Command: "claude", Title: "t"},
				}}}},
				{Name: "two", Windows: []proto.SnapWindow{{Index: 1, Active: true, Panes: []proto.PaneInfo{
					{Number: 1, ID: 22, Command: "codex", Title: "t"},
				}}}},
				{Name: "three", Windows: []proto.SnapWindow{{Index: 1, Active: true, Panes: []proto.PaneInfo{
					{Number: 1, ID: 33, Command: "zsh", Title: "t"},
				}}}},
			}}
		},
		Context:    func() map[string]string { return map[string]string{"session": "one", "pane": "%99"} },
		AgentState: func(int) string { return "idle" },
	}

	// The cursor is drawn even unfocused (dimmed), so its position survives a blur.
	cv, _, state, _ := binds.RunComponent(w.Component, nil, 24, 14, emu.White, emu.Black, 0)
	got := canvasText(cv)
	if !strings.Contains(strings.Join(got, "\n"), ">") {
		t.Fatalf("unfocused dock drew no cursor:\n%s", strings.Join(got, "\n"))
	}
	// Sessions each carry their agent beneath them, one blank line per group.
	want := []string{"one", "! Claude", "two", "! Codex", "three"}
	joined := strings.Join(got, "\n")
	at := -1
	for _, frag := range want {
		i := strings.Index(joined, frag)
		if i <= at {
			t.Fatalf("expected %q after the previous entry, in order; got:\n%s", frag, joined)
		}
		at = i
	}

	// Focused: the cursor shows, and j/k walks items without stopping on a
	// spacer row (every stop is a line with content).
	state = binds.MarkFocused(state, true)
	for i := 0; i < 5; i++ {
		cv, _, state, _ = binds.RunComponent(w.Component, state, 24, 14, emu.White, emu.Black, 0)
		row := -1
		for r, l := range canvasText(cv) {
			if strings.Contains(l, ">") {
				row = r
			}
		}
		if row < 0 {
			t.Fatalf("focused dock drew no cursor on step %d:\n%s", i, strings.Join(canvasText(cv), "\n"))
		}
		if strings.TrimSpace(strings.ReplaceAll(canvasText(cv)[row], ">", "")) == "" {
			t.Fatalf("cursor parked on a blank spacer row on step %d", i)
		}
		binds.RunKey(w.OnKey, "j", state)
	}
}

// The cursor remembers the session NAME, not its index. Kill a session ABOVE
// the cursor and the list shifts up — an index-based cursor would then switch
// you to a session you never previewed.
func TestSidebarCursorSurvivesListShift(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	if err := os.WriteFile(path, []byte(`require("gtmux.sidebar"){ size = 24, title = false, spinner = false }`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, binds := LoadClient(path)
	defer binds.Close()
	w := lastWidget(cfg)

	names := []string{"one", "two", "three", "four"}
	binds.Hooks = WidgetHooks{
		Snapshot: func() *proto.StateSnapshot {
			snap := &proto.StateSnapshot{}
			for _, n := range names {
				snap.Sessions = append(snap.Sessions, proto.SnapSession{
					Name: n, Windows: []proto.SnapWindow{{Index: 1, Active: true}}})
			}
			return snap
		},
		Context: func() map[string]string { return map[string]string{"session": "one", "pane": "%1"} },
	}

	_, _, state, _ := binds.RunComponent(w.Component, nil, 24, 20, emu.White, emu.Black, 0)
	state = binds.MarkFocused(state, true)
	_, _, state, _ = binds.RunComponent(w.Component, state, 24, 20, emu.White, emu.Black, 0)
	// Walk down to "two".
	for i := 0; i < 1; i++ {
		binds.RunKey(w.OnKey, "j", state)
		_, _, state, _ = binds.RunComponent(w.Component, state, 24, 20, emu.White, emu.Black, 0)
	}

	// A session above the cursor goes away; the list shifts up under it.
	names = []string{"two", "three", "four"}
	_, _, state, _ = binds.RunComponent(w.Component, state, 24, 20, emu.White, emu.Black, 0)

	ops, _ := binds.RunKey(w.OnKey, "Enter", state)
	var target string
	for _, op := range ops {
		if len(op.Action) == 3 && op.Action[0] == "switch-client" {
			target = op.Action[2]
		}
	}
	if target != "two" {
		t.Fatalf("Enter switched to %q, want the session the cursor was on (two)", target)
	}
}

// The list can be taller than the dock: walking down scrolls, instead of
// leaving the cursor on a row that isn't painted.
func TestSidebarCursorStaysVisible(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	if err := os.WriteFile(path, []byte(`require("gtmux.sidebar"){ size = 24, title = false, spinner = false }`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, binds := LoadClient(path)
	defer binds.Close()
	w := lastWidget(cfg)

	binds.Hooks = WidgetHooks{
		Snapshot: func() *proto.StateSnapshot {
			snap := &proto.StateSnapshot{}
			for _, n := range []string{"s1", "s2", "s3", "s4", "s5", "s6", "s7", "s8"} {
				snap.Sessions = append(snap.Sessions, proto.SnapSession{
					Name: n, Windows: []proto.SnapWindow{{Index: 1, Active: true}}})
			}
			return snap
		},
		Context: func() map[string]string { return map[string]string{"session": "s1", "pane": "%1"} },
	}

	_, _, state, _ := binds.RunComponent(w.Component, nil, 24, 6, emu.White, emu.Black, 0)
	state = binds.MarkFocused(state, true)
	for i := 0; i < 6; i++ {
		binds.RunKey(w.OnKey, "j", state)
		var cv *Canvas
		cv, _, state, _ = binds.RunComponent(w.Component, state, 24, 6, emu.White, emu.Black, 0)
		if !strings.Contains(strings.Join(canvasText(cv), "\n"), ">") {
			t.Fatalf("cursor scrolled out of view after %d moves:\n%s", i+1, strings.Join(canvasText(cv), "\n"))
		}
	}
}

// Focused, the frame goes green: the ">" caret is one cell and easy to lose on
// a wide screen, so the border says which surface is eating your keys.
func TestSidebarBorderGreenWhenFocused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lua")
	if err := os.WriteFile(path, []byte(`require("gtmux.sidebar"){ size = 24 }`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, binds := LoadClient(path)
	defer binds.Close()
	w := lastWidget(cfg)
	binds.Hooks = WidgetHooks{
		Snapshot: func() *proto.StateSnapshot {
			return &proto.StateSnapshot{Sessions: []proto.SnapSession{
				{Name: "one", Windows: []proto.SnapWindow{{Index: 1, Active: true}}},
			}}
		},
		Context: func() map[string]string { return map[string]string{"session": "one", "pane": "%1"} },
	}

	corner := func(state *lua.LTable) emu.Color {
		cv, _, _, _ := binds.RunComponent(w.Component, state, 24, 8, emu.White, emu.Black, 0)
		g, _ := cv.At(0, 0)
		return g.FG
	}

	_, _, st, _ := binds.RunComponent(w.Component, nil, 24, 8, emu.White, emu.Black, 0)
	if got := corner(st); got != emu.Cyan {
		t.Fatalf("unfocused border %v, want cyan", got)
	}
	if got := corner(binds.MarkFocused(st, true)); got != emu.Green {
		t.Fatalf("focused border %v, want green", got)
	}
}
