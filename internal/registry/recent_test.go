package registry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func isolateRuntime(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("XDG_DATA_HOME", tmp)
}

// TestRecordRecentKeepsEveryExitingSession is the reason these are
// per-PID files. Issue #48 is fourteen wrappers exiting inside two
// minutes; a single shared JSON file would have had each atomic write
// replace the last, losing exactly the records that were the whole point.
func TestRecordRecentKeepsEveryExitingSession(t *testing.T) {
	isolateRuntime(t)
	for pid := 100; pid < 114; pid++ {
		RecordRecent(Recent{
			PID:       pid,
			SessionID: "sess-" + strconv.Itoa(pid),
			CWD:       "/work",
			Seat:      "a@x.test",
		})
	}
	got := RecentSessions()
	if len(got) != 14 {
		t.Fatalf("kept %d of 14 ended sessions", len(got))
	}
}

// TestRecentSessionsOrdersNewestFirst — the session a user wants back is
// almost always the one that just died.
func TestRecentSessionsOrdersNewestFirst(t *testing.T) {
	isolateRuntime(t)
	now := time.Now().UTC()
	RecordRecent(Recent{PID: 1, SessionID: "older", CWD: "/w", EndedAt: now.Add(-time.Hour)})
	RecordRecent(Recent{PID: 2, SessionID: "newer", CWD: "/w", EndedAt: now})

	got := RecentSessions()
	if len(got) != 2 || got[0].SessionID != "newer" {
		t.Fatalf("order = %+v, want newest first", got)
	}
}

// TestRecentSessionsPrunesStaleRecords keeps the directory from becoming
// the pile of dead files issue #39 found in the live registry.
func TestRecentSessionsPrunesStaleRecords(t *testing.T) {
	isolateRuntime(t)
	RecordRecent(Recent{PID: 1, SessionID: "ancient", CWD: "/w", EndedAt: time.Now().Add(-recentTTL - time.Hour)})
	RecordRecent(Recent{PID: 2, SessionID: "current", CWD: "/w"})

	got := RecentSessions()
	if len(got) != 1 || got[0].SessionID != "current" {
		t.Fatalf("got %+v, want only the record inside the TTL", got)
	}
	// Pruning is on disk, not just in the returned slice.
	if again := RecentSessions(); len(again) != 1 {
		t.Fatalf("re-read returned %d records; the stale file was not removed", len(again))
	}
}

// TestRecordRecentIgnoresASessionlessExit — a wrapper that never saw a
// session has nothing to offer a user, and an entry with no ID would just
// be noise in `cux sessions`.
func TestRecordRecentIgnoresASessionlessExit(t *testing.T) {
	isolateRuntime(t)
	RecordRecent(Recent{PID: 7, CWD: "/w"})
	if got := RecentSessions(); len(got) != 0 {
		t.Fatalf("stored %+v for a session that never started", got)
	}
}

// TestPruneDeadKeepsTheSessionItSweeps: a wrapper that was killed outright
// never ran its own exit path, so the heartbeat file is the last copy of
// its session ID. Deleting it as crash debris used to delete that too —
// which is how issue #48 turned into an archaeology exercise over
// ~/.claude/projects.
func TestPruneDeadKeepsTheSessionItSweeps(t *testing.T) {
	isolateRuntime(t)
	dead := spawnDead(t)
	if err := os.MkdirAll(dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(Entry{
		PID:       dead,
		CWD:       "/work/thing",
		SessionID: "sess-killed",
		Seat:      "a@x.test",
		UpdatedAt: time.Now().UTC(),
	})
	if err := os.WriteFile(filepath.Join(dir(), strconv.Itoa(dead)+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	PruneDead()

	if _, err := os.Stat(filepath.Join(dir(), strconv.Itoa(dead)+".json")); !os.IsNotExist(err) {
		t.Error("the dead wrapper's heartbeat file should still be swept")
	}
	got := RecentSessions()
	if len(got) != 1 || got[0].SessionID != "sess-killed" {
		t.Fatalf("recent = %+v, want the killed wrapper's session preserved", got)
	}
	if got[0].CWD != "/work/thing" || got[0].Seat != "a@x.test" {
		t.Errorf("recent = %+v, want cwd and seat carried over so `cux sessions` can name it", got[0])
	}
}

// TestListKeepsTheSessionItSweeps — List prunes as a side effect too, and
// on a machine that never runs `cux` again it is the only sweep that fires.
func TestListKeepsTheSessionItSweeps(t *testing.T) {
	isolateRuntime(t)
	dead := spawnDead(t)
	if err := os.MkdirAll(dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(Entry{PID: dead, CWD: "/w", SessionID: "sess-gone", UpdatedAt: time.Now().UTC()})
	if err := os.WriteFile(filepath.Join(dir(), strconv.Itoa(dead)+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	if live := List(); len(live) != 0 {
		t.Fatalf("List returned %d live entries for a dead pid", len(live))
	}
	if got := RecentSessions(); len(got) != 1 || got[0].SessionID != "sess-gone" {
		t.Fatalf("recent = %+v, want the swept session kept", got)
	}
}
