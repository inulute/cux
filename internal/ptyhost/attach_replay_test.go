//go:build !windows

package ptyhost

import (
	"bytes"
	"net"
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
