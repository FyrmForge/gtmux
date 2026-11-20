package bgcache

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestGetNeverBlocksAndFetchesOnce(t *testing.T) {
	c := New(time.Hour)
	var calls atomic.Int32
	fetch := func() string { calls.Add(1); time.Sleep(20 * time.Millisecond); return "v" }

	if got := c.Get("k", fetch); got != "" {
		t.Fatalf("first Get = %q, want empty (no blocking)", got)
	}
	c.Get("k", fetch) // in flight: must not start a second fetch
	time.Sleep(50 * time.Millisecond)
	if got := c.Get("k", fetch); got != "v" {
		t.Fatalf("after fetch landed Get = %q, want v", got)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("fetch calls = %d, want 1", n)
	}
}
