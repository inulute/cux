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
// should receive no FrameOut at all. The attach nudge (redraw) also
// broadcasts a FrameSize to announce the current negotiated size to every
// client, including this brand-new one — that's expected and orthogonal to
// history replay, so it's tolerated here rather than treated as a leak.
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
	for {
		typ, _, err := readFrame(conn)
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				return // no history replay arrived within the window — expected
			}
			t.Fatalf("readFrame: %v", err)
		}
		if typ == FrameOut {
			t.Fatalf("got a FrameOut frame on the alt screen; history should not replay")
		}
	}
}
