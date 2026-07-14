//go:build !windows

package ptyhost

import (
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestResizeSignalsChild verifies that a size change delivers SIGWINCH to
// the registered child. claude runs without a controlling terminal, so a
// TIOCSWINSZ on the master raises no SIGWINCH by itself — the host must
// signal the child explicitly or the child never reflows to the new size.
func TestResizeSignalsChild(t *testing.T) {
	h, err := New(filepath.Join(t.TempDir(), "s.sock"), true)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer h.Close()

	sig := make(chan os.Signal, 8)
	signal.Notify(sig, syscall.SIGWINCH)
	defer signal.Stop(sig)
	// Drain any SIGWINCH already queued from construction.
	for draining := true; draining; {
		select {
		case <-sig:
		default:
			draining = false
		}
	}

	// Register this test process as the child, then change the size.
	h.SetChildPID(os.Getpid())
	h.applySize()

	select {
	case <-sig:
		// delivered — good
	case <-time.After(2 * time.Second):
		t.Fatal("resize did not deliver SIGWINCH to the child")
	}

	// After clearing the child, a resize must not signal anyone.
	h.SetChildPID(0)
	for draining := true; draining; {
		select {
		case <-sig:
		default:
			draining = false
		}
	}
	h.applySize()
	select {
	case <-sig:
		t.Fatal("resize signaled after the child was cleared")
	case <-time.After(300 * time.Millisecond):
		// no signal — good
	}
}
