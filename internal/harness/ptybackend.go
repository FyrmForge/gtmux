package harness

import (
	"bytes"
	"os"
	"sync"

	"github.com/creack/pty"

	"github.com/FyrmForge/gtmux/internal/emu"
	"github.com/FyrmForge/gtmux/internal/geom"
)

// ptyBackend is the default backend: the client subprocess runs on a local
// pty and a reader goroutine feeds its output into an emu.Terminal.
type ptyBackend struct {
	ptmx *os.File
	mu   sync.Mutex
	term emu.Terminal
	raw  []byte // cumulative client output — for observing bytes that bypass the grid (passthrough)
}

func (b *ptyBackend) write(p []byte) error {
	_, err := b.ptmx.Write(p)
	return err
}

// rawContains reports whether the cumulative client output holds sub — used to
// observe passthrough bytes, which bypass the grid and self-clobber under redraw.
func (b *ptyBackend) rawContains(sub []byte) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Contains(b.raw, sub)
}

func (b *ptyBackend) snapshot() ([][]emu.Glyph, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return copyGrid(b.term), nil
}

// resize changes the pty's window size — the kernel delivers SIGWINCH to the
// client — and resizes the observing emulator to match.
func (b *ptyBackend) resize(cols, rows int) error {
	if err := pty.Setsize(b.ptmx, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)}); err != nil {
		return err
	}
	b.mu.Lock()
	b.term.Resize(geom.Vec2{R: rows, C: cols})
	b.mu.Unlock()
	return nil
}

// readLoop pumps the client's rendered bytes into the emulator until the pty
// closes (client exit).
func (b *ptyBackend) readLoop() {
	buf := make([]byte, 4096)
	for {
		n, err := b.ptmx.Read(buf)
		if n > 0 {
			b.mu.Lock()
			b.term.Write(buf[:n])
			b.raw = append(b.raw, buf[:n]...)
			b.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}
