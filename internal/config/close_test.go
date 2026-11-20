package config

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Detach/kill closes the Lua VM while the input goroutine may still repaint
// the status bar (a prefix mode change). Every entry point must see the VM is
// closed instead of running on a closed LState (a gopher-lua nil-pointer crash).
func TestCloseRacesPaint(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.lua")
	src := `gtmux.widget{ dock = "left", size = 20, component = function(props, ui)
  for i = 1, 5 do ui:child(i, 0, 1, 1, function(p, cell) cell:text(0, 0, "x") end, {}) end
end }`
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 50; round++ {
		cfg, b := LoadClient(p)
		comp := cfg.Widgets[len(cfg.Widgets)-1].Component
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				b.RunComponent(comp, nil, 20, 2, 0, 0, 0)
			}
		}()
		b.Close()
		wg.Wait()
		b.RunComponent(comp, nil, 20, 2, 0, 0, 0) // after close: a no-op, not a crash
		b.Close()                                 // twice: fine
	}
}
