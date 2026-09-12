// Package supportnotice throttles cux's support line: nothing for the first
// week after install, then at most once every 30 days.
package supportnotice

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/inulute/cux/internal/atomicfile"
	"github.com/inulute/cux/internal/paths"
)

const URL = "https://support.inulute.com"

const (
	FirstDelay = 7 * 24 * time.Hour
	Interval   = 30 * 24 * time.Hour

	fileName = "support-notice.json"
)

type state struct {
	FirstSeen time.Time `json:"first_seen"`
	LastShown time.Time `json:"last_shown"`
}

func path() string { return filepath.Join(paths.RuntimeDir(), fileName) }

// Due reports whether a notice is due now, and records it if so.
//
// The first call only starts the clock and returns false — without a stored
// first-seen there is no way to tell a week-old install from a new one.
// Every failure path returns false: a notice that cannot record itself would
// repeat on the next exit.
func Due(now time.Time) bool {
	p := path()
	s, err := load(p)
	if err != nil {
		return false
	}

	if s.FirstSeen.IsZero() {
		s.FirstSeen = now
		_ = save(p, s)
		return false
	}

	wait, since := Interval, s.LastShown
	if since.IsZero() {
		wait, since = FirstDelay, s.FirstSeen
	}
	if now.Sub(since) < wait {
		return false
	}

	s.LastShown = now
	if err := save(p, s); err != nil {
		return false
	}
	return true
}

// Line is the notice. One line, no colour: it prints under the
// `cux --resume <id>` line, which is the one the user is there to read.
func Line() string {
	return "If cux is useful to you, consider supporting it: " + URL
}

func load(p string) (state, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return state{}, nil
		}
		return state{}, err
	}
	var s state
	if err := json.Unmarshal(b, &s); err != nil {
		return state{}, nil // corrupt reads as unseen, which restarts the clock
	}
	return s, nil
}

func save(p string, s state) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return atomicfile.Write(p, b, 0o600)
}
