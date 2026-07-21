package server

import "testing"

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
	wa.events <- stopMsg{}
	<-wa.done // run() has exited

	// A pane reader goroutine that read one more chunk before its pty closed
	// posts here after the stop. Must not panic.
	wa.events <- outputMsg{pane: &pane{}, gen: 0, data: []byte("straggler")}
}
