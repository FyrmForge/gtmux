package server

import (
	"sync"
	"testing"
	"time"

	"github.com/FyrmForge/gtmux/internal/emu"
	"github.com/FyrmForge/gtmux/internal/proto"
)

func testAttachment() *attachment {
	a := &attachment{}
	a.outCond = sync.NewCond(&a.outMu)
	return a
}

func paneDiff(id int, cursorCol int, lines map[int]emu.Line) *proto.ServerMsg {
	cursor := emu.Cursor{}
	cursor.C = cursorCol
	return &proto.ServerMsg{PaneContent: []proto.PaneContent{{
		PaneID: id, Lines: lines, Cursor: cursor, CursorVisible: true,
	}}}
}

func queuedOutput(a *attachment) []outbound {
	a.outMu.Lock()
	defer a.outMu.Unlock()
	return append([]outbound(nil), a.out...)
}

func TestAttachmentCoalescesPaneRowsWithLatestCursor(t *testing.T) {
	a := testAttachment()
	first := paneDiff(7, 1, map[int]emu.Line{3: {{Char: 'a'}}})
	a.queuePaneDiff(first)
	a.queuePaneDiff(paneDiff(7, 2, nil)) // cursor-only update must not orphan row 3

	queued := queuedOutput(a)
	if len(queued) != 1 {
		t.Fatalf("queued batches = %d, want 1", len(queued))
	}
	got := queued[0].msg.PaneContent[0]
	if got.Cursor.C != 2 {
		t.Fatalf("cursor column = %d, want 2", got.Cursor.C)
	}
	if line := got.Lines[3]; len(line) != 1 || line[0].Char != 'a' {
		t.Fatalf("coalesced row = %#v, want a", line)
	}
	// Coalescing is attachment-local: the broadcast source message may be queued
	// concurrently by another attachment and must remain immutable.
	if first.PaneContent[0].Cursor.C != 1 {
		t.Fatalf("source cursor mutated to %d", first.PaneContent[0].Cursor.C)
	}
}

func TestAttachmentCoalescingKeepsLatestVersionOfEachRow(t *testing.T) {
	a := testAttachment()
	a.queuePaneDiff(paneDiff(1, 1, map[int]emu.Line{
		0: {{Char: 'a'}},
		1: {{Char: 'x'}},
	}))
	a.queuePaneDiff(paneDiff(1, 2, map[int]emu.Line{
		0: {{Char: 'b'}},
	}))
	a.queuePaneDiff(paneDiff(2, 4, map[int]emu.Line{
		5: {{Char: 'z'}},
	}))

	queued := queuedOutput(a)
	if len(queued) != 1 {
		t.Fatalf("queued batches = %d, want 1", len(queued))
	}
	panes := queued[0].msg.PaneContent
	if len(panes) != 2 {
		t.Fatalf("coalesced panes = %d, want 2", len(panes))
	}
	if panes[0].Lines[0][0].Char != 'b' || panes[0].Lines[1][0].Char != 'x' {
		t.Fatalf("pane 1 rows were not merged latest-wins: %#v", panes[0].Lines)
	}
	if panes[0].Cursor.C != 2 || panes[1].Cursor.C != 4 {
		t.Fatalf("latest cursors = (%d,%d), want (2,4)", panes[0].Cursor.C, panes[1].Cursor.C)
	}
}

func TestAttachmentFloodLeavesOneCurrentDiffBatch(t *testing.T) {
	a := testAttachment()
	a.queuePaneDiff(paneDiff(1, 0, map[int]emu.Line{0: {{Char: 'k'}}}))
	for col := 1; col <= 1000; col++ {
		a.queuePaneDiff(paneDiff(1, col, nil))
	}

	queued := queuedOutput(a)
	if len(queued) != 1 {
		t.Fatalf("queued batches after flood = %d, want 1", len(queued))
	}
	got := queued[0].msg.PaneContent[0]
	if got.Cursor.C != 1000 || got.Lines[0][0].Char != 'k' {
		t.Fatalf("coalesced flood lost state: cursor=%d lines=%#v", got.Cursor.C, got.Lines)
	}
}

func TestAttachmentFramePacingCoalescesBeforePop(t *testing.T) {
	a := testAttachment()
	a.queuePaneDiff(paneDiff(1, 1, map[int]emu.Line{0: {{Char: 'a'}}}))

	type result struct {
		item outbound
		ok   bool
	}
	resultCh := make(chan result, 1)
	ready := time.Now().Add(40 * time.Millisecond)
	go func() {
		item, ok := a.nextOutbound(ready)
		resultCh <- result{item: item, ok: ok}
	}()

	// This cursor-only update arrives while the writer is waiting for the next
	// frame. It must merge with the row update still in the outbox.
	time.Sleep(5 * time.Millisecond)
	a.queuePaneDiff(paneDiff(1, 2, nil))

	got := <-resultCh
	if !got.ok {
		t.Fatal("nextOutbound closed while a diff was queued")
	}
	if early := time.Until(ready); early > time.Millisecond {
		t.Fatalf("diff popped %v before its frame boundary", early)
	}
	content := got.item.msg.PaneContent[0]
	if content.Cursor.C != 2 || content.Lines[0][0].Char != 'a' {
		t.Fatalf("paced diff did not coalesce: cursor=%d lines=%#v", content.Cursor.C, content.Lines)
	}
}

func TestAttachmentOrderedMessageSeparatesDiffBatches(t *testing.T) {
	a := testAttachment()
	a.queuePaneDiff(paneDiff(1, 1, nil))
	a.queue(&proto.ServerMsg{Status: &proto.StatusInfo{PromptText: "boundary"}})
	a.queuePaneDiff(paneDiff(1, 2, nil))

	queued := queuedOutput(a)
	if len(queued) != 3 {
		t.Fatalf("queued items = %d, want 3", len(queued))
	}
	if queued[0].kind != outboundPaneDiff || queued[1].kind != outboundOrdered || queued[2].kind != outboundPaneDiff {
		t.Fatalf("queued kinds = %#v, want diff/ordered/diff", queued)
	}
}

func TestAttachmentCoalescesPopupDiffs(t *testing.T) {
	a := testAttachment()
	popupDiff := func(cursorCol int, lines map[int]emu.Line) *proto.ServerMsg {
		content := paneDiff(9, cursorCol, lines).PaneContent[0]
		return &proto.ServerMsg{Popup: &proto.PopupMsg{Content: &content}}
	}
	a.queuePopupDiff(popupDiff(1, map[int]emu.Line{0: {{Char: 'p'}}}))
	a.queuePopupDiff(popupDiff(2, nil))

	queued := queuedOutput(a)
	if len(queued) != 1 {
		t.Fatalf("queued popup batches = %d, want 1", len(queued))
	}
	got := queued[0].msg.Popup.Content
	if got.Cursor.C != 2 || got.Lines[0][0].Char != 'p' {
		t.Fatalf("coalesced popup lost state: cursor=%d lines=%#v", got.Cursor.C, got.Lines)
	}
}
