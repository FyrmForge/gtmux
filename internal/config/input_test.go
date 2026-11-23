package config

import (
	"strings"
	"testing"

	"github.com/FyrmForge/gtmux/internal/emu"
	"github.com/FyrmForge/gtmux/internal/proto"
)

// The pickers' filter is a real input: Left/Home/C-w edit at a cursor, and the
// cursor is drawn as a reverse-video cell (it used to be append-only, no cursor).
func TestPickerFilterCursor(t *testing.T) {
	_, b := LoadClient("")
	defer b.Close()
	b.Hooks = WidgetHooks{
		Workspace: func(*proto.WorkspaceRequest) (*proto.WorkspaceReply, error) {
			return &proto.WorkspaceReply{Workspace: "w", Workspaces: []string{"w"}}, nil
		},
		Context: func() map[string]string { return map[string]string{} },
	}
	var modal *ModalOpen
	for _, op := range b.RunWorkspacePicker("w", false, "") {
		if op.Modal != nil {
			modal = op.Modal
		}
	}
	_, _, state, _ := b.RunComponent(modal.Component, nil, modal.Width, modal.Height, 0, 0, 0)
	for _, k := range []string{"a", "b", "c", "Left", "Left", "X", "End", " ", "d", "e", "C-w", "Home", "Delete"} {
		b.RunKey(modal.OnKey, k, state)
	}
	cv, _, _, _ := b.RunComponent(modal.Component, state, modal.Width, modal.Height, 0, 0, 0)
	var row strings.Builder
	cursor := -1
	for x := 0; x < cv.W; x++ {
		g, _ := cv.At(x, 2) // row 0 border, 1 workspace tabs
		row.WriteRune(g.Char)
		if g.Mode&emu.AttrReverse != 0 && cursor < 0 {
			cursor = x
		}
	}
	// abc -> aXbc -> "aXbc de" -> C-w "aXbc " -> Home, Delete -> "Xbc "
	if !strings.Contains(row.String(), "filter: Xbc ") {
		t.Fatalf("filter row = %q", row.String())
	}
	r := row.String()
	if want := len([]rune(r[:strings.Index(r, "filter: ")])) + len("filter: "); cursor != want { // cells, not bytes
		t.Fatalf("cursor at col %d, want %d (start of text after Home)", cursor, want)
	}
}
