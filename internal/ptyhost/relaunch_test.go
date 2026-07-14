package ptyhost

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestRelaunchReusesTTY reproduces the wrapper's relaunch path: the same
// Host (and thus the same tty) is wired into a second exec.Cmd after the
// first child has exited. The reported bug was the second Start failing
// with "bad file descriptor".
func TestRelaunchReusesTTY(t *testing.T) {
	h, err := New(filepath.Join(t.TempDir(), "s.sock"), false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer h.Close()

	for i := 0; i < 3; i++ {
		cmd := exec.Command("true")
		// Mirror the wrapper's launch(): a fresh dup per exec, so os/exec's
		// Wait() closing its stdio doesn't take down the shared PTY slave.
		tty, err := h.TTYDup()
		if err != nil {
			t.Fatalf("launch %d: dup: %v", i, err)
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
		cmd.SysProcAttr = SysProcAttr()
		if err := cmd.Start(); err != nil {
			t.Fatalf("launch %d: start: %v", i, err)
		}
		_ = cmd.Wait()
	}
	// The real slave must still be usable after all those launches.
	if _, err := h.TTYDup(); err != nil {
		t.Fatalf("PTY slave was closed by a child's exec: %v", err)
	}
}
