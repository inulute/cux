// Package registry is the wrapper's heartbeat: every running cux
// wrapper self-reports what it is doing into one small JSON file under
// the runtime directory. Until now N concurrent sessions were invisible
// — `cux list` shows seats, nothing shows sessions. The registry makes
// "what is running on this machine, on which seat, in which state"
// answerable, for `cux sessions` today and for remote monitoring
// surfaces later.
//
// One file per wrapper PID, single writer (the wrapper itself), atomic
// writes — no locking needed. Liveness is decided by the reader: an
// entry whose PID is gone is a leftover from a crash and is cleaned up
// on the next List.
package registry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/inulute/cux/internal/atomicfile"
	"github.com/inulute/cux/internal/paths"
)

// States a wrapper can report.
const (
	StateRunning      = "running"
	StateSwapping     = "swapping"
	StateRetrying     = "retrying"
	StateWaitingReset = "waiting-reset"
)

// Entry is one running wrapper's self-reported status.
type Entry struct {
	PID        int       `json:"pid"`
	CWD        string    `json:"cwd"`
	SessionID  string    `json:"sessionId,omitempty"`
	Seat       string    `json:"seat,omitempty"` // email claude was last launched on
	State      string    `json:"state"`
	Attachable bool      `json:"attachable,omitempty"` // wrapper serves an attach socket
	Detail     string    `json:"detail,omitempty"`     // human hint: "reset in 1h23m", "attempt 3"
	StartedAt  time.Time `json:"startedAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

func dir() string { return filepath.Join(paths.RuntimeDir(), "sessions") }

func file(pid int) string { return filepath.Join(dir(), strconv.Itoa(pid)+".json") }

// UpdateSelf mutates (or creates) the calling process's entry. Errors
// are swallowed on purpose: the heartbeat must never break a swap.
func UpdateSelf(mutate func(*Entry)) {
	pid := os.Getpid()
	e := Entry{PID: pid, StartedAt: time.Now().UTC()}
	if b, err := os.ReadFile(file(pid)); err == nil {
		_ = json.Unmarshal(b, &e)
	}
	if e.CWD == "" {
		if cwd, err := os.Getwd(); err == nil {
			e.CWD = cwd
		}
	}
	mutate(&e)
	e.PID = pid
	e.UpdatedAt = time.Now().UTC()
	if err := os.MkdirAll(dir(), 0o700); err != nil {
		return
	}
	if b, err := json.MarshalIndent(e, "", "  "); err == nil {
		_ = atomicfile.Write(file(pid), b, 0o600)
	}
}

// RemoveSelf deletes the calling process's entry (normal exit).
func RemoveSelf() {
	_ = os.Remove(file(os.Getpid()))
}

// List returns every live wrapper's entry, oldest first. Entries whose
// process is gone are removed best-effort and not returned.
func List() []Entry {
	entries, err := os.ReadDir(dir())
	if err != nil {
		return nil
	}
	var out []Entry
	for _, de := range entries {
		if de.IsDir() || filepath.Ext(de.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir(), de.Name()))
		if err != nil {
			continue
		}
		var e Entry
		if json.Unmarshal(b, &e) != nil || e.PID == 0 {
			continue
		}
		if !processAlive(e.PID) {
			_ = os.Remove(filepath.Join(dir(), de.Name()))
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

// PruneDead removes session entries whose process is gone. List already
// drops them as a side effect, but only when something calls it — on a
// machine that never runs `cux sessions`, dead files pile up (issue #39:
// 22 of 34 entries were for processes that no longer existed, some days
// old). Called at wrapper startup, mirroring ReapStaleAttachSockets, so
// the sessions directory is self-healing. The caller's own entry is
// skipped: UpdateSelf may not have written it yet, and it is live anyway.
func PruneDead() {
	entries, err := os.ReadDir(dir())
	if err != nil {
		return
	}
	self := os.Getpid()
	for _, de := range entries {
		name := de.Name()
		if de.IsDir() || filepath.Ext(name) != ".json" {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSuffix(name, ".json"))
		if err != nil || pid <= 0 || pid == self {
			continue
		}
		if processAlive(pid) {
			continue
		}
		_ = os.Remove(filepath.Join(dir(), name))
	}
}

// ReapStaleAttachSockets removes attach sockets left behind by wrappers
// that crashed or were killed: the listener died with its process, so a
// socket whose PID (its filename) is gone can never accept again — but
// while the file exists, attach surfaces (`cux attach`, cuxdeck bridges)
// keep probing a dead endpoint. Called at wrapper startup so the attach
// directory is self-healing, the same way List is for session entries.
// A wrapper's live socket is untouched: its PID probe says alive.
func ReapStaleAttachSockets() {
	entries, err := os.ReadDir(paths.AttachDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".sock" {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSuffix(name, ".sock"))
		if err != nil || pid <= 0 {
			continue
		}
		if pid == os.Getpid() || processAlive(pid) {
			continue
		}
		_ = os.Remove(filepath.Join(paths.AttachDir(), name))
	}
}
