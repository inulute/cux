package registry

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestUpdateListRemoveLifecycle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	UpdateSelf(func(e *Entry) {
		e.State = StateRunning
		e.Seat = "a@x.test"
	})
	UpdateSelf(func(e *Entry) {
		e.State = StateWaitingReset
		e.Detail = "resets in 1h"
		e.SessionID = "sid-123"
	})

	entries := List()
	if len(entries) != 1 {
		t.Fatalf("List() returned %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.PID != os.Getpid() || e.State != StateWaitingReset || e.Seat != "a@x.test" || e.SessionID != "sid-123" {
		t.Errorf("entry = %+v — fields not preserved across updates", e)
	}
	if e.StartedAt.IsZero() || e.UpdatedAt.Before(e.StartedAt) {
		t.Errorf("timestamps look wrong: started=%v updated=%v", e.StartedAt, e.UpdatedAt)
	}

	RemoveSelf()
	if got := List(); len(got) != 0 {
		t.Errorf("after RemoveSelf List() = %v, want empty", got)
	}
}

func TestListReapsDeadProcesses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	// A real process that has already exited: its PID is dead by the
	// time List runs, so the entry must be reaped, not returned.
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadPID := cmd.Process.Pid
	_ = cmd.Wait()

	UpdateSelf(func(e *Entry) { e.State = StateRunning })
	// Forge the dead process's entry by rewriting our own file's PID.
	entries := List()
	if len(entries) != 1 {
		t.Fatalf("setup: %d entries", len(entries))
	}
	self := entries[0]
	self.PID = deadPID
	self.UpdatedAt = time.Now().UTC()
	b, _ := os.ReadFile(file(os.Getpid()))
	_ = b
	if err := os.Rename(file(os.Getpid()), file(deadPID)); err != nil {
		t.Fatal(err)
	}
	// Rewrite content so the stored PID matches the dead one.
	UpdateSelf(func(e *Entry) { e.State = StateRunning })
	forge := file(deadPID)
	data := []byte(`{"pid":` + itoa(deadPID) + `,"cwd":"/x","state":"running","startedAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`)
	if err := os.WriteFile(forge, data, 0o600); err != nil {
		t.Fatal(err)
	}

	got := List()
	if len(got) != 1 || got[0].PID != os.Getpid() {
		t.Errorf("List() = %+v, want only the live self entry", got)
	}
	if _, err := os.Stat(forge); !os.IsNotExist(err) {
		t.Error("dead entry file should have been reaped")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}
