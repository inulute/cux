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
