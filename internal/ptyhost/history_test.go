package ptyhost

import (
	"bytes"
	"testing"
)

func TestHistoryRingTrims(t *testing.T) {
	var h history
	h.record(bytes.Repeat([]byte("a"), 10))
	h.record(bytes.Repeat([]byte("b"), maxHistory))
	snap := h.snapshot()
	if len(snap) != maxHistory {
		t.Fatalf("snapshot len = %d, want %d", len(snap), maxHistory)
	}
	if bytes.Contains(snap, []byte("a")) {
		t.Fatal("oldest bytes should have been trimmed")
	}
	if snap[len(snap)-1] != 'b' {
		t.Fatal("newest byte was lost")
	}
}

func TestHistoryAltScreenSuppressesReplay(t *testing.T) {
	var h history
	h.record([]byte("shell output before the TUI\n"))
	if h.replay() == nil {
		t.Fatal("normal-screen output should be replayed")
	}
	// A full-screen TUI enters the alt screen: replay must be suppressed.
	h.record([]byte("\x1b[?1049h\x1b[2J\x1b[Hpainting a full-screen frame"))
	if h.replay() != nil {
		t.Fatal("alt-screen output must not be replayed (would render off-screen)")
	}
	// The backlog is still retained — snapshot is not gated on alt screen.
	if len(h.snapshot()) == 0 {
		t.Fatal("history should still be recorded while on the alt screen")
	}
	// Leaving the alt screen restores replay.
	h.record([]byte("\x1b[?1049lback to the shell\n"))
	if h.replay() == nil {
		t.Fatal("replay should resume after leaving the alt screen")
	}
}

func TestHistoryAltScreenToggleSplitAcrossWrites(t *testing.T) {
	var h history
	// The enter sequence \x1b[?1049h is split across two record calls; the
	// carry window must still detect it.
	h.record([]byte("output\x1b[?10"))
	h.record([]byte("49h\x1b[2Jframe"))
	if h.replay() != nil {
		t.Fatal("split alt-screen enter should still suppress replay")
	}
}
