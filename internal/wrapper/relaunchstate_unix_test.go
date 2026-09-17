//go:build !windows

package wrapper

import (
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

type nudgeChild struct{ exited atomic.Bool }

func (c *nudgeChild) Pid() int               { return 0 }
func (c *nudgeChild) Signal(os.Signal) error { return nil }
func (c *nudgeChild) Kill() error            { return nil }
func (c *nudgeChild) Exited() bool           { return c.exited.Load() }
func (c *nudgeChild) Wait() error            { return nil }

func openNudgePTY(t *testing.T) (fd int, cleanup func()) {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	if err := pty.Setsize(slave, &pty.Winsize{Rows: 40, Cols: 130}); err != nil {
		t.Fatal(err)
	}
	return int(slave.Fd()), func() { slave.Close(); master.Close() }
}

func fastNudge(t *testing.T, hold time.Duration) {
	t.Helper()
	oldDelays, oldHold := nudgeDelays, nudgeHold
	t.Cleanup(func() { nudgeDelays, nudgeHold = oldDelays, oldHold })
	nudgeDelays = []time.Duration{10 * time.Millisecond}
	nudgeHold = hold
}

func cols(t *testing.T, fd int) uint16 {
	t.Helper()
	ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil {
		t.Fatal(err)
	}
	return ws.Col
}

// A relaunched claude only repaints when its size really changes (Bun emits
// "resize" on change only), so the nudge must narrow and then restore.
func TestNudgeNarrowsByOneColumnAndRestores(t *testing.T) {
	fd, done := openNudgePTY(t)
	defer done()
	fastNudge(t, 200*time.Millisecond)

	finished := make(chan struct{})
	go func() { nudgeFd(fd, &nudgeChild{}); close(finished) }()

	sawNarrow := false
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-finished:
			if !sawNarrow {
				t.Fatal("the terminal was never narrowed, so claude would not repaint")
			}
			if got := cols(t, fd); got != 130 {
				t.Fatalf("size not restored: %d columns, want 130", got)
			}
			return
		case <-deadline:
			t.Fatal("nudge did not finish")
		default:
			if cols(t, fd) == 129 {
				sawNarrow = true
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// If the user resizes while the nudge holds the narrowed size, their size wins.
func TestNudgeLeavesARealResizeAlone(t *testing.T) {
	fd, done := openNudgePTY(t)
	defer done()
	fastNudge(t, 300*time.Millisecond)

	finished := make(chan struct{})
	go func() { nudgeFd(fd, &nudgeChild{}); close(finished) }()
	for cols(t, fd) != 129 {
		time.Sleep(2 * time.Millisecond)
	}
	if err := unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Row: 50, Col: 100}); err != nil {
		t.Fatal(err)
	}
	<-finished
	if got := cols(t, fd); got != 100 {
		t.Fatalf("the nudge overwrote a real resize: %d columns, want 100", got)
	}
}

func TestNudgeDoesNothingOnceClaudeHasExited(t *testing.T) {
	fd, done := openNudgePTY(t)
	defer done()
	fastNudge(t, 50*time.Millisecond)
	ch := &nudgeChild{}
	ch.exited.Store(true)
	nudgeFd(fd, ch)
	if got := cols(t, fd); got != 130 {
		t.Fatalf("size changed although claude had exited: %d", got)
	}
}
