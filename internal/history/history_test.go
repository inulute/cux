package history

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withTempBackup points the paths package at a temp dir for the duration
// of one test by setting XDG_DATA_HOME (which paths.BackupRoot consults
// on Linux). On macOS/Windows BackupRoot ignores XDG, so the tests run
// against the real ~/.cux — they still work because they clear what
// they create, but if you're running them on those platforms during
// local dev, expect a one-time `swap-history.json` file to appear
// under your home.
func withTempBackup(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_DATA_HOME", dir)
	// Make sure no stale file from a prior run exists at the path
	// paths.BackupRoot will compute under our temp XDG_DATA_HOME.
	_ = os.RemoveAll(filepath.Join(dir, "cux"))
	return dir
}

func TestAppendAndTail(t *testing.T) {
	withTempBackup(t)
	if err := Clear(); err != nil {
		t.Fatal(err)
	}

	entries := []Entry{
		{From: "a@x", To: "b@x", Trigger: TriggerManual, Reason: "user"},
		{From: "b@x", To: "a@x", Trigger: TriggerRateLimit, Reason: "hit 5h cap"},
		{From: "a@x", To: "b@x", Trigger: TriggerThreshold, Reason: "7d 96%"},
	}
	for _, e := range entries {
		if err := Append(e); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	got, err := Tail(0)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("tail returned %d entries, want 3", len(got))
	}
	for i, e := range got {
		if e.Timestamp.IsZero() {
			t.Errorf("entry %d has zero timestamp", i)
		}
		if e.Trigger != entries[i].Trigger {
			t.Errorf("entry %d trigger=%s want %s", i, e.Trigger, entries[i].Trigger)
		}
	}
}

func TestAppendCapsAtMax(t *testing.T) {
	withTempBackup(t)
	if err := Clear(); err != nil {
		t.Fatal(err)
	}

	// Write MaxEntries + 5; expect the oldest 5 to be dropped.
	for i := 0; i < MaxEntries+5; i++ {
		if err := Append(Entry{
			Timestamp: time.Date(2026, 1, 1, 0, 0, i, 0, time.UTC),
			From:      "a@x",
			To:        "b@x",
			Trigger:   TriggerManual,
			Reason:    string(rune('A' + (i % 26))),
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	got, err := Tail(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != MaxEntries {
		t.Fatalf("expected %d entries (capped), got %d", MaxEntries, len(got))
	}
	// After dropping 5 oldest, the first remaining entry's seconds
	// field should be 5 (the 6th entry written, 0-indexed).
	if got[0].Timestamp.Second() != 5 {
		t.Fatalf("expected oldest remaining timestamp.Second()=5, got %d", got[0].Timestamp.Second())
	}
}

func TestTailNLimit(t *testing.T) {
	withTempBackup(t)
	if err := Clear(); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 10; i++ {
		_ = Append(Entry{
			Timestamp: time.Date(2026, 1, 1, 0, 0, i, 0, time.UTC),
			From:      "a@x",
			To:        "b@x",
			Trigger:   TriggerManual,
		})
	}
	got, err := Tail(3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("Tail(3) returned %d entries, want 3", len(got))
	}
	if got[0].Timestamp.Second() != 7 {
		t.Fatalf("Tail(3) should return seconds 7,8,9 (last three); got starts at %d", got[0].Timestamp.Second())
	}
}

func TestAppendRequiresTrigger(t *testing.T) {
	withTempBackup(t)
	_ = Clear()
	if err := Append(Entry{From: "a@x", To: "b@x"}); err == nil {
		t.Fatal("expected error on empty trigger, got nil")
	}
}

func TestClearOnMissingIsNoOp(t *testing.T) {
	withTempBackup(t)
	// File definitely does not exist yet.
	if err := Clear(); err != nil {
		t.Fatalf("Clear on missing should succeed, got: %v", err)
	}
	got, err := Tail(0)
	if err != nil || len(got) != 0 {
		t.Fatalf("Tail after Clear-on-missing should be empty; got %v err=%v", got, err)
	}
}
