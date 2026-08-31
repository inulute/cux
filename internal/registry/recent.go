package registry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/inulute/cux/internal/atomicfile"
	"github.com/inulute/cux/internal/paths"
)

const (
	// recentTTL is generous on purpose: the sessions this exists for are
	// the long-lived ones. Issue #48 lost conversations that had been open
	// for over a week.
	recentTTL = 30 * 24 * time.Hour
	recentMax = 50
)

// Recent is a session that has ended. Claude Code prints its session ID
// nowhere but its own running UI, so when a wrapped session ends without a
// clean quit the ID leaves with the scrollback — and recovering the
// conversation means sorting ~/.claude/projects/*/*.jsonl by mtime and
// reading each one to work out which was which (issue #48). One record per
// ended session turns that back into `cux sessions`.
type Recent struct {
	PID       int       `json:"pid"`
	SessionID string    `json:"sessionId"`
	CWD       string    `json:"cwd"`
	Seat      string    `json:"seat,omitempty"`
	EndedAt   time.Time `json:"endedAt"`
}

func recentDir() string         { return filepath.Join(paths.RuntimeDir(), "recent") }
func recentFile(pid int) string { return filepath.Join(recentDir(), strconv.Itoa(pid)+".json") }

// RecordRecent stores how this wrapper's session ended.
//
// One file per PID, like the live registry. A single shared file would be
// the obvious shape and the wrong one: an atomic write is a replace, not an
// append, so a dozen wrappers exiting inside the same minute — exactly the
// case this exists for — would overwrite each other's records.
func RecordRecent(r Recent) {
	if r.SessionID == "" {
		return
	}
	if r.PID == 0 {
		r.PID = os.Getpid()
	}
	if r.EndedAt.IsZero() {
		r.EndedAt = time.Now().UTC()
	}
	if err := os.MkdirAll(recentDir(), 0o700); err != nil {
		return
	}
	if b, err := json.MarshalIndent(r, "", "  "); err == nil {
		_ = atomicfile.Write(recentFile(r.PID), b, 0o600)
	}
}

// RecentSessions returns ended sessions, newest first, dropping anything
// past recentTTL or beyond recentMax from disk as it goes. Reading is the
// only moment anyone cares, so it is also the moment to prune.
func RecentSessions() []Recent {
	entries, err := os.ReadDir(recentDir())
	if err != nil {
		return nil
	}
	cutoff := time.Now().Add(-recentTTL)
	var out []Recent
	for _, de := range entries {
		if de.IsDir() || filepath.Ext(de.Name()) != ".json" {
			continue
		}
		path := filepath.Join(recentDir(), de.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var r Recent
		if json.Unmarshal(b, &r) != nil || r.SessionID == "" {
			_ = os.Remove(path)
			continue
		}
		if r.EndedAt.Before(cutoff) {
			_ = os.Remove(path)
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EndedAt.After(out[j].EndedAt) })
	for i := recentMax; i < len(out); i++ {
		_ = os.Remove(recentFile(out[i].PID))
	}
	if len(out) > recentMax {
		out = out[:recentMax]
	}
	return out
}
