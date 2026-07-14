//go:build !windows

package ptyhost

import (
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestBacklogReplayedOnAttach checks a newly attached client receives the
// prior output (scrollback) as its first frame, not just live output.
func TestBacklogReplayedOnAttach(t *testing.T) {
	h, err := New(filepath.Join(t.TempDir(), "s.sock"), true)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer h.Close()
	h.hist.record([]byte("PRIOR-OUTPUT-LINE"))

	conn, err := net.Dial("unix", h.sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	typ, payload, err := readFrame(conn)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if typ != FrameOut || !bytes.Contains(payload, []byte("PRIOR-OUTPUT-LINE")) {
		t.Fatalf("first frame should replay backlog; got typ=%d payload=%q", typ, payload)
	}
}

// TestNoReplayWhileAltScreen checks that when the app is on the alternate
// screen (a full-screen TUI like claude), attaching does NOT dump the raw
// history — that would render off-screen on a differently sized client and
// blank the view. With no PTY child to repaint in the test, the client
// should simply receive nothing.
func TestNoReplayWhileAltScreen(t *testing.T) {
	h, err := New(filepath.Join(t.TempDir(), "s.sock"), true)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer h.Close()
	h.hist.record([]byte("\x1b[?1049h\x1b[2J\x1b[Hfull-screen frame at host size"))

	conn, err := net.Dial("unix", h.sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(400 * time.Millisecond))
	_, _, err = readFrame(conn)
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("expected no replay frame on the alt screen (read should time out); got err=%v", err)
	}
}
