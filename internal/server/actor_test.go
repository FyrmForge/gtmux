package server

import (
	"testing"
	"time"

	"github.com/FyrmForge/gtmux/internal/emu"
	"github.com/FyrmForge/gtmux/internal/proto"
)

// Actor contract behind the server-crash fix: stopMsg must end run() (so a
// stopped actor's channel is never closed), and a straggler outputMsg sent
// afterward must not panic. In prod a pane reader goroutine sends outputMsg
// straight to origin.events; when the window tore down, close(wa.events) let a
// late read panic on a closed channel — fatal to the whole server. If run()
// stops leaving the channel open (as here), the late send just buffers. If
// someone removes the stopMsg case, `<-wa.done` below hangs the test instead.
func TestStoppedActorTakesStragglerOutputWithoutPanic(t *testing.T) {
	w := &window{}
	wa := newWindowActor(w)
	go wa.run()

	// Stop it exactly the way finishStop does.
	wa.stopped = true
	wa.ctl <- stopMsg{}
	<-wa.done // run() has exited

	// A pane reader goroutine that read one more chunk before its pty closed
	// posts here after the stop. Must not panic.
	wa.events <- outputMsg{pane: &pane{}, gen: 0, data: []byte("straggler")}
}

func TestViewRenderMailboxKeepsDirtyRowWithLatestCursor(t *testing.T) {
	renders := make(chan *view)
	vw := newView(renders, make(chan any))
	defer vw.stopRenders()
	p := &pane{}

	firstCursor := emu.Cursor{}
	firstCursor.C = 1
	vw.queueRender(renderMsg{pane: p, content: &proto.PaneContent{
		PaneID: p.id, Lines: map[int]emu.Line{4: {{Char: 'x'}}},
		Cursor: firstCursor, CursorVisible: true,
	}})
	for col := 2; col <= 1000; col++ {
		cursor := emu.Cursor{}
		cursor.C = col
		vw.queueRender(renderMsg{pane: p, content: &proto.PaneContent{
			PaneID: p.id, Cursor: cursor, CursorVisible: true,
		}})
	}

	gotView := <-renders
	got := gotView.takeRenders()
	if len(got) != 1 {
		t.Fatalf("pending renders = %d, want one coalesced pane render", len(got))
	}
	if got[0].content.Cursor.C != 1000 {
		t.Fatalf("cursor column = %d, want 1000", got[0].content.Cursor.C)
	}
	if line := got[0].content.Lines[4]; len(line) != 1 || line[0].Char != 'x' {
		t.Fatalf("dirty row was lost while cursor updates coalesced: %#v", line)
	}

	select {
	case <-renders:
		t.Fatal("mailbox queued more than one view token for one pending batch")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestViewRenderMailboxBoundsAlternatingPaneFlood(t *testing.T) {
	renders := make(chan *view)
	vw := newView(renders, make(chan any))
	defer vw.stopRenders()
	panes := []*pane{{id: 1}, {id: 2}}

	for col := 0; col < 1000; col++ {
		p := panes[col%len(panes)]
		cursor := emu.Cursor{}
		cursor.C = col
		vw.queueRender(renderMsg{pane: p, content: &proto.PaneContent{
			PaneID: p.id, Cursor: cursor, CursorVisible: true,
		}})
	}

	got := (<-renders).takeRenders()
	if len(got) != len(panes) {
		t.Fatalf("pending renders = %d, want one per pane (%d)", len(got), len(panes))
	}
}
